package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
)

func readyNode(t *testing.T, env testEnvironment, node NodeView, uid string) {
	t.Helper()
	now := time.Now().UTC()
	if err := env.db.Model(&nodeRecord{}).Where("id = ?", node.ID).Updates(map[string]any{"cluster_node_uid": uid, "os": "linux", "arch": "amd64", "connected_at": now, "last_seen": now, "build_ready": true, "runtime_ready": true, "ingress_ready": true, "metrics_ready": true, "readiness_reason": "已核验登记资格证据"}).Error; err != nil {
		t.Fatal(err)
	}
}

func auditActions(env testEnvironment, objectID string) map[string]bool {
	var events []model.AuditEvent
	if err := env.db.Where("object_id = ?", objectID).Find(&events).Error; err != nil {
		return nil
	}
	result := map[string]bool{}
	for _, event := range events {
		result[event.Action] = true
	}
	return result
}

// 资格登记是实例级平台管理；空间管理员、普通成员和PAT都不能代替安装者验收。
func TestQualificationManagementIsInstanceAdminOnly(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	input := QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Runtime: true, Ingress: true, Metrics: true, Evidence: map[string]string{"验收人": "安装者"}}
	if _, err := env.service.RegisterQualification(ctx, env.admin, input); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间管理员登记资格=%v", err)
	}
	if _, err := env.service.RegisterQualification(ctx, env.viewer, input); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员登记资格=%v", err)
	}
	if _, err := env.service.RevokeQualification(ctx, env.admin, "nqf_missing"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间管理员撤销资格=%v", err)
	}
	if _, err := env.service.ListQualifications(ctx, env.viewer, 20, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员列出资格=%v", err)
	}
	token := model.AccessToken{ID: "pat", UserID: "root", Name: "限定凭证", TokenHash: "token-digest", Scopes: []string{"application:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := env.db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	bearer := authorization.Principal{UserID: "root", AuthVersion: 1, TokenID: "pat"}
	if _, err := env.service.RegisterQualification(ctx, bearer, input); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT继承实例管理=%v", err)
	}
	if _, err := env.service.ListQualifications(ctx, bearer, 20, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT列出资格=%v", err)
	}
}

func TestQualificationRegisterValidatesNodeIdentityAndConflicts(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "备用节点", WorkerID: "worker-2", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	base := QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Runtime: true, Ingress: true, Metrics: true, Evidence: map[string]string{"验收记录": "线下安装验收通过"}}
	// 节点必须存在，且worker身份必须与节点登记一致。
	missing := base
	missing.NodeID = "nod_missing"
	if _, err := env.service.RegisterQualification(ctx, env.root, missing); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("不存在节点登记=%v", err)
	}
	mismatched := base
	mismatched.WorkerID = "worker-2"
	if _, err := env.service.RegisterQualification(ctx, env.root, mismatched); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("worker身份不一致=%v", err)
	}
	// 节点在线核验过其他UID时拒绝，防止资格记录与真实节点事实冲突。
	readyNode(t, env, node, "uid-real")
	conflictUID := base
	if _, err := env.service.RegisterQualification(ctx, env.root, conflictUID); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("UID与节点事实不一致=%v", err)
	}
	if err := env.db.Model(&nodeRecord{}).Where("id = ?", node.ID).Updates(map[string]any{"cluster_node_uid": "", "build_ready": false, "runtime_ready": false, "ingress_ready": false, "metrics_ready": false, "readiness_reason": "等待真实节点资格检查"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, input := range []QualificationInput{
		{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "  ", QualificationID: "q1", Build: true},
		{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "带空格 的标识", Build: true},
		{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "q1", OS: "windows", Build: true},
		{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "q1", Arch: "arm64", Build: true},
		{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "q1"},
		{NodeID: node.ID, WorkerID: "invalid/worker", ClusterNodeUID: "uid-a", QualificationID: "q1", Build: true},
	} {
		if _, err := env.service.RegisterQualification(ctx, env.root, input); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("非法输入 %#v = %v", input, err)
		}
	}
	// 平台留空时按数据库默认补齐为linux/amd64。
	view, err := env.service.RegisterQualification(ctx, env.root, QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Runtime: true, Evidence: map[string]string{"验收记录": "线下安装验收通过"}})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID == "" || view.Status != "ACTIVE" || view.OS != "linux" || view.Arch != "amd64" || view.CreatedBy != "root" || view.RevokedAt != nil {
		t.Fatalf("登记视图=%#v", view)
	}
	if !view.BuildReady || !view.RuntimeReady || view.IngressReady || view.MetricsReady {
		t.Fatalf("能力位=%#v", view)
	}
	// 登记不把节点置为就绪：就绪只能由worker连接核验产生。
	var stored nodeRecord
	if err := env.db.First(&stored, "id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BuildReady || stored.RuntimeReady || stored.ReadinessReason != "等待真实节点资格检查" {
		t.Fatalf("登记伪造就绪=%#v", stored)
	}
	// 同一节点已有活动资格时必须先显式撤销。
	duplicate := base
	duplicate.QualificationID = "install-accept-2"
	if _, err := env.service.RegisterQualification(ctx, env.root, duplicate); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("重复活动资格=%v", err)
	}
	// 活动qualification_id全局唯一，不能被另一节点复用。
	reused := base
	reused.NodeID = other.ID
	reused.WorkerID = "worker-2"
	if _, err := env.service.RegisterQualification(ctx, env.root, reused); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("跨节点复用上报标识=%v", err)
	}
	// 数据库约束兜底：绕过服务也不能写出第二条活动记录。
	if err := env.db.Exec(`INSERT INTO node_qualifications (id, node_id, worker_id, cluster_node_uid, os, arch, build_ready, qualification_id, evidence, status, created_by) VALUES ('nqf_raw', ?, 'worker-1', 'uid-a', 'linux', 'amd64', TRUE, 'install-accept-1', '{}'::jsonb, 'ACTIVE', 'root')`, node.ID).Error; err == nil {
		t.Fatal("数据库未约束活动qualification_id唯一")
	}
	actions := auditActions(env, view.ID)
	if !actions["instance.node.qualification.register"] {
		t.Fatalf("缺少登记审计=%v", actions)
	}
}

func TestQualificationRevokeIsIdempotentAndClearsReadiness(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	view, err := env.service.RegisterQualification(ctx, env.root, QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Runtime: true, Ingress: true, Metrics: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.RevokeQualification(ctx, env.root, "nqf_missing"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("撤销不存在记录=%v", err)
	}
	readyNode(t, env, node, "uid-a")
	revoked, err := env.service.RevokeQualification(ctx, env.root, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != "REVOKED" || revoked.RevokedAt == nil {
		t.Fatalf("撤销视图=%#v", revoked)
	}
	// 撤销立即清空就绪事实。
	var stored nodeRecord
	if err := env.db.First(&stored, "id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BuildReady || stored.RuntimeReady || stored.IngressReady || stored.MetricsReady || stored.ReadinessReason != "登记资格已撤销，等待重新核验" {
		t.Fatalf("撤销未清空就绪=%#v", stored)
	}
	again, err := env.service.RevokeQualification(ctx, env.root, view.ID)
	if err != nil || again.Status != "REVOKED" || again.RevokedAt == nil {
		t.Fatalf("重复撤销不幂等=%#v,%v", again, err)
	}
	// 撤销后同一qualification_id允许重新登记（安装者重新验收）。
	reissued, err := env.service.RegisterQualification(ctx, env.root, QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Metrics: true})
	if err != nil {
		t.Fatalf("撤销后标识不能复用=%v", err)
	}
	if reissued.ID == view.ID || reissued.Status != "ACTIVE" {
		t.Fatalf("复用登记=%#v", reissued)
	}
	actions := auditActions(env, view.ID)
	if !actions["instance.node.qualification.revoke"] {
		t.Fatalf("缺少撤销审计=%v", actions)
	}
	if auditActions(env, reissued.ID)["instance.node.qualification.revoke"] {
		t.Fatal("新记录不应有撤销审计")
	}
}

func TestQualificationListUsesKeysetPagination(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := env.service.RegisterQualification(ctx, env.root, QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.RevokeQualification(ctx, env.root, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := env.service.RegisterQualification(ctx, env.root, QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-2", Build: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := env.service.ListQualifications(ctx, env.root, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != first.ID || page.NextCursor == "" {
		t.Fatalf("首页=%#v", page)
	}
	next, err := env.service.ListQualifications(ctx, env.root, 1, page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.Items[0].ID != second.ID || next.NextCursor != "" {
		t.Fatalf("次页=%#v", next)
	}
	if _, err := env.service.ListQualifications(ctx, env.root, 0, "bad-cursor"); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("非法游标=%v", err)
	}
	if _, err := env.service.ListQualifications(ctx, env.root, 101, ""); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("非法页大小=%v", err)
	}
}

// 审计失败时登记必须整体回滚，不能留下无审计的资格记录。
func TestQualificationRegisterAuditFailureRollsBack(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.Exec(`ALTER TABLE audit_events ADD CONSTRAINT test_reject_qualification CHECK (action <> 'instance.node.qualification.register')`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.RegisterQualification(ctx, env.root, QualificationInput{NodeID: node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true}); err == nil {
		t.Fatal("审计失败应回滚")
	}
	var count int64
	if err := env.db.Model(&qualificationRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("登记未回滚=%d,%v", count, err)
	}
}
