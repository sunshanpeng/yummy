package connectrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/gen/go/demo/v1/demov1connect"
	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/worker"
)

type WorkerClientConfig struct {
	HTTPClient        connect.HTTPClient
	ControlURL        string
	WorkerID          string
	Version           string
	HeartbeatInterval time.Duration
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
}

type WorkerClient struct {
	config  WorkerClientConfig
	runtime *worker.Runtime
}

func NewWorkerClient(config WorkerClientConfig, runtime *worker.Runtime) *WorkerClient {
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 10 * time.Second
	}
	if config.ReconnectMinDelay <= 0 {
		config.ReconnectMinDelay = time.Second
	}
	if config.ReconnectMaxDelay <= 0 {
		config.ReconnectMaxDelay = 30 * time.Second
	}
	return &WorkerClient{config: config, runtime: runtime}
}

func (c *WorkerClient) Run(ctx context.Context) error {
	if c.config.HTTPClient == nil {
		return errors.New("HTTPClient 不能为空")
	}
	if c.config.ControlURL == "" || c.config.WorkerID == "" {
		return errors.New("ControlURL 和 WorkerID 不能为空")
	}
	if c.runtime == nil {
		return errors.New("worker runtime 不能为空")
	}

	delay := c.config.ReconnectMinDelay
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		// 记录断线原因，避免证书身份不符等配置错误表现为没有任何输出的重连循环。
		slog.WarnContext(ctx, "与控制面的连接已断开，稍后重连", "error", err, "retry_in", delay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay *= 2
		if delay > c.config.ReconnectMaxDelay {
			delay = c.config.ReconnectMaxDelay
		}
	}
}

func (c *WorkerClient) runOnce(ctx context.Context) error {
	connectionContext, cancel := context.WithCancel(ctx)

	client := demov1connect.NewAgentServiceClient(
		c.config.HTTPClient,
		c.config.ControlURL,
		connect.WithGRPC(),
	)
	stream := client.Connect(connectionContext)

	outbound := make(chan *demov1.AgentMessage, 32)
	sendErrors := make(chan error, 1)
	writerDone := make(chan struct{})
	defer func() {
		cancel()
		<-writerDone
		_ = stream.CloseResponse()
	}()
	outbound <- &demov1.AgentMessage{
		Payload: &demov1.AgentMessage_Hello{
			Hello: &demov1.Hello{
				WorkerId: c.config.WorkerID,
				Version:  c.config.Version,
			},
		},
	}

	go func() {
		defer close(writerDone)
		for {
			select {
			case <-connectionContext.Done():
				_ = stream.CloseRequest()
				return
			case message := <-outbound:
				if err := stream.Send(message); err != nil {
					sendErrors <- err
					_ = stream.CloseRequest()
					return
				}
			}
		}
	}()

	go c.sendHeartbeats(connectionContext, outbound)

	for {
		message, err := stream.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 优先返回接收错误：它带有控制面返回的错误码和原因（例如证书身份不符），
			// 而连接关闭后的发送错误通常只是 EOF。
			if !errors.Is(err, io.EOF) {
				return err
			}
			select {
			case sendErr := <-sendErrors:
				return sendErr
			default:
			}
			return io.EOF
		}
		if welcome := message.GetWelcome(); welcome != nil {
			if welcome.GetWorkerId() != c.config.WorkerID {
				return fmt.Errorf("控制面确认了意外的 worker ID %q", welcome.GetWorkerId())
			}
			continue
		}
		command := message.GetCommand()
		if command == nil {
			return errors.New("控制面发送了未知消息")
		}
		c.runtime.Execute(connectionContext, commandFromProto(command), func(event domain.Event) {
			select {
			case outbound <- eventToAgentMessage(event):
			case <-connectionContext.Done():
			}
		})
	}
}

func (c *WorkerClient) sendHeartbeats(
	ctx context.Context,
	outbound chan<- *demov1.AgentMessage,
) {
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			message := &demov1.AgentMessage{
				Payload: &demov1.AgentMessage_Heartbeat{
					Heartbeat: &demov1.Heartbeat{UnixMillis: now.UnixMilli()},
				},
			}
			select {
			case outbound <- message:
			case <-ctx.Done():
				return
			}
		}
	}
}

func commandFromProto(command *demov1.Command) domain.Command {
	return domain.Command{
		ID:       command.GetId(),
		WorkerID: command.GetWorkerId(),
		Input:    command.GetInput(),
	}
}

func eventToAgentMessage(event domain.Event) *demov1.AgentMessage {
	switch event.Type {
	case domain.EventAccepted:
		return &demov1.AgentMessage{Payload: &demov1.AgentMessage_Accepted{
			Accepted: &demov1.CommandAccepted{CommandId: event.CommandID},
		}}
	case domain.EventProgress:
		return &demov1.AgentMessage{Payload: &demov1.AgentMessage_Progress{
			Progress: &demov1.CommandProgress{
				CommandId: event.CommandID,
				Message:   event.Message,
			},
		}}
	default:
		return &demov1.AgentMessage{Payload: &demov1.AgentMessage_Result{
			Result: &demov1.CommandResult{
				CommandId: event.CommandID,
				Output:    event.Output,
				Error:     event.Error,
			},
		}}
	}
}
