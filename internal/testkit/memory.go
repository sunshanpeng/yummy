package testkit

import (
	"context"
	"fmt"
	"sync"

	"example.com/multitopology-demo/internal/domain"
)

type MemoryCommandRepository struct {
	mu          sync.Mutex
	commands    map[string]domain.Command
	Transitions []string
}

func NewMemoryCommandRepository() *MemoryCommandRepository {
	return &MemoryCommandRepository{
		commands: make(map[string]domain.Command),
	}
}

func (r *MemoryCommandRepository) Create(_ context.Context, command domain.Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands[command.ID] = command
	return nil
}

func (r *MemoryCommandRepository) Get(_ context.Context, id string) (domain.Command, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	command, ok := r.commands[id]
	if !ok {
		return domain.Command{}, domain.ErrNotFound
	}
	return command, nil
}

func (r *MemoryCommandRepository) Transition(
	_ context.Context,
	id string,
	from domain.CommandStatus,
	to domain.CommandStatus,
	output string,
	errorText string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	command, ok := r.commands[id]
	if !ok {
		return domain.ErrNotFound
	}
	if command.Status != from {
		return domain.ErrStateConflict
	}
	command.Status = to
	command.Output = output
	command.Error = errorText
	r.commands[id] = command
	r.Transitions = append(r.Transitions, fmt.Sprintf("%s->%s", from, to))
	return nil
}

type MemoryPresence struct {
	mu     sync.Mutex
	online map[string]domain.WorkerPresence
}

func NewMemoryPresence(workerIDs ...string) *MemoryPresence {
	store := &MemoryPresence{
		online: make(map[string]domain.WorkerPresence),
	}
	for _, workerID := range workerIDs {
		store.online[workerID] = domain.WorkerPresence{WorkerID: workerID}
	}
	return store
}

func (s *MemoryPresence) Upsert(_ context.Context, presence domain.WorkerPresence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.online[presence.WorkerID] = presence
	return nil
}

func (s *MemoryPresence) IsOnline(_ context.Context, workerID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.online[workerID]
	return ok, nil
}

func (s *MemoryPresence) ListOnline(_ context.Context) ([]domain.WorkerPresence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	workers := make([]domain.WorkerPresence, 0, len(s.online))
	for _, worker := range s.online {
		workers = append(workers, worker)
	}
	return workers, nil
}

type RecordingBus struct {
	Commands       []domain.Command
	Err            error
	BeforeDispatch func(domain.Command)
}

func (b *RecordingBus) Dispatch(_ context.Context, command domain.Command) error {
	if b.BeforeDispatch != nil {
		b.BeforeDispatch(command)
	}
	b.Commands = append(b.Commands, command)
	return b.Err
}
