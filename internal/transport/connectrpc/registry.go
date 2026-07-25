package connectrpc

import (
	"context"
	"errors"
	"sync"
	"time"

	demov1 "example.com/multitopology-demo/gen/go/demo/v1"
	"example.com/multitopology-demo/internal/domain"
)

var ErrWorkerUnavailable = errors.New("worker 未连接控制面")

type workerConnection struct {
	messages chan<- *demov1.ControlMessage
}

type Registry struct {
	mu          sync.RWMutex
	connections map[string]*workerConnection
	sendTimeout time.Duration
}

func NewRegistry(sendTimeout time.Duration) *Registry {
	return &Registry{
		connections: make(map[string]*workerConnection),
		sendTimeout: sendTimeout,
	}
}

func (r *Registry) Register(workerID string, messages chan<- *demov1.ControlMessage) func() {
	connection := &workerConnection{messages: messages}

	r.mu.Lock()
	r.connections[workerID] = connection
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.connections[workerID] == connection {
			delete(r.connections, workerID)
		}
	}
}

func (r *Registry) Dispatch(ctx context.Context, command domain.Command) error {
	r.mu.RLock()
	connection := r.connections[command.WorkerID]
	r.mu.RUnlock()
	if connection == nil {
		return ErrWorkerUnavailable
	}

	timer := time.NewTimer(r.sendTimeout)
	defer timer.Stop()
	message := &demov1.ControlMessage{
		Payload: &demov1.ControlMessage_Command{Command: commandToProto(command)},
	}
	select {
	case connection.messages <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrWorkerUnavailable
	}
}
