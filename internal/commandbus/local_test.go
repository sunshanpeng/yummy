package commandbus

import (
	"context"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
	"github.com/sunshanpeng/yummy/internal/worker"
)

func TestLocalBusProducesUppercaseResult(t *testing.T) {
	events := make(chan domain.Event, 3)
	bus := NewLocal(
		worker.NewRuntime(),
		ports.EventHandlerFunc(func(_ context.Context, event domain.Event) error {
			events <- event
			return nil
		}),
	)

	err := bus.Dispatch(context.Background(), domain.Command{
		ID:       "cmd-1",
		WorkerID: "local",
		Input:    "hello",
	})
	if err != nil {
		t.Fatal(err)
	}

	var result domain.Event
	for range 3 {
		select {
		case event := <-events:
			result = event
		case <-time.After(time.Second):
			t.Fatal("等待本地 worker 事件超时")
		}
	}
	if result.Type != domain.EventSucceeded || result.Output != "HELLO" {
		t.Fatalf("结果事件 = %#v", result)
	}
}
