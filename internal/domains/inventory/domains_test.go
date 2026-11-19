package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
)

// claimFixture 固定一个已受理的 HTTP placement 与两条域名声明，
// 模拟发布受理后 application_domain_claims 的占用事实。
func claimFixture(t *testing.T, env testEnvironment) {
	t.Helper()
	ctx := context.Background()
	instanceID, err := env.service.InstanceID(ctx, env.root)
	if err != nil {
		t.Fatal(err)
	}
	node, _, _ := configure(t, env)
	if err := env.db.Exec(`INSERT INTO application_placements (id, application_id, workspace_id, instance_id, node_id, worker_id, cluster_node_uid, policy_revision, runtime_type, default_domain) VALUES ('plc-app_123','app_123','space',?,?,'worker-1','uid-a',1,'HTTP','app.apps.example.com')`, instanceID, node.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"app.example.com", "extra.example.com"} {
		if err := env.db.Exec(`INSERT INTO application_domain_claims (id, domain, workspace_id, application_id, is_default) VALUES (CONCAT('dcl-', ?::text), ?, 'space','app_123',FALSE)`, domain, domain).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// 域名证书绑定属实例级平台管理；空间管理员、普通成员和PAT都不能代替全局管理员改证书事实。
func TestDomainTLSSetRequiresInstanceAdmin(t *testing.T) {
	env := setup(t)
	claimFixture(t, env)
	ctx := context.Background()
	if _, err := env.service.SetDomainTLSSecret(ctx, env.admin, "app.example.com", "wildcard-example"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间管理员绑定证书=%v", err)
	}
	if _, err := env.service.SetDomainTLSSecret(ctx, env.viewer, "app.example.com", "wildcard-example"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员绑定证书=%v", err)
	}
	if _, err := env.service.GetDomainTLSSecret(ctx, env.viewer, "app.example.com"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员读取绑定=%v", err)
	}
	token := model.AccessToken{ID: "pat", UserID: "root", Name: "限定凭证", TokenHash: "token-digest", Scopes: []string{"application:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := env.db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	bearer := authorization.Principal{UserID: "root", AuthVersion: 1, TokenID: "pat"}
	if _, err := env.service.SetDomainTLSSecret(ctx, bearer, "app.example.com", ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT继承实例证书管理=%v", err)
	}
}

func TestDomainTLSSetValidatesClaimAndSecret(t *testing.T) {
	env := setup(t)
	claimFixture(t, env)
	ctx := context.Background()
	// 域声明必须已存在；未占用域名失败关闭，不预登记未来域名。
	if _, err := env.service.SetDomainTLSSecret(ctx, env.root, "ghost.example.com", "wildcard-example"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("未占用域名绑定=%v", err)
	}
	if _, err := env.service.GetDomainTLSSecret(ctx, env.root, "ghost.example.com"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("未占用域名查询=%v", err)
	}
	// Secret 名与 kube.Runtime 装配 Ingress.Spec.TLS 的 DNS1123 子域规则一致。
	for _, bad := range []string{"带 空格", "UPPER-CASE", "secret/name", "secret_name", "-leading", "-"} {
		if _, err := env.service.SetDomainTLSSecret(ctx, env.root, "app.example.com", bad); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("非法Secret名 %q = %v", bad, err)
		}
	}
	// 绑定成功并落审计。
	bound, err := env.service.SetDomainTLSSecret(ctx, env.root, "app.example.com", "wildcard-example")
	if err != nil {
		t.Fatal(err)
	}
	if bound.Domain != "app.example.com" || bound.SecretName != "wildcard-example" || bound.WorkspaceID != "space" {
		t.Fatalf("绑定视图=%#v", bound)
	}
	// 空格自动修剪，重复设置覆盖旧值。
	updated, err := env.service.SetDomainTLSSecret(ctx, env.root, "  app.example.com  ", " rotated-secret ")
	if err != nil || updated.SecretName != "rotated-secret" {
		t.Fatalf("覆盖绑定=%#v,%v", updated, err)
	}
	// Get 只读返回当前绑定。
	got, err := env.service.GetDomainTLSSecret(ctx, env.root, "app.example.com")
	if err != nil || got.SecretName != "rotated-secret" {
		t.Fatalf("读取绑定=%#v,%v", got, err)
	}
	// 未绑定域名返回空 Secret，不报错。
	other, err := env.service.GetDomainTLSSecret(ctx, env.root, "extra.example.com")
	if err != nil || other.SecretName != "" {
		t.Fatalf("未绑定读取=%#v,%v", other, err)
	}
	actions := auditActions(env, "app.example.com")
	if !actions["instance.domain.tls.set"] {
		t.Fatalf("缺少绑定审计=%v", actions)
	}
}

// 解除绑定幂等：空串重复提交仍成功且视图为空；只清绑定列，不动域名占用与 Secret 本身。
func TestDomainTLSUnbindIsIdempotent(t *testing.T) {
	env := setup(t)
	claimFixture(t, env)
	ctx := context.Background()
	if _, err := env.service.SetDomainTLSSecret(ctx, env.root, "app.example.com", "wildcard-example"); err != nil {
		t.Fatal(err)
	}
	first, err := env.service.SetDomainTLSSecret(ctx, env.root, "app.example.com", "")
	if err != nil || first.SecretName != "" {
		t.Fatalf("解除绑定=%#v,%v", first, err)
	}
	again, err := env.service.SetDomainTLSSecret(ctx, env.root, "app.example.com", "")
	if err != nil || again.SecretName != "" || again.Domain != "app.example.com" {
		t.Fatalf("重复解除不幂等=%#v,%v", again, err)
	}
	var stored domainClaimRecord
	if err := env.db.First(&stored, "domain = ?", "app.example.com").Error; err != nil {
		t.Fatal(err)
	}
	if stored.TLSSecretName != "" || stored.WorkspaceID != "space" || stored.ApplicationID != "app_123" {
		t.Fatalf("解除后声明被破坏=%#v", stored)
	}
}
