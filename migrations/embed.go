package migrations

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed 001_init.sql
var initialSchema string

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, initialSchema); err != nil {
		return fmt.Errorf("执行数据库迁移: %w", err)
	}
	return nil
}
