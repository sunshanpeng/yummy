package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed 001_init.sql
var initialSchema string

//go:embed *.sql
var migrationFiles embed.FS

// Apply 保留旧 demo 的调用契约；适配层共用 pgxpool，不另外创建数据库连接池。
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	return ApplySQL(ctx, db)
}

// ApplySQL 是正式产品与旧 demo 唯一的迁移入口，禁止与 AutoMigrate 混用。
func ApplySQL(ctx context.Context, db *sql.DB) error {
	// Goose 默认每五秒重试；多个控制面或隔离 schema 同时启动时会积累不必要的等待。
	// 保持相同锁 ID 和五分钟上限，只缩短重试间隔，不削弱跨进程互斥。
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return fmt.Errorf("配置迁移锁: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrationFiles,
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
		// 001 是历史上无 Goose 注解的 SQL，不能改写已发布文件。
		goose.WithExcludeNames([]string{"001_init.sql"}),
		goose.WithGoMigrations(goose.NewGoMigration(1, &goose.GoFunc{RunTx: applyBaseline}, nil)),
	)
	if err != nil {
		return fmt.Errorf("准备数据库迁移: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("执行数据库迁移: %w", err)
	}
	return nil
}

// applyBaseline 对空库创建 demo 表，对既有库核实历史结构后登记版本 1。
// 迁移状态与基线执行处于同一事务；失败时不假装旧结构已被正确接管。
func applyBaseline(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, initialSchema); err != nil {
		return fmt.Errorf("建立历史基线: %w", err)
	}
	tables := map[string]map[string]string{
		"workers": {
			"id": "text", "version": "text",
			"created_at": "timestamp with time zone", "updated_at": "timestamp with time zone",
		},
		"commands": {
			"id": "text", "worker_id": "text", "input": "text", "output": "text", "error": "text", "status": "text",
			"created_at": "timestamp with time zone", "updated_at": "timestamp with time zone",
		},
	}
	for table, expected := range tables {
		if err := verifyBaselineTable(ctx, tx, table, expected); err != nil {
			return err
		}
	}
	return nil
}

func verifyBaselineTable(ctx context.Context, tx *sql.Tx, table string, expected map[string]string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		return fmt.Errorf("检查历史表 %s: %w", table, err)
	}
	defer rows.Close()
	found := make(map[string]bool, len(expected))
	for rows.Next() {
		var column, dataType, nullable string
		if err := rows.Scan(&column, &dataType, &nullable); err != nil {
			return fmt.Errorf("读取历史表 %s: %w", table, err)
		}
		if want, ok := expected[column]; ok {
			if dataType != want || nullable != "NO" {
				return fmt.Errorf("历史表 %s.%s 不符合基线：需要非空 %s", table, column, want)
			}
			found[column] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取历史表 %s: %w", table, err)
	}
	if len(found) != len(expected) {
		return fmt.Errorf("历史表 %s 缺少基线字段，拒绝登记迁移版本", table)
	}
	var primaryKeyOK bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint c
			JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
			WHERE c.conrelid = to_regclass($1) AND c.contype = 'p'
			AND cardinality(c.conkey) = 1 AND a.attname = 'id'
		)`, table).Scan(&primaryKeyOK); err != nil {
		return fmt.Errorf("检查历史表 %s 主键: %w", table, err)
	}
	if !primaryKeyOK {
		return fmt.Errorf("历史表 %s 缺少 id 主键，拒绝登记迁移版本", table)
	}
	return nil
}
