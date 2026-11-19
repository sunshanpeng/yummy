package deployment

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
)

// funcProber 把函数适配为 Prober，ObservedAt 在探测时动态取当前时间，保证晚于最近一次 Kubernetes 观察。
type funcProber func(ctx context.Context, domain, path string) (ProbeResult, error)

func (f funcProber) Probe(ctx context.Context, domain, path string) (ProbeResult, error) {
	return f(ctx, domain, path)
}

func healthyProber() Prober {
	return funcProber(func(context.Context, string, string) (ProbeResult, error) {
		return ProbeResult{TLSVerified: true, Healthy: true, ObservedAt: time.Now().UTC()}, nil
	})
}

// markAutoDeploy 直接把测试构建改成自动发布形态；CHECK 约束要求 auto_deploy 与 runtime_config_revision 同现。
func markAutoDeploy(t *testing.T, e environment, app, expectedServing string) {
	t.Helper()
	mustExec(t, e.db, "UPDATE builds SET auto_deploy=TRUE, runtime_config_revision=1, expected_serving_release=? WHERE application_id=?", expectedServing, app)
}

func countRows(t *testing.T, e environment, model any, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := e.db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAutoPublishSkipsUnverifiedOrManualBuilds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// 默认 auto_deploy=FALSE：不发布。
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 0 {
		t.Fatalf("auto_deploy=false 被发布:%d %v", published, err)
	}
	// 产物仍在 VERIFYING（真实流程中构建事件先于产物验证到达）：不发布。
	markAutoDeploy(t, e, "app_two", "")
	mustExec(t, e.db, "UPDATE build_artifacts SET state='VERIFYING', verified_at=NULL WHERE id='artifact_app_two'")
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 0 {
		t.Fatalf("VERIFYING 产物被发布:%d %v", published, err)
	}
	// 构建未到 SUCCEEDED（RUNNING/VERIFYING_ARTIFACT 中间态）：即使产物已 VERIFIED 也不发布。
	mustExec(t, e.db, "UPDATE build_artifacts SET state='VERIFIED', verified_at=NOW() WHERE id='artifact_app_two'")
	mustExec(t, e.db, "UPDATE builds SET state='RUNNING', stage='VERIFYING_ARTIFACT' WHERE application_id='app_two'")
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 0 {
		t.Fatalf("未成功构建被发布:%d %v", published, err)
	}
	if count := countRows(t, e, &releaseRecord{}, "workspace_id = ?", "space"); count != 0 {
		t.Fatal("不应产生任何 release")
	}
}

func TestAutoPublishCreatesReleaseAndOperationThenIdempotent(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	markAutoDeploy(t, e, "app_two", "")
	published, err := e.service.AutoPublish(ctx, 10)
	if err != nil || published != 1 {
		t.Fatalf("自动发布受理失败:%d %v", published, err)
	}
	var build struct{ ID string }
	if err := e.db.Table("builds").Select("id").Where("application_id = 'app_two'").Take(&build).Error; err != nil {
		t.Fatal(err)
	}
	var release releaseRecord
	if err := e.db.First(&release, "application_id = 'app_two'").Error; err != nil {
		t.Fatal(err)
	}
	if release.ArtifactID != "artifact_app_two" || release.RuntimeConfigRevision != 1 || release.Source != Deploy || release.ActorID != "root" {
		t.Fatalf("release 绑定错误:%#v", release)
	}
	var op operationRecord
	if err := e.db.First(&op, "application_id = 'app_two'").Error; err != nil {
		t.Fatal(err)
	}
	if op.Action != Deploy || op.State != Queued || !op.Active || op.ActorID != "root" || op.IdempotencyKey != "autodeploy:"+build.ID {
		t.Fatalf("operation 受理错误:%#v", op)
	}
	// 重复扫描幂等：不再产生第二个 release/operation，也不再计数。
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 0 {
		t.Fatalf("重复扫描非幂等:%d %v", published, err)
	}
	if count := countRows(t, e, &releaseRecord{}, "application_id = 'app_two'"); count != 1 {
		t.Fatal("幂等扫描产生多个 release")
	}
	if count := countRows(t, e, &operationRecord{}, "application_id = 'app_two'"); count != 1 {
		t.Fatal("幂等扫描产生多个 operation")
	}
}

func TestAutoPublishConcurrentRunsProduceSingleRelease(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	markAutoDeploy(t, e, "app_two", "")
	const n = 4
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 并发竞争下的冲突/重复都属于预期跳过，不应返回错误。
			if _, err := e.service.AutoPublish(ctx, 10); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count := countRows(t, e, &releaseRecord{}, "application_id = 'app_two'"); count != 1 {
		t.Fatal("并发自动发布产生多个 release")
	}
	if count := countRows(t, e, &operationRecord{}, "application_id = 'app_two' AND idempotency_key LIKE 'autodeploy:%'"); count != 1 {
		t.Fatal("并发自动发布产生多个 operation")
	}
}

// insertAutoDeployBuild 为指定应用注入第二个成功构建及其 VERIFIED 产物，
// 用于构造“产物从未发布但 serving 期望不匹配”的场景。
func insertAutoDeployBuild(t *testing.T, e environment, app, expectedServing string) string {
	t.Helper()
	var settings settingsRecord
	if err := e.db.First(&settings, "singleton = TRUE").Error; err != nil {
		t.Fatal(err)
	}
	buildID := "bld_extra_" + app
	artifactID := "artifact_extra_" + app
	mustExec(t, e.db, `INSERT INTO builds(id,workspace_id,application_id,actor_id,actor_auth_version,idempotency_key,fingerprint,source_snapshot_id,source_sha256,config_revision,config_snapshot,instance_id,policy_revision,node_id,registry_id,credential_version,repository,tag,platform,execution_snapshot,auto_deploy,runtime_config_revision,expected_serving_release,state,desired_state,deadline_at) VALUES (?,'space',?,'root',1,?,?,?, ?,0,'{}'::jsonb,?,1,?,?,1,?,?, 'linux/amd64','{}'::jsonb,TRUE,1,?,'SUCCEEDED','RUN',NOW()+INTERVAL '1 hour')`,
		buildID, app, "extra-"+app, strings.Repeat("c", 64), "src_"+app, strings.Repeat("d", 64), settings.ID, e.node.ID, e.registry.ID, "registry.example.com/tenant/"+app+"-extra", buildID, expectedServing)
	mustExec(t, e.db, `INSERT INTO build_attempts(id,workspace_id,application_id,build_id,node_id,worker_id,job_name,occupied,stopped_at) VALUES (?,'space',?,?,?,?,?,FALSE,NOW())`, "attempt_extra_"+app, app, buildID, e.node.ID, e.node.WorkerID, "job-extra-"+app)
	mustExec(t, e.db, `INSERT INTO build_artifacts(id,workspace_id,application_id,build_id,attempt_id,registry_id,credential_version,repository,tag,digest,platform,state,verified_at) VALUES (?,'space',?,?,?,?,1,?,?,?,'linux/amd64','VERIFIED',NOW())`, artifactID, app, buildID, "attempt_extra_"+app, e.registry.ID, "registry.example.com/tenant/"+app+"-extra", buildID, "sha256:"+strings.Repeat("e", 64))
	return artifactID
}

func TestAutoPublishExpectedServingMismatchSkipsWithoutRollback(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	serving := create(t, e, "one")
	finishHTTP(t, e, serving)
	// 另一个自动发布构建期望的 serving 与当前不一致：跳过并审计，绝不把应用回滚到该构建产物。
	artifact := insertAutoDeployBuild(t, e, "app_one", "rel_not_current")
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 0 {
		t.Fatalf("serving 期望不匹配仍发布:%d %v", published, err)
	}
	v := status(t, e)
	if v.ServingReleaseID != serving.Release.ID || v.Generation != 1 {
		t.Fatalf("跳过时改变了当前服务:%#v", v)
	}
	if count := countRows(t, e, &model.AuditEvent{}, "action = ? AND object_type = 'build'", "application.deployment.autodeploy.skip.serving_mismatch"); count != 1 {
		t.Fatal("未审计 serving 不匹配原因")
	}
	// 期望修正为当前 serving 后正常发布，且记录前一个 serving。
	mustExec(t, e.db, "UPDATE builds SET expected_serving_release=? WHERE id='bld_extra_app_one'", serving.Release.ID)
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 1 {
		t.Fatalf("serving 期望匹配未发布:%d %v", published, err)
	}
	var latest releaseRecord
	if err := e.db.Order("created_at DESC").First(&latest, "application_id = 'app_one'").Error; err != nil {
		t.Fatal(err)
	}
	if latest.ArtifactID != artifact || text(latest.PreviousServingID) != serving.Release.ID {
		t.Fatalf("自动发布未正确衔接 serving:%#v", latest)
	}
	if v := status(t, e); v.Generation != 2 || v.ServingReleaseID != serving.Release.ID {
		t.Fatalf("受理冒充成功:%#v", v)
	}
}

func TestAutoPublishSkipsWhenFrozenConfigSuperseded(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	markAutoDeploy(t, e, "app_two", "")
	cfg := runtimeconfig.DefaultInput()
	cfg.Confirmed = true
	cfg.ExpectedRevision = 1
	if _, err := e.configs.Save(ctx, e.root, "space", "app_two", cfg); err != nil {
		t.Fatal(err)
	}
	if published, err := e.service.AutoPublish(ctx, 10); err != nil || published != 0 {
		t.Fatalf("冻结配置被新版本取代仍发布:%d %v", published, err)
	}
	if count := countRows(t, e, &releaseRecord{}, "application_id = 'app_two'"); count != 0 {
		t.Fatal("不应产生 release")
	}
	if count := countRows(t, e, &model.AuditEvent{}, "action = ?", "application.deployment.autodeploy.skip.stale_config"); count != 1 {
		t.Fatal("未审计配置前移原因")
	}
}

func TestAutoPublishLimitInvalid(t *testing.T) {
	e := setup(t)
	if _, err := e.service.AutoPublish(context.Background(), 0); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal("非法 limit 未拒绝")
	}
	if err := e.service.ProbeOnce(context.Background(), nil, 10); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal("空探测器未拒绝")
	}
}

func TestProbeOnceDrivesVerifyingIngressToServing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := create(t, e, "one")
	if _, err := e.service.ApplyObservation(ctx, e.worker, readyEvent(r, 1)); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ObservedState != "PENDING" || v.ServingReleaseID != "" {
		t.Fatalf("Kubernetes 就绪不应直接成功:%#v", v)
	}
	if err := e.service.ProbeOnce(ctx, healthyProber(), 10); err != nil {
		t.Fatal(err)
	}
	v := status(t, e)
	if v.ServingReleaseID != r.Release.ID || v.ObservedState != "RUNNING" {
		t.Fatalf("完整探测证据未收敛:%#v", v)
	}
	history, err := e.service.Get(ctx, e.root, "space", "app_one", r.Operation.ID)
	if err != nil || history.Operation.State != Succeeded || history.Operation.Active {
		t.Fatalf("入口探测未完成操作:%#v %v", history, err)
	}
}

func TestProbeOnceUnverifiedTLSOrUnhealthyNeverSucceeds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := create(t, e, "one")
	if _, err := e.service.ApplyObservation(ctx, e.worker, readyEvent(r, 1)); err != nil {
		t.Fatal(err)
	}
	for _, broken := range []ProbeResult{
		{TLSVerified: false, Healthy: true},
		{TLSVerified: true, Healthy: false},
	} {
		broken := broken
		if err := e.service.ProbeOnce(ctx, funcProber(func(context.Context, string, string) (ProbeResult, error) {
			return ProbeResult{TLSVerified: broken.TLSVerified, Healthy: broken.Healthy, ObservedAt: time.Now().UTC()}, nil
		}), 10); err != nil {
			t.Fatal(err)
		}
		if v := status(t, e); v.ServingReleaseID != "" {
			t.Fatalf("不完整证据冒充成功:%#v", broken)
		}
		history, err := e.service.Get(ctx, e.root, "space", "app_one", r.Operation.ID)
		if err != nil || history.Operation.State != VerifyingIngress {
			t.Fatalf("不完整证据改变操作状态:%#v %v", history, err)
		}
	}
	// 证据补齐后仍可收敛。
	if err := e.service.ProbeOnce(ctx, healthyProber(), 10); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ServingReleaseID != r.Release.ID {
		t.Fatalf("补齐证据未成功:%#v", v)
	}
}

func TestProbeOnceContinuesAfterStaleTarget(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first := create(t, e, "one")
	second, err := e.service.CreateDeployment(ctx, e.root, "space", "app_two", CreateInput{ArtifactID: "artifact_app_two", RuntimeConfigRevision: 1, ExpectedPolicyRevision: 1, Expected: Expected{ConfigRevision: 1}, IdempotencyKey: "two"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		app string
		r   Result
	}{{"app_one", first}, {"app_two", second}} {
		event := readyEvent(item.r, 1)
		event.ApplicationID = item.app
		if _, err := e.service.ApplyObservation(ctx, e.worker, event); err != nil {
			t.Fatal(err)
		}
	}
	// 让 app_one 的观察过期：ApplyProbe 应拒绝，但同轮 app_two 仍被探测并收敛。
	mustExec(t, e.db, "UPDATE deployment_operations SET last_observed_at = NOW() - INTERVAL '5 minutes' WHERE id=?", first.Operation.ID)
	err = e.service.ProbeOnce(ctx, healthyProber(), 10)
	if !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("过期观察未按陈旧拒绝:%v", err)
	}
	other, err := e.service.GetStatus(ctx, e.root, "space", "app_two")
	if err != nil || other.ServingReleaseID != second.Release.ID {
		t.Fatalf("单目标失败中断整轮:%#v %v", other, err)
	}
	if v := status(t, e); v.ServingReleaseID != "" {
		t.Fatalf("过期目标不应成功:%#v", v)
	}
}

func TestSweepMarksStaleObservationsUnknown(t *testing.T) {
	e := setup(t)
	r := create(t, e, "one")
	finishHTTP(t, e, r)
	var before stateRecord
	if err := e.db.First(&before, "application_id = 'app_one'").Error; err != nil {
		t.Fatal(err)
	}
	if before.ObservedState != "RUNNING" {
		t.Fatalf("前置状态错误:%#v", before)
	}
	mustExec(t, e.db, "UPDATE application_runtime_states SET last_observed_at = NOW() - INTERVAL '10 minutes' WHERE application_id='app_one'")
	if err := e.service.Sweep(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var after stateRecord
	if err := e.db.First(&after, "application_id = 'app_one'").Error; err != nil {
		t.Fatal(err)
	}
	if after.ObservedState != Unknown {
		t.Fatalf("过期观察未置 UNKNOWN:%#v", after)
	}
	// 历史服务事实不被收敛抹除。
	if text(after.ServingReleaseID) != r.Release.ID {
		t.Fatalf("收敛抹除历史服务事实:%#v", after)
	}
}

func TestHTTPSProberTLSRedirectAndHealthSemantics(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/moved":
			// 跟随重定向到 /health 会得到 2xx；探测器必须禁止跟随。
			http.Redirect(w, r, "/health", http.StatusFound)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	host := strings.TrimPrefix(srv.URL, "https://")
	// 注入客户端只扩展受信根（InsecureSkipVerify 保持 false），验证语义仍由探测器执行。
	prober := HTTPSProber{Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}}
	ctx := context.Background()
	result, err := prober.Probe(ctx, host, "/health")
	if err != nil || !result.TLSVerified || !result.Healthy {
		t.Fatalf("2xx 健康探测失败:%#v %v", result, err)
	}
	if result.ObservedAt.IsZero() || time.Since(result.ObservedAt) > time.Minute {
		t.Fatalf("ObservedAt 非探测完成时刻:%#v", result)
	}
	result, err = prober.Probe(ctx, host, "/moved")
	if err != nil || !result.TLSVerified || result.Healthy {
		t.Fatalf("重定向被跟随或误判健康:%#v %v", result, err)
	}
	result, err = prober.Probe(ctx, host, "/unavailable")
	if err != nil || !result.TLSVerified || result.Healthy {
		t.Fatalf("非 2xx 误判健康:%#v %v", result, err)
	}
	// 默认客户端不信任测试证书：TLS 验证失败必须返回错误而非伪造证据。
	if _, err := (HTTPSProber{}).Probe(ctx, host, "/health"); err == nil {
		t.Fatal("默认根证书未拒绝自签证书")
	}
}
