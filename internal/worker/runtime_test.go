package worker

import (
	"context"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domain"
)

func TestRuntimeUppercase(t *testing.T) {
	runtime := NewRuntime()
	var events []domain.Event

	runtime.Execute(
		context.Background(),
		domain.Command{
			ID:       "cmd-1",
			WorkerID: "worker-1",
			Input:    "Hello 世界",
		},
		func(event domain.Event) {
			events = append(events, event)
		},
	)

	if len(events) != 3 {
		t.Fatalf("事件数 = %d，期望 3", len(events))
	}
	if events[0].Type != domain.EventAccepted {
		t.Fatalf("首个事件 = %q，期望 %q", events[0].Type, domain.EventAccepted)
	}
	if events[1].Type != domain.EventProgress {
		t.Fatalf("第二个事件 = %q，期望 %q", events[1].Type, domain.EventProgress)
	}
	if events[2].Type != domain.EventSucceeded {
		t.Fatalf("最后事件 = %q，期望 %q", events[2].Type, domain.EventSucceeded)
	}
	if events[2].Output != "HELLO 世界" {
		t.Fatalf("输出 = %q，期望 %q", events[2].Output, "HELLO 世界")
	}
}

func TestRuntimeReportsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runtime := NewRuntime()
	var events []domain.Event
	runtime.Execute(ctx, domain.Command{ID: "cmd-1", Input: "hello"}, func(event domain.Event) {
		events = append(events, event)
	})

	last := events[len(events)-1]
	if last.Type != domain.EventFailed {
		t.Fatalf("最后事件 = %q，期望 %q", last.Type, domain.EventFailed)
	}
	if last.Error == "" {
		t.Fatal("取消执行时必须返回错误")
	}
}
