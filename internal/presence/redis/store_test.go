package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"example.com/multitopology-demo/internal/domain"
	goredis "github.com/redis/go-redis/v9"
)

func TestPresenceExpires(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
	}

	ctx := context.Background()
	client := goredis.NewClient(&goredis.Options{
		Addr: address,
		DB:   15,
	})
	t.Cleanup(func() {
		_ = client.FlushDB(context.Background()).Err()
		_ = client.Close()
	})
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	store := New(client, 100*time.Millisecond)
	if err := store.Upsert(ctx, domain.WorkerPresence{
		WorkerID:   "worker-1",
		InstanceID: "control-1",
		Version:    "demo",
		LastSeen:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	online, err := store.IsOnline(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !online {
		t.Fatal("刚写入的 worker 应在线")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		online, err = store.IsOnline(ctx, "worker-1")
		if err != nil {
			t.Fatal(err)
		}
		if !online {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("worker 在线状态未按 TTL 过期")
}

func TestListOnlineReturnsPresence(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
	}

	ctx := context.Background()
	client := goredis.NewClient(&goredis.Options{
		Addr: address,
		DB:   15,
	})
	t.Cleanup(func() {
		_ = client.FlushDB(context.Background()).Err()
		_ = client.Close()
	})
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	store := New(client, time.Second)
	if err := store.Upsert(ctx, domain.WorkerPresence{
		WorkerID:   "worker-1",
		InstanceID: "control-1",
		Version:    "demo",
		LastSeen:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	workers, err := store.ListOnline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].WorkerID != "worker-1" {
		t.Fatalf("在线列表 = %#v", workers)
	}
}
