package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/sunshanpeng/yummy/internal/presence/redis"
	"github.com/sunshanpeng/yummy/internal/repository/postgres"
	"github.com/sunshanpeng/yummy/migrations"
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

	redisOptions, err := parseRedisOptions(config.RedisAddr)
	if err != nil {
		pool.Close()
		return nil, err
	}
	redisClient := goredis.NewClient(redisOptions)
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

// parseRedisOptions 兼容 host:port 和 redis:// / rediss:// URL，
// 后者可以携带用户名、密码、数据库编号并启用 TLS。
func parseRedisOptions(address string) (*goredis.Options, error) {
	if !strings.Contains(address, "://") {
		return &goredis.Options{Addr: address}, nil
	}
	options, err := goredis.ParseURL(address)
	if err != nil {
		// 解析错误可能带出 URL 片段（例如密码中未编码的 / # ?），因此不返回底层错误。
		return nil, errors.New("解析 Redis URL 失败：请检查格式，用户名和密码中的保留字符需要百分号编码")
	}
	return options, nil
}

func (d *Dependencies) Close() {
	_ = d.Redis.Close()
	d.Postgres.Close()
}
