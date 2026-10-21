package connectrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/gen/go/demo/v1/demov1connect"
	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
)

const (
	// maxAgentMessageBytes 限制 worker 单条消息的大小。
	maxAgentMessageBytes = 1 << 20
	// maxLoggedValueBytes 限制写入日志的 worker 输入长度。
	maxLoggedValueBytes = 128
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
	return demov1connect.NewAgentServiceHandler(
		&agentHandler{
			registry:   registry,
			presence:   presence,
			events:     events,
			instanceID: instanceID,
			now:        time.Now,
		},
		connect.WithReadMaxBytes(maxAgentMessageBytes),
	)
}

func (h *agentHandler) Connect(
	ctx context.Context,
	stream *connect.BidiStream[demov1.AgentMessage, demov1.ControlMessage],
) error {
	identity, ok := PeerIdentity(ctx)
	if !ok {
		return rejectAgent(ctx, connect.CodeUnauthenticated, "缺少 mTLS 客户端身份")
	}
	if !domain.ValidWorkerID(identity) {
		return rejectAgent(
			ctx,
			connect.CodeUnauthenticated,
			"客户端证书身份格式无效",
			"identity", truncateForLog(identity),
		)
	}

	first, err := stream.Receive()
	if err != nil {
		return receiveError(err)
	}
	hello := first.GetHello()
	if hello == nil {
		return rejectAgent(ctx, connect.CodeInvalidArgument, "首条消息必须是 Hello", "identity", identity)
	}
	if hello.GetWorkerId() == "" || hello.GetWorkerId() != identity {
		return rejectAgent(
			ctx,
			connect.CodeUnauthenticated,
			"worker ID 与客户端证书身份不一致",
			"identity", identity,
			"hello_worker_id", truncateForLog(hello.GetWorkerId()),
		)
	}
	if len(hello.GetVersion()) > domain.MaxWorkerVersionLength {
		return rejectAgent(
			ctx,
			connect.CodeInvalidArgument,
			fmt.Sprintf("worker 版本号不能超过 %d 字节", domain.MaxWorkerVersionLength),
			"identity", identity,
		)
	}

	presence := domain.WorkerPresence{
		WorkerID:   identity,
		InstanceID: h.instanceID,
		Version:    hello.GetVersion(),
		LastSeen:   h.now().UTC(),
	}
	if err := h.presence.Upsert(ctx, presence); err != nil {
		return internalAgentError(ctx, "记录 worker 在线状态失败", err, "worker_id", identity)
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
			return internalAgentError(ctx, "更新 worker 心跳失败", err, "worker_id", presence.WorkerID)
		}
		return nil
	case message.GetAccepted() != nil:
		return h.handleEvent(ctx, presence.WorkerID, domain.Event{
			CommandID: message.GetAccepted().GetCommandId(),
			Type:      domain.EventAccepted,
		})
	case message.GetProgress() != nil:
		return h.handleEvent(ctx, presence.WorkerID, domain.Event{
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
		return h.handleEvent(ctx, presence.WorkerID, event)
	default:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("未知的 agent 消息"))
	}
}

// handleEvent 用 mTLS 认证得到的身份标记事件来源，供控制面校验指令归属。
func (h *agentHandler) handleEvent(ctx context.Context, workerID string, event domain.Event) error {
	event.WorkerID = workerID
	err := h.events.HandleEvent(ctx, event)
	if err == nil {
		return nil
	}
	attrs := []any{
		"worker_id", workerID,
		"command_id", truncateForLog(event.CommandID),
		"event", event.Type,
	}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// 指令不存在和不属于该 worker 返回相同结果，不暴露其他 worker 的指令是否存在。
		slog.WarnContext(ctx, "拒绝 worker 事件：指令不存在或不属于该 worker", attrs...)
		return connect.NewError(connect.CodeNotFound, errors.New("指令不存在或不属于当前 worker"))
	case errors.Is(err, domain.ErrStateConflict):
		slog.WarnContext(ctx, "worker 事件与指令状态冲突", attrs...)
		return connect.NewError(connect.CodeAborted, domain.ErrStateConflict)
	default:
		return internalAgentError(ctx, "处理 worker 事件失败", err, attrs...)
	}
}

// rejectAgent 记录并拒绝不合规的 worker 连接，便于排查证书或配置问题。
func rejectAgent(ctx context.Context, code connect.Code, reason string, attrs ...any) error {
	slog.WarnContext(ctx, "拒绝 worker 连接", append([]any{"reason", reason}, attrs...)...)
	return connect.NewError(code, errors.New(reason))
}

// internalAgentError 只在控制面日志里记录内部错误详情，返回给 worker 的信息不包含基础设施细节。
func internalAgentError(ctx context.Context, message string, err error, attrs ...any) error {
	slog.ErrorContext(ctx, message, append([]any{"error", err}, attrs...)...)
	return connect.NewError(connect.CodeInternal, errors.New(message))
}

// truncateForLog 截断来自 worker 的字符串，避免超长输入撑大日志。
func truncateForLog(value string) string {
	if len(value) <= maxLoggedValueBytes {
		return value
	}
	end := maxLoggedValueBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}

func receiveError(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
