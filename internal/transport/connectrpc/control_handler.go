package connectrpc

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	demov1 "example.com/multitopology-demo/gen/go/demo/v1"
	"example.com/multitopology-demo/gen/go/demo/v1/demov1connect"
	"example.com/multitopology-demo/internal/control"
	"example.com/multitopology-demo/internal/domain"
)

type controlHandler struct {
	service *control.Service
}

func NewControlHandler(service *control.Service) (string, http.Handler) {
	return demov1connect.NewControlServiceHandler(&controlHandler{service: service})
}

func (h *controlHandler) ListWorkers(
	ctx context.Context,
	_ *connect.Request[demov1.ListWorkersRequest],
) (*connect.Response[demov1.ListWorkersResponse], error) {
	workers, err := h.service.ListWorkers(ctx)
	if err != nil {
		return nil, connectError(err)
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
		return nil, connectError(err)
	}
	return connect.NewResponse(&demov1.CreateCommandResponse{Command: commandToProto(command)}), nil
}

func (h *controlHandler) GetCommand(
	ctx context.Context,
	request *connect.Request[demov1.GetCommandRequest],
) (*connect.Response[demov1.GetCommandResponse], error) {
	command, err := h.service.GetCommand(ctx, request.Msg.GetId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&demov1.GetCommandResponse{Command: commandToProto(command)}), nil
}

func connectError(err error) error {
	switch {
	case errors.Is(err, control.ErrInvalidInput):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, domain.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, domain.ErrStateConflict):
		return connect.NewError(connect.CodeAborted, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
