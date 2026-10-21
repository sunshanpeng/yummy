package control

import (
	"context"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/testkit"
)

func TestCreateCommandTransitionsBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	repo := testkit.NewMemoryCommandRepository()
	presence := testkit.NewMemoryPresence("worker-1")
	bus := &testkit.RecordingBus{}
	bus.BeforeDispatch = func(command domain.Command) {
		stored, err := repo.Get(ctx, command.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != domain.StatusDispatched {
			t.Fatalf("下发时持久化状态 = %q，期望 %q", stored.Status, domain.StatusDispatched)
		}
	}
	service := NewService(repo, presence, bus, func() string { return "cmd-1" })

	got, err := service.CreateCommand(ctx, "worker-1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusDispatched {
		t.Fatalf("状态 = %q，期望 %q", got.Status, domain.StatusDispatched)
	}
	if len(bus.Commands) != 1 || bus.Commands[0].ID != "cmd-1" {
		t.Fatalf("下发记录 = %#v", bus.Commands)
	}
	if repo.Transitions[0] != "CREATED->DISPATCHED" {
		t.Fatalf("首个转换 = %q", repo.Transitions[0])
	}
}

func TestCreateCommandMarksOfflineWorkerAsDispatchFailed(t *testing.T) {
	repo := testkit.NewMemoryCommandRepository()
	service := NewService(
		repo,
		testkit.NewMemoryPresence(),
		&testkit.RecordingBus{},
		func() string { return "cmd-1" },
	)

	got, err := service.CreateCommand(context.Background(), "worker-1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusDispatchFailed {
		t.Fatalf("状态 = %q，期望 %q", got.Status, domain.StatusDispatchFailed)
	}
	if got.Error == "" {
		t.Fatal("离线下发失败必须保存错误")
	}
}

func TestHandleEventCompletesCommand(t *testing.T) {
	ctx := context.Background()
	repo := testkit.NewMemoryCommandRepository()
	service := NewService(
		repo,
		testkit.NewMemoryPresence("worker-1"),
		&testkit.RecordingBus{},
		func() string { return "cmd-1" },
	)
	if _, err := service.CreateCommand(ctx, "worker-1", "hello"); err != nil {
		t.Fatal(err)
	}

	if err := service.HandleEvent(ctx, domain.Event{
		CommandID: "cmd-1",
		WorkerID:  "worker-1",
		Type:      domain.EventAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleEvent(ctx, domain.Event{
		CommandID: "cmd-1",
		WorkerID:  "worker-1",
		Type:      domain.EventProgress,
		Message:   "正在转换文本",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleEvent(ctx, domain.Event{
		CommandID: "cmd-1",
		WorkerID:  "worker-1",
		Type:      domain.EventSucceeded,
		Output:    "HELLO",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := service.GetCommand(ctx, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusSucceeded || got.Output != "HELLO" {
		t.Fatalf("完成指令 = %#v", got)
	}
}
