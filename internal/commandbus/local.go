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
		// 本地 runtime 与控制面在同一进程内，直接使用指令的目标 worker 作为事件来源。
		event.WorkerID = command.WorkerID
		_ = b.events.HandleEvent(executionContext, event)
	})
	return nil
}
