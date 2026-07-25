package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"example.com/multitopology-demo/internal/domain"
	goredis "github.com/redis/go-redis/v9"
)

const keyPrefix = "demo:worker:"

type Store struct {
	client *goredis.Client
	ttl    time.Duration
}

func New(client *goredis.Client, ttl time.Duration) *Store {
	return &Store{
		client: client,
		ttl:    ttl,
	}
}

func (s *Store) Upsert(ctx context.Context, presence domain.WorkerPresence) error {
	data, err := json.Marshal(presence)
	if err != nil {
		return fmt.Errorf("编码 worker 在线状态: %w", err)
	}
	if err := s.client.Set(
		ctx,
		keyPrefix+presence.WorkerID,
		data,
		s.ttl,
	).Err(); err != nil {
		return fmt.Errorf("写入 worker 在线状态: %w", err)
	}
	return nil
}

func (s *Store) IsOnline(ctx context.Context, workerID string) (bool, error) {
	count, err := s.client.Exists(ctx, keyPrefix+workerID).Result()
	if err != nil {
		return false, fmt.Errorf("查询 worker 在线状态: %w", err)
	}
	return count > 0, nil
}

func (s *Store) ListOnline(ctx context.Context) ([]domain.WorkerPresence, error) {
	var (
		cursor  uint64
		workers []domain.WorkerPresence
	)
	for {
		keys, next, err := s.client.Scan(ctx, cursor, keyPrefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("扫描 worker 在线状态: %w", err)
		}
		cursor = next
		for _, key := range keys {
			data, err := s.client.Get(ctx, key).Bytes()
			if err == goredis.Nil {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("读取 worker 在线状态: %w", err)
			}
			var presence domain.WorkerPresence
			if err := json.Unmarshal(data, &presence); err != nil {
				return nil, fmt.Errorf("解码 worker 在线状态: %w", err)
			}
			workers = append(workers, presence)
		}
		if cursor == 0 {
			break
		}
	}
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].WorkerID < workers[j].WorkerID
	})
	return workers, nil
}
