package ports

import (
	"context"

	"example.com/multitopology-demo/internal/domain"
)

type CommandRepository interface {
	Create(context.Context, domain.Command) error
	Get(context.Context, string) (domain.Command, error)
	Transition(
		context.Context,
		string,
		domain.CommandStatus,
		domain.CommandStatus,
		string,
		string,
	) error
}

type PresenceStore interface {
	Upsert(context.Context, domain.WorkerPresence) error
	IsOnline(context.Context, string) (bool, error)
	ListOnline(context.Context) ([]domain.WorkerPresence, error)
}

type CommandBus interface {
	Dispatch(context.Context, domain.Command) error
}

type EventHandler interface {
	HandleEvent(context.Context, domain.Event) error
}

type EventHandlerFunc func(context.Context, domain.Event) error

func (f EventHandlerFunc) HandleEvent(ctx context.Context, event domain.Event) error {
	return f(ctx, event)
}
