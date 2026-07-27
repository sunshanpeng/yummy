package connectrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"
	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/gen/go/demo/v1/demov1connect"
	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
)

type agentHandler struct {
	registry   *Registry
	presence   ports.PresenceStore
	events     ports.EventHandler
	instanceID string
	now        func() time.Time
}

func NewAgentHandler(
	registry *Registry,
	presence ports.PresenceStore,
	events ports.EventHandler,
	instanceID string,
) (string, http.Handler) {
	return demov1connect.NewAgentServiceHandler(&agentHandler{
		registry:   registry,
		presence:   presence,
		events:     events,
		instanceID: instanceID,
		now:        time.Now,
	})
}

func (h *agentHandler) Connect(
	ctx context.Context,
	stream *connect.BidiStream[demov1.AgentMessage, demov1.ControlMessage],
) error {
	identity, ok := PeerIdentity(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("缺少 mTLS 客户端身份"))
	}

	first, err := stream.Receive()
	if err != nil {
		return receiveError(err)
	}
	hello := first.GetHello()
	if hello == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("首条消息必须是 Hello"))
	}
	if hello.GetWorkerId() == "" || hello.GetWorkerId() != identity {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("worker ID 与客户端证书身份不一致"))
	}

	presence := domain.WorkerPresence{
		WorkerID:   identity,
		InstanceID: h.instanceID,
		Version:    hello.GetVersion(),
		LastSeen:   h.now().UTC(),
	}
	if err := h.presence.Upsert(ctx, presence); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("记录 worker 在线状态: %w", err))
	}

	outbound := make(chan *demov1.ControlMessage, 32)
	unregister := h.registry.Register(identity, outbound)
	defer unregister()
	if err := stream.Send(&demov1.ControlMessage{
		Payload: &demov1.ControlMessage_Welcome{
			Welcome: &demov1.Welcome{WorkerId: identity},
		},
	}); err != nil {
		return err
	}

	sendErrors := make(chan error, 1)
	writerDone := make(chan struct{})
	connectionContext, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		<-writerDone
	}()
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-connectionContext.Done():
				return
			case message := <-outbound:
				if err := stream.Send(message); err != nil {
					sendErrors <- err
					return
				}
			}
		}
	}()

	for {
		message, err := stream.Receive()
		if err != nil {
			return receiveError(err)
		}
		if err := h.handleAgentMessage(ctx, presence, message); err != nil {
			return err
		}
		select {
		case err := <-sendErrors:
			return err
		default:
		}
	}
}

func (h *agentHandler) handleAgentMessage(
	ctx context.Context,
	presence domain.WorkerPresence,
	message *demov1.AgentMessage,
) error {
	switch {
	case message.GetHeartbeat() != nil:
		presence.LastSeen = h.now().UTC()
		if err := h.presence.Upsert(ctx, presence); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("更新 worker 心跳: %w", err))
		}
		return nil
	case message.GetAccepted() != nil:
		return h.handleEvent(ctx, domain.Event{
			CommandID: message.GetAccepted().GetCommandId(),
			Type:      domain.EventAccepted,
		})
	case message.GetProgress() != nil:
		return h.handleEvent(ctx, domain.Event{
			CommandID: message.GetProgress().GetCommandId(),
			Type:      domain.EventProgress,
			Message:   message.GetProgress().GetMessage(),
		})
	case message.GetResult() != nil:
		result := message.GetResult()
		event := domain.Event{
			CommandID: result.GetCommandId(),
			Type:      domain.EventSucceeded,
			Output:    result.GetOutput(),
		}
		if result.GetError() != "" {
			event.Type = domain.EventFailed
			event.Error = result.GetError()
		}
		return h.handleEvent(ctx, event)
	default:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("未知的 agent 消息"))
	}
}

func (h *agentHandler) handleEvent(ctx context.Context, event domain.Event) error {
	if err := h.events.HandleEvent(ctx, event); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("处理 worker 事件: %w", err))
	}
	return nil
}

func receiveError(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
