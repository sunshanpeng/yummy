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
	// WorkerID 是上报事件的 worker 身份。远程模式取自 mTLS 客户端证书，
	// 不能取自 worker 发送的消息内容。
	WorkerID string
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
