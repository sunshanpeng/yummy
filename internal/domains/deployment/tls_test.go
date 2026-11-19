package deployment

import (
	"context"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domains/runtimeconfig"
)

// claimTLSRelease 构造带两条自定义域名的 HTTP 发布：配置另存 revision 2 后受理，
// release.Domains 冻结为配置域名加默认域名；绑定由后续 SQL fixture 直写。
func claimTLSRelease(t *testing.T, e environment) Result {
	t.Helper()
	cfg := runtimeconfig.DefaultInput()
	cfg.Confirmed = true
	cfg.ExpectedRevision = 1
	cfg.Domains = []string{"app.example.com", "extra.example.com"}
	if _, err := e.configs.Save(context.Background(), e.root, "space", "app_one", cfg); err != nil {
		t.Fatal(err)
	}
	in := createInput("tls")
	in.RuntimeConfigRevision = 2
	in.Expected = Expected{ConfigRevision: 2}
	r, err := e.service.CreateDeployment(context.Background(), e.root, "space", "app_one", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Release.Domains) < 2 {
		t.Fatalf("发布必须冻结多条域名=%v", r.Release.Domains)
	}
	return r
}

func bindAll(t *testing.T, e environment, secret string) {
	t.Helper()
	if err := e.db.Exec("UPDATE application_domain_claims SET tls_secret_name=? WHERE workspace_id='space' AND application_id='app_one'", secret).Error; err != nil {
		t.Fatal(err)
	}
}

// 全部域名绑定同一非空 Secret 时，领取下发 HTTPS 与该 Secret；绑定取当下值，
// 发布创建时未绑定、领取前补绑同样生效——绑定是实例级运行设施，不冻结进发布。
func TestClaimPendingConvergesTLSSameBinding(t *testing.T) {
	e := setup(t)
	r := claimTLSRelease(t, e)
	bindAll(t, e, "wildcard-example")
	dispatch, err := e.service.ClaimPending(context.Background(), e.worker)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.TLS == nil || !dispatch.TLS.HTTPS || dispatch.TLS.SecretName != "wildcard-example" {
		t.Fatalf("同绑必须汇聚TLS=%#v", dispatch.TLS)
	}
	// 默认域名也在冻结域名列表内，同样要求绑定；此处 SQL 已覆盖全部 claim 行。
	if len(dispatch.Domains) != len(r.Release.Domains) {
		t.Fatalf("冻结域名不完整=%v", dispatch.Domains)
	}
}

// 任一域名未绑定即失败关闭：TLS=nil，入口探测将因缺少 TLS 证据无法收敛，不猜证书。
func TestClaimPendingMissingBindingDropsTLS(t *testing.T) {
	e := setup(t)
	r := claimTLSRelease(t, e)
	bindAll(t, e, "wildcard-example")
	// 只保留一条绑定，其余域名（含默认域名）保持未绑定。
	if err := e.db.Exec("UPDATE application_domain_claims SET tls_secret_name='' WHERE workspace_id='space' AND application_id='app_one' AND domain <> ?", r.Release.Domains[0]).Error; err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.service.ClaimPending(context.Background(), e.worker)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.TLS != nil {
		t.Fatalf("缺绑必须丢弃TLS=%#v", dispatch.TLS)
	}
}

// 绑定不一致同样失败关闭；不同 Secret 之间存在无法安全合并的歧义。
func TestClaimPendingInconsistentBindingDropsTLS(t *testing.T) {
	e := setup(t)
	r := claimTLSRelease(t, e)
	for i, domain := range r.Release.Domains {
		secret := "wildcard-example"
		if i%2 == 1 {
			secret = "other-secret"
		}
		if err := e.db.Exec("UPDATE application_domain_claims SET tls_secret_name=? WHERE domain=?", secret, domain).Error; err != nil {
			t.Fatal(err)
		}
	}
	dispatch, err := e.service.ClaimPending(context.Background(), e.worker)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.TLS != nil {
		t.Fatalf("不一致绑定必须丢弃TLS=%#v", dispatch.TLS)
	}
}

// 无域名发布（后台进程不配置入口）不携带 TLS；直接验证汇聚函数语义。
// 注：WORKER 类型走完整 CreateDeployment 落库会触发 application_releases.domains
// 非空约束（既有边界，修复属 service.go，超出本次授权修改范围），故此处单元验证。
func TestConvergeTLSWithNoDomainsIsNil(t *testing.T) {
	e := setup(t)
	tls, err := convergeTLS(e.db, nil)
	if err != nil || tls != nil {
		t.Fatalf("空域名必须不携带TLS=%#v,%v", tls, err)
	}
}
