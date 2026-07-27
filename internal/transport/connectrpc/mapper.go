package connectrpc

import (
	"time"

	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/internal/domain"
)

func commandToProto(command domain.Command) *demov1.Command {
	return &demov1.Command{
		Id:        command.ID,
		WorkerId:  command.WorkerID,
		Input:     command.Input,
		Output:    command.Output,
		Error:     command.Error,
		Status:    statusToProto(command.Status),
		CreatedAt: formatTime(command.CreatedAt),
		UpdatedAt: formatTime(command.UpdatedAt),
	}
}

func statusToProto(status domain.CommandStatus) demov1.CommandStatus {
	switch status {
	case domain.StatusCreated:
		return demov1.CommandStatus_COMMAND_STATUS_CREATED
	case domain.StatusDispatched:
		return demov1.CommandStatus_COMMAND_STATUS_DISPATCHED
	case domain.StatusRunning:
		return demov1.CommandStatus_COMMAND_STATUS_RUNNING
	case domain.StatusSucceeded:
		return demov1.CommandStatus_COMMAND_STATUS_SUCCEEDED
	case domain.StatusFailed:
		return demov1.CommandStatus_COMMAND_STATUS_FAILED
	case domain.StatusDispatchFailed:
		return demov1.CommandStatus_COMMAND_STATUS_DISPATCH_FAILED
	default:
		return demov1.CommandStatus_COMMAND_STATUS_UNSPECIFIED
	}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
