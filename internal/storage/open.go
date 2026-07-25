package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/multitopology-demo/internal/presence/redis"
	"example.com/multitopology-demo/internal/repository/postgres"
	"example.com/multitopology-demo/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

type Config struct {
	DatabaseURL string
	RedisAddr   string
	PresenceTTL time.Duration
}

type Dependencies struct {
	Postgres *pgxpool.Pool
	Redis    *goredis.Client
	Commands *postgres.Repository
	Presence *redis.Store
}

func Open(ctx context.Context, config Config) (*Dependencies, error) {
	if config.DatabaseURL == "" || config.RedisAddr == "" || config.PresenceTTL <= 0 {
		return nil, errors.New("数据库、Redis 和在线 TTL 配置不能为空")
	}

	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("检查 PostgreSQL: %w", err)
	}
	if err := migrations.Apply(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	redisClient := goredis.NewClient(&goredis.Options{Addr: config.RedisAddr})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		pool.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("检查 Redis: %w", err)
	}

	return &Dependencies{
		Postgres: pool,
		Redis:    redisClient,
		Commands: postgres.New(pool),
		Presence: redis.New(redisClient, config.PresenceTTL),
	}, nil
}

func (d *Dependencies) Close() {
	_ = d.Redis.Close()
	d.Postgres.Close()
}
