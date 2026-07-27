package connectrpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
	"github.com/sunshanpeng/yummy/internal/testkit"
	"github.com/sunshanpeng/yummy/internal/worker"
)

func TestWorkerClientExecutesRemoteCommand(t *testing.T) {
	certificates := writeTestCertificates(t)
	registry := NewRegistry(time.Second)
	presence := testkit.NewMemoryPresence()
	events := make(chan domain.Event, 3)
	path, handler := NewAgentHandler(
		registry,
		presence,
		ports.EventHandlerFunc(func(_ context.Context, event domain.Event) error {
			events <- event
			return nil
		}),
		"control-1",
	)
	server, httpClient := newAgentTestServer(t, certificates, path, handler)
	defer server.Close()

	client := NewWorkerClient(WorkerClientConfig{
		HTTPClient:        httpClient,
		ControlURL:        server.URL,
		WorkerID:          "worker-1",
		Version:           "test",
		HeartbeatInterval: 20 * time.Millisecond,
		ReconnectMinDelay: 10 * time.Millisecond,
		ReconnectMaxDelay: 20 * time.Millisecond,
	}, worker.NewRuntime())
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- client.Run(ctx)
	}()

	command := domain.NewCommand("command-1", "worker-1", "hello")
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := registry.Dispatch(context.Background(), command)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrWorkerUnavailable) || time.Now().After(deadline) {
			t.Fatalf("Dispatch() error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	assertEventType(t, events, domain.EventAccepted)
	assertEventType(t, events, domain.EventProgress)
	select {
	case event := <-events:
		if event.Type != domain.EventSucceeded || event.Output != "HELLO" {
			t.Fatalf("result event = %#v, want succeeded HELLO", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到任务结果")
	}

	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WorkerClient 没有在 context 取消后退出")
	}
}
