package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/store/query"
	legacy "github.com/sunshanpeng/yummy/internal/repository/postgres"
	"github.com/sunshanpeng/yummy/migrations"
	"gorm.io/gorm"
)

var schemaSequence atomic.Int64

func TestOpenRejectsEmptyDatabaseURL(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("空数据库配置必须拒绝")
	}
}

// 每个测试独占一个临时 schema，禁止清空 TEST_DATABASE_URL 所指数据库的既有表。
func isolatedDatabase(t *testing.T) (string, *sql.DB, *pgx.ConnConfig) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("yummy_store_test_%d_%d", time.Now().UnixNano(), schemaSequence.Add(1))
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !strings.HasPrefix(schema, "yummy_store_test_") {
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
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return dsn, db, config
}

func openIsolatedStore(t *testing.T) *gorm.DB {
	t.Helper()
	dsn, _, _ := isolatedDatabase(t)
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Close(db) })
	return db
}

func TestMigrationsAreVersionedAndRepeatable(t *testing.T) {
	_, db, _ := isolatedDatabase(t)
	ctx := context.Background()
	for range 2 {
		if err := migrations.ApplySQL(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id IN (1, 2, 3) AND is_applied`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("已应用的版本数量 = %d，期望 3", count)
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name IN ('users', 'applications', 'commands', 'workers')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 4 {
		t.Fatalf("核心/旧 demo 表数量 = %d，期望 4", tables)
	}
}

func TestMigrationsSerializeConcurrentEntrypoints(t *testing.T) {
	_, db, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errors <- migrations.ApplySQL(ctx, db)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationAdoptsLegacySchemaAndKeepsPGXRepository(t *testing.T) {
	_, db, config := isolatedDatabase(t)
	ctx := context.Background()
	// 使用原始已发布 SQL 建立无 Goose 历史的旧库。
	initialSchema, err := os.ReadFile("../../../migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(initialSchema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO commands(id, worker_id, input, status, created_at, updated_at) VALUES ('preserved', 'worker-1', 'old', 'CREATED', NOW(), NOW())`); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig = config.Copy()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repository := legacy.New(pool)
	command, err := repository.Get(ctx, "preserved")
	if err != nil || command.Input != "old" {
		t.Fatalf("旧数据未保留: %#v, %v", command, err)
	}
	if err := repository.Transition(ctx, command.ID, domain.StatusCreated, domain.StatusDispatched, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRejectsDriftedLegacySchema(t *testing.T) {
	_, db, _ := isolatedDatabase(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE workers(id TEXT PRIMARY KEY, version BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.ApplySQL(ctx, db); err == nil {
		t.Fatal("与既有基线不一致时必须拒绝继续迁移")
	}
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id = 1 AND is_applied`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatal("失败的基线不能登记为已应用")
	}
}

func createFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, value := range []any{
		&model.User{ID: "user-1", Username: "alice", PasswordHash: "test-hash", Status: "ACTIVE", AuthVersion: 1},
		&model.Workspace{ID: "space-a", Name: "空间甲", Slug: "space-a", Status: "ACTIVE", Revision: 1},
		&model.Workspace{ID: "space-b", Name: "空间乙", Slug: "space-b", Status: "ACTIVE", Revision: 1},
		&model.WorkspaceMember{WorkspaceID: "space-a", UserID: "user-1", Role: "admin"},
		&model.Application{ID: "app-a", WorkspaceID: "space-a", Name: "gateway", Status: "ACTIVE", Revision: 1},
		&model.Application{ID: "app-b", WorkspaceID: "space-b", Name: "gateway", Status: "ACTIVE", Revision: 1},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func requirePostgresCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != code {
		t.Fatalf("错误 = %v，期望 PostgreSQL %s", err, code)
	}
}

func TestCompositeConstraintsRejectCrossWorkspaceReferences(t *testing.T) {
	db := openIsolatedStore(t)
	createFixture(t, db)
	requirePostgresCode(t, db.Create(&model.ApplicationMember{WorkspaceID: "space-a", ApplicationID: "app-b", UserID: "user-1", Role: "viewer"}).Error, "23503")
	requirePostgresCode(t, db.Create(&model.ApplicationMember{WorkspaceID: "space-b", ApplicationID: "app-b", UserID: "user-1", Role: "viewer"}).Error, "23503")
	requirePostgresCode(t, db.Create(&model.Application{ID: "app-duplicate", WorkspaceID: "space-a", Name: "gateway"}).Error, "23505")
	requirePostgresCode(t, db.Create(&model.User{ID: "user-uppercase", Username: "ALICE", PasswordHash: "hash"}).Error, "23514")
	requirePostgresCode(t, db.Create(&model.User{ID: "user-duplicate", Username: "alice", PasswordHash: "hash"}).Error, "23505")
	if err := db.Create(&model.ApplicationMember{WorkspaceID: "space-a", ApplicationID: "app-a", UserID: "user-1", Role: "viewer"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("workspace_id = ? AND user_id = ?", "space-a", "user-1").Delete(&model.WorkspaceMember{}).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ApplicationMember{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("移除空间成员必须移除原应用授权: %d, %v", count, err)
	}
}

func TestAccessTokenScopesRoundTripAndApplicationBoundaries(t *testing.T) {
	db := openIsolatedStore(t)
	createFixture(t, db)
	token := model.AccessToken{ID: "token-1", UserID: "user-1", Name: "测试凭证", TokenHash: "test-digest", Scopes: []string{"build:read", "logs:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	var got model.AccessToken
	if err := db.First(&got, "id = ?", token.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "build:read" {
		t.Fatalf("scope JSONB 未正确回读: %#v", got.Scopes)
	}
	requirePostgresCode(t, db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: "space-a", ApplicationID: "app-b"}).Error, "23503")
	if err := db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: "space-a", ApplicationID: "app-a"}).Error; err != nil {
		t.Fatal(err)
	}
	requirePostgresCode(t, db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: "space-a", ApplicationID: "app-a"}).Error, "23505")
}

func TestGeneratedQueryKeepsSerializationAndTransactionBoundary(t *testing.T) {
	db := openIsolatedStore(t)
	createFixture(t, db)
	ctx := context.Background()
	queries := query.Use(db)
	token := &model.AccessToken{ID: "gen-token", UserID: "user-1", Name: "生成查询", TokenHash: "gen-token-digest", Scopes: []string{"build:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := queries.AccessToken.WithContext(ctx).Create(token); err != nil {
		t.Fatal(err)
	}
	got, err := queries.AccessToken.WithContext(ctx).Where(queries.AccessToken.ID.Eq(token.ID)).First()
	if err != nil || len(got.Scopes) != 1 || got.Scopes[0] != "build:read" {
		t.Fatalf("生成 DAO 的 JSONB 回读错误: %#v, %v", got, err)
	}
	rollback := errors.New("撤销事务")
	err = queries.Transaction(func(tx *query.Query) error {
		if err := tx.AuditEvent.WithContext(ctx).Create(&model.AuditEvent{ID: "gen-audit", ActorID: "user-1", Action: "account.token.create", ObjectType: "token", ObjectID: token.ID, Result: "SUCCESS"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	count, err := queries.AuditEvent.WithContext(ctx).Where(queries.AuditEvent.ID.Eq("gen-audit")).Count()
	if err != nil || count != 0 {
		t.Fatalf("生成 DAO 未保持事务边界: count=%d, %v", count, err)
	}
}

func TestConditionalInvitationConsumptionHasOneWinner(t *testing.T) {
	db := openIsolatedStore(t)
	createFixture(t, db)
	invitation := model.Invitation{ID: "invite-1", WorkspaceID: "space-a", Username: "alice", Role: "member", TokenHash: "invite-digest", CreatedBy: "user-1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&invitation).Error; err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int64
	var workers sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := db.Model(&model.Invitation{}).
				Where("id = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()", invitation.ID).
				Updates(map[string]any{"consumed_at": time.Now().UTC(), "consumed_by": "user-1"})
			if result.Error != nil {
				errors <- result.Error
				return
			}
			winners.Add(result.RowsAffected)
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if winners.Load() != 1 {
		t.Fatalf("邀请消费成功次数 = %d，期望 1", winners.Load())
	}
}

func TestRevisionConditionalUpdateHasOneWinner(t *testing.T) {
	db := openIsolatedStore(t)
	createFixture(t, db)
	var winners atomic.Int64
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := db.Model(&model.Application{}).
				Where("workspace_id = ? AND id = ? AND revision = ?", "space-a", "app-a", 1).
				Updates(map[string]any{"description": "更新后", "revision": gorm.Expr("revision + 1")})
			if result.Error != nil {
				failures <- result.Error
				return
			}
			winners.Add(result.RowsAffected)
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if winners.Load() != 1 {
		t.Fatalf("同一 revision 更新成功次数 = %d，期望 1", winners.Load())
	}
	var application model.Application
	if err := db.First(&application, "id = ?", "app-a").Error; err != nil {
		t.Fatal(err)
	}
	if application.Revision != 2 {
		t.Fatalf("revision = %d，期望 2", application.Revision)
	}
}

func TestTransactionRollsBackDomainChangeAndAuditTogether(t *testing.T) {
	db := openIsolatedStore(t)
	createFixture(t, db)
	rollback := errors.New("主动回滚")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Application{}).Where("id = ? AND revision = ?", "app-a", 1).Updates(map[string]any{"description": "未提交", "revision": 2}).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.AuditEvent{ID: "audit-1", ActorID: "user-1", Action: "application.update", ObjectType: "application", ObjectID: "app-a", Result: "SUCCESS"}).Error; err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var application model.Application
	if err := db.First(&application, "id = ?", "app-a").Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.AuditEvent{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if application.Revision != 1 || application.Description != "" || count != 0 {
		t.Fatalf("回滚未完整生效: %#v, audit_count=%d", application, count)
	}
}
