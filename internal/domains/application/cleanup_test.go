package application_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/workspace"
	"gorm.io/gorm"
)

type buildNode string

func (n buildNode) CleanupBuildNode(*gorm.DB, string, string) (string, error) {
	return string(n), nil
}

func TestApplicationCleanupRequiresSeparateVolumeConfirmationAndCurrentLease(t *testing.T) {
	db, svc, root, member := fixture(t)
	ctx := context.Background()
	const space = "space-a"
	if err := db.Exec("INSERT INTO instance_settings (id, singleton) VALUES ('cleanup-instance', TRUE) ON CONFLICT DO NOTHING").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO execution_nodes(id, instance_id, name, worker_id, cluster_node_uid) SELECT 'node-a', id, 'node', 'worker-a', 'uid-a' FROM instance_settings WHERE singleton = TRUE").Error; err != nil {
		t.Fatal(err)
	}
	svc.Builds = buildNode("node-a")
	app, err := svc.Create(ctx, root, space, application.CreateInput{Name: "gateway", IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Archive(ctx, root, space, app.ID, app.Revision); err != nil {
		t.Fatal(err)
	}
	if state, err := svc.Delete(ctx, root, space, app.ID, 2); state != application.CleanupPending || err != nil {
		t.Fatalf("删除没有进入队列：%q %v", state, err)
	}
	spaces := workspace.NewService(db, svc.Access)
	spaces.ApplicationCleanup = svc
	if err := spaces.Archive(ctx, root, space, 1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("有待清理应用时允许归档空间：%v", err)
	}
	worker := application.CleanupWorker{NodeID: "node-a", ClusterUID: "uid-a", ConnectionEpoch: 1}
	claim, err := svc.ClaimCleanup(ctx, worker)
	if err != nil || claim.ApplicationID != app.ID || claim.SpaceSlug != space || claim.AppName != app.Name || claim.DeleteVolumes {
		t.Fatalf("首次领取清理：%+v %v", claim, err)
	}
	if _, err := svc.DeleteVolumes(ctx, root, space, app.ID, 2, app.Name); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("工作负载清理前可删卷：%v", err)
	}
	stale := worker
	stale.ConnectionEpoch++
	if err := svc.CompleteCleanup(ctx, stale, claim.Lease, application.CleanupResult{Done: true}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("不同连接可确认完成：%v", err)
	}
	if err := svc.CompleteCleanup(ctx, worker, claim.Lease, application.CleanupResult{ErrorCode: "PERMISSION_DENIED"}); err != nil {
		t.Fatal(err)
	}
	failed, err := svc.ListArchived(ctx, root, space, 10, "")
	if err != nil || len(failed.Items) != 1 || failed.Items[0].DeletionState != application.CleanupFailed || failed.Items[0].DeletionError != "PERMISSION_DENIED" {
		t.Fatalf("清理失败没有保留重试入口：%+v %v", failed, err)
	}
	if _, err := svc.Delete(ctx, root, space, app.ID, 2); err != nil {
		t.Fatal(err)
	}
	priorLease := claim.Lease
	claim, err = svc.ClaimCleanup(ctx, worker)
	if err != nil || claim.Lease.Revision <= priorLease.Revision || claim.DeleteVolumes {
		t.Fatalf("重试没有保持清理阶段：%+v %v", claim, err)
	}
	if err := svc.CompleteCleanup(ctx, worker, claim.Lease, application.CleanupResult{Done: true, HasVolumes: true}); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListArchived(ctx, root, space, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].DeletionState != application.CleanupWaitingVolumes {
		t.Fatalf("等待删卷状态：%+v %v", page, err)
	}
	if _, err := svc.Create(ctx, root, space, application.CreateInput{Name: app.Name, IdempotencyKey: "too-early"}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("卷保留时释放了名称：%v", err)
	}
	if _, err := svc.DeleteVolumes(ctx, member, space, app.ID, 2, app.Name); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员可确认删卷：%v", err)
	}
	if _, err := svc.DeleteVolumes(ctx, root, space, app.ID, 2, "another-app"); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("确认名称不匹配仍受理删卷：%v", err)
	}
	if _, err := svc.DeleteVolumes(ctx, root, space, app.ID, 2, app.Name); err != nil {
		t.Fatal(err)
	}
	volume, err := svc.ClaimCleanup(ctx, worker)
	if err != nil || !volume.DeleteVolumes || volume.Lease.Revision <= claim.Lease.Revision {
		t.Fatalf("删卷领取：%+v %v", volume, err)
	}
	if err := svc.CheckCleanup(ctx, worker, claim.Lease); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("旧租约仍有效：%v", err)
	}
	if err := svc.CompleteCleanup(ctx, worker, volume.Lease, application.CleanupResult{Done: true, HasVolumes: true}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("有卷时允许标记删除完成：%v", err)
	}
	if err := svc.CompleteCleanup(ctx, worker, volume.Lease, application.CleanupResult{Done: true}); err != nil {
		t.Fatal(err)
	}
	other, err := svc.Create(ctx, root, space, application.CreateInput{Name: app.Name, IdempotencyKey: "replacement"})
	if err != nil || other.ID == app.ID {
		t.Fatalf("清理完成后同名重建：%+v %v", other, err)
	}
}
