package connectrpc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/gen/go/demo/v1/demov1connect"
	"github.com/sunshanpeng/yummy/internal/control"
	"github.com/sunshanpeng/yummy/internal/domain"
)

// maxControlMessageBytes 限制公共 API 单条请求消息的大小，
// 需大于 control.MaxInputBytes 并为 JSON 编码留出余量。
const maxControlMessageBytes = 128 << 10

var errInternal = errors.New("控制面内部错误")

type controlHandler struct {
	service *control.Service
}

func NewControlHandler(service *control.Service) (string, http.Handler) {
	return demov1connect.NewControlServiceHandler(
		&controlHandler{service: service},
		connect.WithReadMaxBytes(maxControlMessageBytes),
	)
}

func (h *controlHandler) ListWorkers(
	ctx context.Context,
	_ *connect.Request[demov1.ListWorkersRequest],
) (*connect.Response[demov1.ListWorkersResponse], error) {
	workers, err := h.service.ListWorkers(ctx)
	if err != nil {
		return nil, connectError(ctx, err)
	}

	response := &demov1.ListWorkersResponse{
		Workers: make([]*demov1.Worker, 0, len(workers)),
	}
	for _, worker := range workers {
		response.Workers = append(response.Workers, &demov1.Worker{
			Id:       worker.WorkerID,
			Version:  worker.Version,
			LastSeen: formatTime(worker.LastSeen),
		})
	}
	return connect.NewResponse(response), nil
}

func (h *controlHandler) CreateCommand(
	ctx context.Context,
	request *connect.Request[demov1.CreateCommandRequest],
) (*connect.Response[demov1.CreateCommandResponse], error) {
	command, err := h.service.CreateCommand(ctx, request.Msg.GetWorkerId(), request.Msg.GetInput())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&demov1.CreateCommandResponse{Command: commandToProto(command)}), nil
}

func (h *controlHandler) GetCommand(
	ctx context.Context,
	request *connect.Request[demov1.GetCommandRequest],
) (*connect.Response[demov1.GetCommandResponse], error) {
	command, err := h.service.GetCommand(ctx, request.Msg.GetId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&demov1.GetCommandResponse{Command: commandToProto(command)}), nil
}

func connectError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, control.ErrInvalidInput):
		// 校验错误只包含面向用户的说明，可以原样返回。
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, domain.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, domain.ErrNotFound)
	case errors.Is(err, domain.ErrStateConflict):
		return connect.NewError(connect.CodeAborted, domain.ErrStateConflict)
	case errors.Is(err, context.Canceled):
		// 客户端取消或超时不是控制面故障，不按内部错误记录。
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	default:
		// 内部错误可能包含数据库、Redis 地址等基础设施细节，只写日志，不返回给调用方。
		slog.ErrorContext(ctx, "ControlService 内部错误", "error", err)
		return connect.NewError(connect.CodeInternal, errInternal)
	}
}
