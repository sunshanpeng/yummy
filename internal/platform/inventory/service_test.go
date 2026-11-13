package inventory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"gorm.io/gorm"
)

type testEnvironment struct {
	db                  *gorm.DB
	service             *Service
	box                 *security.SecretBox
	root, admin, viewer authorization.Principal
}

func setup(t *testing.T) testEnvironment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, value := range []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}, &model.User{ID: "admin", Username: "admin", PasswordHash: "hash"}, &model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash"}, &model.Workspace{ID: "space", Name: "空间", Slug: "space"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "admin", Role: "admin"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: "member"}, &model.Application{ID: "app_123", WorkspaceID: "space", Name: "gateway"}, &model.Application{ID: "app_456", WorkspaceID: "space", Name: "worker"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app_123", UserID: "viewer", Role: "viewer"}} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return testEnvironment{db: db, service: NewService(db, access, box), box: box, root: authorization.Principal{UserID: "root", AuthVersion: 1}, admin: authorization.Principal{UserID: "admin", AuthVersion: 1}, viewer: authorization.Principal{UserID: "viewer", AuthVersion: 1}}
}
func configure(t *testing.T, env testEnvironment) (NodeView, RegistryView, PolicyView) {
	t.Helper()
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := env.service.CreateRegistry(ctx, env.root, RegistryInput{Name: "输出仓库", URL: "https://registry.example.com", RepositoryPrefix: "tenant", Username: "robot", Password: "secret-password"})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := env.service.SavePolicy(ctx, env.root, PolicyInput{BuildNodeID: node.ID, RuntimeNodeID: node.ID, RegistryID: registry.ID, RepositoryPrefix: "apps", BaseDomain: "Apps.Example.COM.", ResourceDefault: Resources{CPUMillis: 500, MemoryMiB: 512}, ResourceLimits: Resources{CPUMillis: 2000, MemoryMiB: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.SetAllocation(ctx, env.root, "space", AllocationInput{AllowedNodeIDs: []string{node.ID}, MaxApplications: 10, BuildConcurrency: 1, CPUMillis: 1000, MemoryMiB: 1024}); err != nil {
		t.Fatal(err)
	}
	return node, registry, policy
}

// 只有测试直接写入资格事实；公开管理 API 不具备这些字段或入口。
func qualify(t *testing.T, env testEnvironment, node NodeView, registry RegistryView) {
	t.Helper()
	now := time.Now().UTC()
	if err := env.db.Model(&nodeRecord{}).Where("id = ?", node.ID).Updates(map[string]any{"cluster_node_uid": "kubernetes-node-uid", "os": "linux", "arch": "amd64", "connected_at": now, "last_seen": now, "build_ready": true, "runtime_ready": true, "ingress_ready": true, "metrics_ready": true, "readiness_reason": "已通过测试资格"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := env.db.Model(&registryRecord{}).Where("id = ?", registry.ID).Updates(map[string]any{"verified_at": now, "verified_revision": registry.Revision}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestInstanceIDIsPersistentAndManagementRejectsWorkspaceAdminAndPAT(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	if _, err := env.service.CreateNode(ctx, env.admin, NodeInput{Name: "节点", WorkerID: "node", Capacity: 1}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间管理员修改实例=%v", err)
	}
	id, err := env.service.InstanceID(ctx, env.root)
	if err != nil {
		t.Fatal(err)
	}
	access, err := authorization.NewAccess(env.db)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewService(env.db, access, env.box)
	next, err := restarted.InstanceID(ctx, env.root)
	if err != nil || next != id {
		t.Fatalf("实例ID重启变化=%s,%s,%v", id, next, err)
	}
	token := model.AccessToken{ID: "pat", UserID: "root", Name: "限定凭证", TokenHash: "token-digest", Scopes: []string{"application:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := env.db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	bearer := authorization.Principal{UserID: "root", AuthVersion: 1, TokenID: "pat"}
	if _, err := env.service.ListNodes(ctx, bearer, 20, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT继承实例管理=%v", err)
	}
	if _, err := env.service.InstanceID(ctx, env.admin); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal(err)
	}
}

func TestNodeRegistrationCannotForgeReadinessAndCASInvalidatesQualifications(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, registry, _ := configure(t, env)
	if node.BuildReady || node.RuntimeReady || node.LastSeen != nil || node.ClusterNodeUID != "" {
		t.Fatalf("登记不能伪造就绪=%#v", node)
	}
	if _, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "重复", WorkerID: node.WorkerID, Capacity: 1}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("重复worker=%v", err)
	}
	if _, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "错误", WorkerID: "invalid/worker", Capacity: 0}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal(err)
	}
	qualify(t, env, node, registry)
	updated, err := env.service.UpdateNode(ctx, env.root, node.ID, UpdateNodeInput{Name: "更名", Status: "DRAINING", Capacity: 2, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.BuildReady || updated.RuntimeReady || updated.Status != "DRAINING" {
		t.Fatalf("变更未撤销资格=%#v", updated)
	}
	if _, err := env.service.UpdateNode(ctx, env.root, node.ID, UpdateNodeInput{Name: "过期", Status: "ACTIVE", Capacity: 1, ExpectedRevision: 1}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
}

func TestRegistryCredentialsAreEncryptedVersionedAndNeverReturned(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	_, registry, _ := configure(t, env)
	for _, address := range []string{"http://registry.example.com", "https://user:password@registry.example.com", "https://registry.example.com/v2/", "https://registry.example.com?secret=1"} {
		if _, err := env.service.CreateRegistry(ctx, env.root, RegistryInput{Name: "错误", URL: address}); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("非法registry URL %q = %v", address, err)
		}
	}
	var stored registryRecord
	if err := env.db.First(&stored, "id = ?", registry.ID).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.EncryptedPassword, []byte("secret-password")) {
		t.Fatal("数据库包含明文")
	}
	plaintext, err := env.box.Decrypt(stored.EncryptedPassword, credentialIdentity(stored.InstanceID, stored.ID, 1))
	if err != nil || string(plaintext) != "secret-password" {
		t.Fatalf("密文=%q,%v", plaintext, err)
	}
	if _, err := env.box.Decrypt(stored.EncryptedPassword, credentialIdentity(stored.InstanceID, stored.ID, 2)); err == nil {
		t.Fatal("跨版本密文应认证失败")
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "password") || strings.Contains(string(data), "robot") || strings.Contains(string(data), "encrypted") {
		t.Fatalf("DTO泄漏凭据=%s", data)
	}
	username, password := "robot-v2", "second-password"
	updated, err := env.service.UpdateRegistry(ctx, env.root, registry.ID, UpdateRegistryInput{Name: "输出", URL: registry.URL, RepositoryPrefix: registry.RepositoryPrefix, Status: "ACTIVE", ExpectedRevision: 1, Username: &username, Password: &password})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CredentialVersion != 2 || updated.VerifiedAt != nil {
		t.Fatal(updated)
	}
	var versions []credentialRecord
	if err := env.db.Where("registry_id = ?", registry.ID).Order("version ASC").Find(&versions).Error; err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("凭据历史=%#v", versions)
	}
	unchanged, err := env.service.UpdateRegistry(ctx, env.root, registry.ID, UpdateRegistryInput{Name: "只改名", URL: registry.URL, RepositoryPrefix: registry.RepositoryPrefix, Status: "ACTIVE", ExpectedRevision: 2})
	if err != nil || unchanged.CredentialVersion != 2 {
		t.Fatalf("省略替换应保留旧值=%#v,%v", unchanged, err)
	}
	var audits []model.AuditEvent
	if err := env.db.Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	auditJSON, _ := json.Marshal(audits)
	if strings.Contains(string(auditJSON), "secret-password") || strings.Contains(string(auditJSON), "second-password") {
		t.Fatal("审计泄漏凭据")
	}
}

func TestPolicyIsImmutableVersionedAndAllocationUsesSameInstanceFK(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, registry, first := configure(t, env)
	if first.Revision != 1 || first.BaseDomain != "apps.example.com" || first.AutoRecoveryEnabled {
		t.Fatal(first)
	}
	input := PolicyInput{BuildNodeID: node.ID, RuntimeNodeID: node.ID, RegistryID: registry.ID, RepositoryPrefix: "new", BaseDomain: "apps.example.com", ResourceDefault: Resources{CPUMillis: 500, MemoryMiB: 512}, ResourceLimits: Resources{CPUMillis: 1000, MemoryMiB: 1024}, ExpectedRevision: 1}
	if _, err := env.service.SavePolicy(ctx, env.admin, input); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间修改策略=%v", err)
	}
	second, err := env.service.SavePolicy(ctx, env.root, input)
	if err != nil || second.Revision != 2 {
		t.Fatalf("新策略=%#v,%v", second, err)
	}
	if _, err := env.service.SavePolicy(ctx, env.root, input); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("策略CAS=%v", err)
	}
	old, err := env.service.GetPolicy(ctx, env.root, 1)
	if err != nil || old.RepositoryPrefix != "apps" {
		t.Fatalf("历史被改写=%#v,%v", old, err)
	}
	allocation, err := env.service.GetAllocation(ctx, env.root, "space")
	if err != nil || len(allocation.AllowedNodeIDs) != 1 {
		t.Fatal(allocation, err)
	}
	if _, err := env.service.SetAllocation(ctx, env.root, "space", AllocationInput{AllowedNodeIDs: []string{"missing"}, MaxApplications: 1, BuildConcurrency: 1, CPUMillis: 1, MemoryMiB: 1, ExpectedRevision: 1}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("不存在节点分配=%v", err)
	}
	if _, err := env.service.SetAllocation(ctx, env.admin, "space", AllocationInput{AllowedNodeIDs: []string{node.ID}, MaxApplications: 1, BuildConcurrency: 1, CPUMillis: 1, MemoryMiB: 1, ExpectedRevision: 1}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间修改额度=%v", err)
	}
	if err := env.db.Create(&allocationNodeRecord{WorkspaceID: "space", InstanceID: first.InstanceID, NodeID: "missing"}).Error; err == nil {
		t.Fatal("关系表未启用外键")
	}
}

func TestResolveRequiresRealReadinessAndPreservesImmutableApplicationIdentity(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, registry, policy := configure(t, env)
	summary, err := env.service.ResolveBuild(ctx, env.viewer, "space", "app_123", policy.Revision)
	if !errors.Is(err, ErrNotReady) || summary.Ready {
		t.Fatalf("登记不能构建=%#v,%v", summary, err)
	}
	qualify(t, env, node, registry)
	summary, err = env.service.ResolveBuild(ctx, env.viewer, "space", "app_123", policy.Revision)
	if err != nil || !summary.Ready || summary.Platform != "linux/amd64" {
		t.Fatalf("构建摘要=%#v,%v", summary, err)
	}
	if summary.ImageRepository != "registry.example.com/tenant/apps/app_123" || summary.DefaultDomain != "app-123.apps.example.com" {
		t.Fatalf("系统身份=%#v", summary)
	}
	if summary.ResourceLimits.CPUMillis != 1000 || summary.ResourceLimits.MemoryMiB != 1024 {
		t.Fatalf("额度未收紧=%#v", summary)
	}
	if err := env.db.Model(&model.Application{}).Where("id = ?", "app_123").Update("name", "renamed").Error; err != nil {
		t.Fatal(err)
	}
	renamed, err := env.service.ResolveBuild(ctx, env.viewer, "space", "app_123", policy.Revision)
	if err != nil || renamed.ImageRepository != summary.ImageRepository {
		t.Fatalf("更名改变repository=%#v,%v", renamed, err)
	}
	if _, err := env.service.ResolveBuild(ctx, env.viewer, "space", "app_456", policy.Revision); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("越权解析=%v", err)
	}
	other, err := env.service.ResolveBuild(ctx, env.root, "space", "app_456", policy.Revision)
	if err != nil || other.ImageRepository == summary.ImageRepository {
		t.Fatalf("应用共用repository=%#v,%v", other, err)
	}
	if _, err := env.service.ResolveRuntime(ctx, env.viewer, "space", "app_123", policy.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.ResolveBuild(ctx, env.viewer, "space", "app_123", policy.Revision+1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("预览策略过期=%v", err)
	}
	if err := env.db.Model(&nodeRecord{}).Where("id = ?", node.ID).Update("last_seen", time.Now().Add(-5*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.ResolveRuntime(ctx, env.viewer, "space", "app_123", policy.Revision); !errors.Is(err, ErrNotReady) {
		t.Fatalf("过期在线=%v", err)
	}
}

func TestCredentialUpdateRevokesVerificationAndAuditFailureRollsBack(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, registry, policy := configure(t, env)
	qualify(t, env, node, registry)
	if err := env.db.Exec(`ALTER TABLE audit_events ADD CONSTRAINT test_reject_registry_update CHECK (action <> 'instance.registry.update')`).Error; err != nil {
		t.Fatal(err)
	}
	username, password := "new", "password"
	input := UpdateRegistryInput{Name: registry.Name, URL: registry.URL, RepositoryPrefix: registry.RepositoryPrefix, Status: "ACTIVE", ExpectedRevision: 1, Username: &username, Password: &password}
	if _, err := env.service.UpdateRegistry(ctx, env.root, registry.ID, input); err == nil {
		t.Fatal("审计失败应回滚")
	}
	var count int64
	if err := env.db.Model(&credentialRecord{}).Where("registry_id = ?", registry.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("凭据版本未回滚=%d,%v", count, err)
	}
	if err := env.db.Exec(`ALTER TABLE audit_events DROP CONSTRAINT test_reject_registry_update`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.UpdateRegistry(ctx, env.root, registry.ID, input); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.ResolveBuild(ctx, env.root, "space", "app_123", policy.Revision); !errors.Is(err, ErrNotReady) {
		t.Fatalf("旧验证不能覆盖新口令=%v", err)
	}
}
