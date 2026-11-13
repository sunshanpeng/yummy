// cleanup_test.go 在真实 PostgreSQL 上验证过期上传与快照的系统级回收语义。
package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
)

// cleanupEnv 在 source 基础环境上补齐构建内核基础设施，builds 行经真实
// build.Service 受理产生，保证引用事实与生产状态机一致。
type cleanupEnv struct {
	*environment
	builder *build.Service
}

func cleanupSetup(t *testing.T) *cleanupEnv {
	t.Helper()
	base := setup(t)
	ctx := context.Background()
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.NewService(base.db, base.access, box)
	node, err := inv.CreateNode(ctx, base.admin, inventory.NodeInput{Name: "构建节点", WorkerID: "worker-cleanup", Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := inv.CreateRegistry(ctx, base.admin, inventory.RegistryInput{Name: "仓库", URL: "https://registry.example.com", RepositoryPrefix: "tenant", Username: "robot", Password: "never-in-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.SavePolicy(ctx, base.admin, inventory.PolicyInput{BuildNodeID: node.ID, RuntimeNodeID: node.ID, RegistryID: registry.ID, RepositoryPrefix: "apps", ResourceDefault: inventory.Resources{CPUMillis: 500, MemoryMiB: 512}, ResourceLimits: inventory.Resources{CPUMillis: 1000, MemoryMiB: 1024}}); err != nil {
		t.Fatal(err)
	}
	if _, err := inv.SetAllocation(ctx, base.admin, "space", inventory.AllocationInput{AllowedNodeIDs: []string{node.ID}, MaxApplications: 10, BuildConcurrency: 2, CPUMillis: 2000, MemoryMiB: 2048}); err != nil {
		t.Fatal(err)
	}
	// 只有测试显式写入节点与仓库就绪事实；不声称本机执行过真实构建。
	for _, item := range []struct {
		query string
		args  []any
	}{
		{"UPDATE execution_nodes SET cluster_node_uid='node-uid',os='linux',arch='amd64',last_seen=NOW(),build_ready=TRUE WHERE id=?", []any{node.ID}},
		{"UPDATE registries SET verified_at=NOW(),verified_revision=revision WHERE id=?", []any{registry.ID}},
	} {
		if err := base.db.Exec(item.query, item.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	return &cleanupEnv{environment: base, builder: build.NewService(base.db, base.access, inv)}
}

// fixtureBuild 经真实 CreateBuild 受理引用指定快照的构建；state 非默认排队态时
// 测试显式写入目标状态（只构造引用事实，不模拟执行事件流）。
func (e *cleanupEnv) fixtureBuild(t *testing.T, snapshotID, state string) string {
	t.Helper()
	created, err := e.builder.CreateBuild(context.Background(), e.admin, "space", "app", build.CreateInput{SourceSnapshotID: snapshotID, ExpectedPolicyRevision: 1, IdempotencyKey: "cleanup-" + state + "-" + snapshotID})
	if err != nil {
		t.Fatal(err)
	}
	if state != build.Queued {
		if err := e.db.Exec("UPDATE builds SET state = ?, finished_at = NOW() WHERE id = ?", state, created.ID).Error; err != nil {
			t.Fatal(err)
		}
	}
	return created.ID
}

func expireSnapshot(t *testing.T, e *environment, snapshotID string) {
	t.Helper()
	// CHECK (expires_at > created_at) 要求同时回溯创建时间。
	if err := e.db.Model(&snapshotRecord{}).Where("id = ?", snapshotID).Updates(map[string]any{"created_at": time.Now().Add(-8 * 24 * time.Hour), "expires_at": time.Now().Add(-time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
}

func expireUpload(t *testing.T, e *environment, uploadID string) {
	t.Helper()
	if err := e.db.Model(&uploadRecord{}).Where("id = ?", uploadID).Updates(map[string]any{"created_at": time.Now().Add(-time.Hour), "expires_at": time.Now().Add(-time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
}

func rowCount(t *testing.T, db *gorm.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.Raw(query, args...).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func storedFileExists(e *environment, key string) bool {
	_, err := os.Stat(filepath.Join(e.root, key))
	return err == nil
}

func mustGetUpload(t *testing.T, e *environment, uploadID string) uploadRecord {
	t.Helper()
	var stored uploadRecord
	if err := e.db.First(&stored, "id = ?", uploadID).Error; err != nil {
		t.Fatalf("上传行应存在: %v", err)
	}
	return stored
}

func writeStoredFile(t *testing.T, e *environment, key string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.root, key), e.data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupExpiredUploadsRemoveFileAndRow(t *testing.T) {
	e := cleanupSetup(t)
	// PENDING 会话通常尚无落盘文件；手写一份模拟 Upload 中断后的残留。
	pending := e.create(t)
	pendingRow := mustGetUpload(t, e.environment, pending.ID)
	writeStoredFile(t, e.environment, pendingRow.StorageKey)
	// UPLOADED 会话持有接收时 rename 的真实文件。
	received := e.received(t)
	uploadedRow := mustGetUpload(t, e.environment, received.ID)
	if !storedFileExists(e.environment, uploadedRow.StorageKey) {
		t.Fatal("UPLOADED 文件应存在")
	}
	expireUpload(t, e.environment, pending.ID)
	expireUpload(t, e.environment, received.ID)
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || removed != 2 {
		t.Fatalf("过期上传回收: removed=%d err=%v", removed, err)
	}
	for _, row := range []uploadRecord{pendingRow, uploadedRow} {
		var count int64
		if err := e.db.Model(&uploadRecord{}).Where("id = ?", row.ID).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("上传行未删除: %s %d %v", row.ID, count, err)
		}
		if storedFileExists(e.environment, row.StorageKey) {
			t.Fatalf("存储文件未删除: %s", row.StorageKey)
		}
	}
}

func TestCleanupSnapshotReferencedByActiveBuildIsSkipped(t *testing.T) {
	e := cleanupSetup(t)
	ready := e.ready(t)
	buildID := e.fixtureBuild(t, ready.ID, build.Running)
	var snapshot snapshotRecord
	if err := e.db.First(&snapshot, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	expireSnapshot(t, e.environment, ready.ID)
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || removed != 0 {
		t.Fatalf("在途构建引用应完全跳过: removed=%d err=%v", removed, err)
	}
	var after snapshotRecord
	if err := e.db.First(&after, "id = ?", ready.ID).Error; err != nil || after.State != StateReady {
		t.Fatalf("快照行不应变化: %+v %v", after.State, err)
	}
	if !storedFileExists(e.environment, snapshot.StorageKey) {
		t.Fatal("在途构建的快照文件不可删除")
	}
	if row := mustGetUpload(t, e.environment, snapshot.UploadID); row.State != StateComplete {
		t.Fatalf("上传行不应变化: %+v", row)
	}
	var builds int64
	if err := e.db.Table("builds").Where("id = ?", buildID).Count(&builds).Error; err != nil || builds != 1 {
		t.Fatalf("构建行应保留: %d %v", builds, err)
	}
}

func TestCleanupSnapshotReferencedByFinishedBuildExpiresRow(t *testing.T) {
	e := cleanupSetup(t)
	ready := e.ready(t)
	buildID := e.fixtureBuild(t, ready.ID, build.Succeeded)
	var snapshot snapshotRecord
	if err := e.db.First(&snapshot, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	expireSnapshot(t, e.environment, ready.ID)
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || removed != 1 {
		t.Fatalf("终态引用快照应回收文件: removed=%d err=%v", removed, err)
	}
	var after snapshotRecord
	if err := e.db.First(&after, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.State != StateExpired {
		t.Fatalf("快照行应置 EXPIRED: %s", after.State)
	}
	if storedFileExists(e.environment, snapshot.StorageKey) {
		t.Fatal("快照文件未删除")
	}
	if row := mustGetUpload(t, e.environment, snapshot.UploadID); row.State != StateComplete {
		t.Fatalf("被引用上传行应保留: %+v", row)
	}
	var builds int64
	if err := e.db.Table("builds").Where("id = ?", buildID).Count(&builds).Error; err != nil || builds != 1 {
		t.Fatalf("构建行应保留: %d %v", builds, err)
	}
	// 已置 EXPIRED 的行不再被选取，重复清理必须无操作。
	again, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || again != 0 {
		t.Fatalf("重复清理应幂等: removed=%d err=%v", again, err)
	}
}

func TestCleanupUnreferencedSnapshotRemovesRowsAndFile(t *testing.T) {
	e := cleanupSetup(t)
	ready := e.ready(t)
	var snapshot snapshotRecord
	if err := e.db.First(&snapshot, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	expireSnapshot(t, e.environment, ready.ID)
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || removed != 1 {
		t.Fatalf("无引用快照应回收: removed=%d err=%v", removed, err)
	}
	var snapshots int64
	if err := e.db.Model(&snapshotRecord{}).Where("id = ?", ready.ID).Count(&snapshots).Error; err != nil || snapshots != 0 {
		t.Fatalf("快照行未删除: %d %v", snapshots, err)
	}
	var uploads int64
	if err := e.db.Model(&uploadRecord{}).Where("id = ?", snapshot.UploadID).Count(&uploads).Error; err != nil || uploads != 0 {
		t.Fatalf("成对上传行未删除: %d %v", uploads, err)
	}
	if storedFileExists(e.environment, snapshot.StorageKey) {
		t.Fatal("快照文件未删除")
	}
}

func TestCleanupLimitBoundsEachClass(t *testing.T) {
	t.Run("上传", func(t *testing.T) {
		e := cleanupSetup(t)
		ids := make([]string, 0, 3)
		keys := make([]string, 0, 3)
		for range 3 {
			upload := e.create(t)
			row := mustGetUpload(t, e.environment, upload.ID)
			writeStoredFile(t, e.environment, row.StorageKey)
			expireUpload(t, e.environment, upload.ID)
			ids = append(ids, upload.ID)
			keys = append(keys, row.StorageKey)
		}
		removed, err := e.service.Cleanup(context.Background(), time.Now(), 2)
		if err != nil || removed != 2 {
			t.Fatalf("limit 应限制每轮处理量: removed=%d err=%v", removed, err)
		}
		var remaining int64
		if err := e.db.Model(&uploadRecord{}).Where("id IN ?", ids).Count(&remaining).Error; err != nil || remaining != 1 {
			t.Fatalf("应剩一行: %d %v", remaining, err)
		}
		if removed, err = e.service.Cleanup(context.Background(), time.Now(), 10); err != nil || removed != 1 {
			t.Fatalf("次轮应清空: removed=%d err=%v", removed, err)
		}
		for _, key := range keys {
			if storedFileExists(e.environment, key) {
				t.Fatalf("文件未删除: %s", key)
			}
		}
	})
	t.Run("快照", func(t *testing.T) {
		e := cleanupSetup(t)
		first, second := e.ready(t), e.ready(t)
		expireSnapshot(t, e.environment, first.ID)
		expireSnapshot(t, e.environment, second.ID)
		removed, err := e.service.Cleanup(context.Background(), time.Now(), 1)
		if err != nil || removed != 1 {
			t.Fatalf("limit 应限制每轮处理量: removed=%d err=%v", removed, err)
		}
		var remaining int64
		if err := e.db.Model(&snapshotRecord{}).Count(&remaining).Error; err != nil || remaining != 1 {
			t.Fatalf("应剩一个快照: %d %v", remaining, err)
		}
		if removed, err = e.service.Cleanup(context.Background(), time.Now(), 10); err != nil || removed != 1 {
			t.Fatalf("次轮应清空: removed=%d err=%v", removed, err)
		}
		var uploads int64
		if err := e.db.Model(&uploadRecord{}).Count(&uploads).Error; err != nil || uploads != 0 {
			t.Fatalf("成对上传行应全部回收: %d %v", uploads, err)
		}
	})
}

func TestCleanupMissingFileStillRemovesRow(t *testing.T) {
	e := cleanupSetup(t)
	// PENDING 会话从未落盘：文件缺失应视为已删除并照常删行。
	pending := e.create(t)
	expireUpload(t, e.environment, pending.ID)
	// 快照文件被外部删除：同样照常删行删成对上传行。
	ready := e.ready(t)
	var snapshot snapshotRecord
	if err := e.db.First(&snapshot, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.root, snapshot.StorageKey)); err != nil {
		t.Fatal(err)
	}
	expireSnapshot(t, e.environment, ready.ID)
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || removed != 2 {
		t.Fatalf("文件缺失不应阻塞回收: removed=%d err=%v", removed, err)
	}
	var rows int64
	if err := e.db.Model(&uploadRecord{}).Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("上传行应删除: %d %v", rows, err)
	}
	if err := e.db.Model(&snapshotRecord{}).Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("快照行应删除: %d %v", rows, err)
	}
}

func TestCleanupFreshItemsUntouched(t *testing.T) {
	e := cleanupSetup(t)
	pending := e.create(t)
	received := e.received(t)
	ready := e.ready(t)
	uploadedRow := mustGetUpload(t, e.environment, received.ID)
	var snapshot snapshotRecord
	if err := e.db.First(&snapshot, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err != nil || removed != 0 {
		t.Fatalf("未过期不应回收: removed=%d err=%v", removed, err)
	}
	var rows int64
	if err := e.db.Model(&uploadRecord{}).Count(&rows).Error; err != nil || rows != 3 {
		t.Fatalf("上传行应保留: %d %v", rows, err)
	}
	if row := mustGetUpload(t, e.environment, pending.ID); row.State != StatePending {
		t.Fatalf("未过期上传行应原样保留: %+v", row)
	}
	if err := e.db.Model(&snapshotRecord{}).Count(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("快照行应保留: %d %v", rows, err)
	}
	for _, key := range []string{uploadedRow.StorageKey, snapshot.StorageKey} {
		if !storedFileExists(e.environment, key) {
			t.Fatalf("未过期文件应保留: %s", key)
		}
	}
	// limit 非法返回 ErrInvalid。
	if _, err := e.service.Cleanup(context.Background(), time.Now(), 0); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("非法 limit: %v", err)
	}
	// 系统维护不写审计事件（actor_id 外键禁止伪造操作者）。
	var audits int64
	if err := e.db.Table("audit_events").Where("action LIKE 'source.cleanup%'").Count(&audits).Error; err != nil || audits != 0 {
		t.Fatalf("不应产生清理审计: %d %v", audits, err)
	}
}

func TestCleanupContinuesAfterIndividualFailure(t *testing.T) {
	e := cleanupSetup(t)
	blocked := e.create(t)
	blockedRow := mustGetUpload(t, e.environment, blocked.ID)
	// 以同名目录占位使文件删除失败；目录内放文件确保任何删除语义都失败。
	if err := os.Mkdir(filepath.Join(e.root, blockedRow.StorageKey), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, blockedRow.StorageKey, "inner"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	good := e.create(t)
	goodRow := mustGetUpload(t, e.environment, good.ID)
	writeStoredFile(t, e.environment, goodRow.StorageKey)
	expireUpload(t, e.environment, blocked.ID)
	expireUpload(t, e.environment, good.ID)
	removed, err := e.service.Cleanup(context.Background(), time.Now(), 10)
	if err == nil || removed != 1 {
		t.Fatalf("单条失败应记录并继续: removed=%d err=%v", removed, err)
	}
	if row := mustGetUpload(t, e.environment, blocked.ID); row.ID != blocked.ID {
		t.Fatalf("失败条目行应保留: %+v", row)
	}
	if !storedFileExists(e.environment, blockedRow.StorageKey) || !storedFileExists(e.environment, blockedRow.StorageKey+"/inner") {
		t.Fatal("占位目录应保留供排查")
	}
	if storedFileExists(e.environment, goodRow.StorageKey) {
		t.Fatal("成功条目文件未删除")
	}
}
