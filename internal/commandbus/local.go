package commandbus

import (
	"context"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
	"github.com/sunshanpeng/yummy/internal/worker"
)

type Local struct {
	runtime *worker.Runtime
	events  ports.EventHandler
}

func NewLocal(runtime *worker.Runtime, events ports.EventHandler) *Local {
	return &Local{
		runtime: runtime,
		events:  events,
	}
}

func (b *Local) Dispatch(ctx context.Context, command domain.Command) error {
	executionContext := context.WithoutCancel(ctx)
	go b.runtime.Execute(executionContext, command, func(event domain.Event) {
		_ = b.events.HandleEvent(executionContext, event)
	})
	return nil
}
