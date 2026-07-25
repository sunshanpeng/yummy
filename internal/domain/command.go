package domain

import (
	"errors"
	"time"
)

type CommandStatus string

const (
	StatusCreated        CommandStatus = "CREATED"
	StatusDispatched     CommandStatus = "DISPATCHED"
	StatusRunning        CommandStatus = "RUNNING"
	StatusSucceeded      CommandStatus = "SUCCEEDED"
	StatusFailed         CommandStatus = "FAILED"
	StatusDispatchFailed CommandStatus = "DISPATCH_FAILED"
)

type Command struct {
	ID        string
	WorkerID  string
	Input     string
	Output    string
	Error     string
	Status    CommandStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

type EventType string

const (
	EventAccepted  EventType = "ACCEPTED"
	EventProgress  EventType = "PROGRESS"
	EventSucceeded EventType = "SUCCEEDED"
	EventFailed    EventType = "FAILED"
)

type Event struct {
	CommandID string
	Type      EventType
	Message   string
	Output    string
	Error     string
}

var (
	ErrNotFound      = errors.New("记录不存在")
	ErrStateConflict = errors.New("指令状态冲突")
)

func CanTransition(from, to CommandStatus) bool {
	allowed := map[CommandStatus]map[CommandStatus]bool{
		StatusCreated: {
			StatusDispatched:     true,
			StatusDispatchFailed: true,
		},
		StatusDispatched: {
			StatusRunning:        true,
			StatusDispatchFailed: true,
		},
		StatusRunning: {
			StatusSucceeded: true,
			StatusFailed:    true,
		},
	}
	return allowed[from][to]
}

func NewCommand(id, workerID, input string) Command {
	now := time.Now().UTC()
	return Command{
		ID:        id,
		WorkerID:  workerID,
		Input:     input,
		Status:    StatusCreated,
		CreatedAt: now,
		UpdatedAt: now,
	}
}
