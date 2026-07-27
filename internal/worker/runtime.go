package worker

import (
	"context"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domain"
)

type Runtime struct{}

func NewRuntime() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Execute(
	ctx context.Context,
	command domain.Command,
	emit func(domain.Event),
) {
	emit(domain.Event{
		CommandID: command.ID,
		Type:      domain.EventAccepted,
	})
	emit(domain.Event{
		CommandID: command.ID,
		Type:      domain.EventProgress,
		Message:   "正在转换文本",
	})

	select {
	case <-ctx.Done():
		emit(domain.Event{
			CommandID: command.ID,
			Type:      domain.EventFailed,
			Error:     ctx.Err().Error(),
		})
	default:
		emit(domain.Event{
			CommandID: command.ID,
			Type:      domain.EventSucceeded,
			Output:    strings.ToUpper(command.Input),
		})
	}
}
