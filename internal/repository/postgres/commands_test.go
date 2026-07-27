package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTransitionUsesExpectedCurrentStatus(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	repo := New(pool)
	command := domain.NewCommand("cmd-transition", "worker-1", "hello")

	if err := repo.Create(ctx, command); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM commands WHERE id = $1", command.ID)
	})

	if err := repo.Transition(
		ctx,
		command.ID,
		domain.StatusCreated,
		domain.StatusDispatched,
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}

	err := repo.Transition(
		ctx,
		command.ID,
		domain.StatusCreated,
		domain.StatusDispatchFailed,
		"",
		"重复转换",
	)
	if !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("重复转换错误 = %v，期望 %v", err, domain.ErrStateConflict)
	}
}

func TestGetReturnsStoredResult(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	repo := New(pool)
	command := domain.NewCommand("cmd-result", "worker-1", "hello")

	if err := repo.Create(ctx, command); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM commands WHERE id = $1", command.ID)
	})
	if err := repo.Transition(
		ctx,
		command.ID,
		domain.StatusCreated,
		domain.StatusDispatched,
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transition(
		ctx,
		command.ID,
		domain.StatusDispatched,
		domain.StatusRunning,
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transition(
		ctx,
		command.ID,
		domain.StatusRunning,
		domain.StatusSucceeded,
		"HELLO",
		"",
	); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, command.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusSucceeded || got.Output != "HELLO" {
		t.Fatalf("查询结果 = %#v", got)
	}
}

func openTestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 TEST_DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("../../../migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	return pool
}
