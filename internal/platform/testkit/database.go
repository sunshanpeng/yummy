// Package testkit 为产品服务提供隔离的真实 PostgreSQL 测试环境。
package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
)

var schemaSequence atomic.Int64

// OpenDatabase 只删除本次创建的随机 schema，不会清空指定数据库内已有的表。
// 未提供 TEST_DATABASE_URL 时跳过；已提供但不可用时必须失败。
func OpenDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(stdlib.GetConnector(*config))
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("yummy_platform_test_%d_%d", time.Now().UnixNano(), schemaSequence.Add(1))
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !strings.HasPrefix(schema, "yummy_platform_test_") {
			t.Error("拒绝清理不属于本测试的 schema")
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("清理测试 schema: %v", err)
		}
	})
	config.RuntimeParams["search_path"] = schema
	dsn := stdlib.RegisterConnConfig(config)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(dsn) })
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(db) })
	return db
}
