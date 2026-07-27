package connectrpc

import (
	"context"
	"errors"
	"testing"
	"time"

	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/internal/domain"
)

func TestRegistryDispatchesCommandToRegisteredWorker(t *testing.T) {
	registry := NewRegistry(time.Second)
	messages := make(chan *demov1.ControlMessage, 1)
	unregister := registry.Register("worker-1", messages)
	defer unregister()

	command := domain.NewCommand("command-1", "worker-1", "hello")
	if err := registry.Dispatch(context.Background(), command); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	message := <-messages
	if message.GetCommand().GetId() != "command-1" {
		t.Fatalf("command id = %q, want %q", message.GetCommand().GetId(), "command-1")
	}
	if message.GetCommand().GetInput() != "hello" {
		t.Fatalf("command input = %q, want %q", message.GetCommand().GetInput(), "hello")
	}
}

func TestRegistryRejectsUnavailableWorker(t *testing.T) {
	registry := NewRegistry(time.Second)

	err := registry.Dispatch(context.Background(), domain.NewCommand("command-1", "missing", "hello"))

	if !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatalf("Dispatch() error = %v, want ErrWorkerUnavailable", err)
	}
}

func TestRegistryStaleUnregisterDoesNotRemoveReplacement(t *testing.T) {
	registry := NewRegistry(time.Second)
	oldMessages := make(chan *demov1.ControlMessage, 1)
	unregisterOld := registry.Register("worker-1", oldMessages)
	newMessages := make(chan *demov1.ControlMessage, 1)
	unregisterNew := registry.Register("worker-1", newMessages)
	defer unregisterNew()

	unregisterOld()
	if err := registry.Dispatch(context.Background(), domain.NewCommand("command-1", "worker-1", "hello")); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	select {
	case <-newMessages:
	case <-time.After(time.Second):
		t.Fatal("新连接没有收到指令")
	}
	select {
	case <-oldMessages:
		t.Fatal("旧连接不应收到指令")
	default:
	}
}
