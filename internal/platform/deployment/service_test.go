package deployment

import (
	"regexp"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"gorm.io/gorm"
)

type verifierFunc func(context.Context, ArtifactSnapshot) error

func (f verifierFunc) Verify(ctx context.Context, a ArtifactSnapshot) error { return f(ctx, a) }

type environment struct {
	db        *gorm.DB
	service   *Service
	configs   *runtimeconfig.Service
	inventory *inventory.Service
	root      authorization.Principal
	worker    WorkerIdentity
	node      inventory.NodeView
	registry  inventory.RegistryView
	artifact  string
}

func mustExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatal(err)
	}
}
func setup(t *testing.T) environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, v := range []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}, &model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash"}, &model.Workspace{ID: "space", Name: "空间", Slug: "space"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: "member"}, &model.Application{ID: "app_one", WorkspaceID: "space", Name: "one", AddressSuffix: "axk101"}, &model.Application{ID: "app_two", WorkspaceID: "space", Name: "two", AddressSuffix: "bxk202"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app_one", UserID: "viewer", Role: "viewer"}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	e := environment{db: db, inventory: inventory.NewService(db, access, box), configs: runtimeconfig.NewService(db, access, box), root: authorization.Principal{UserID: "root", AuthVersion: 1}}
	e.service = NewService(db, access, e.configs, verifierFunc(func(context.Context, ArtifactSnapshot) error { return nil }))
	ctx := context.Background()
	e.node, err = e.inventory.CreateNode(ctx, e.root, inventory.NodeInput{Name: "节点", WorkerID: "worker-one", Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.registry, err = e.inventory.CreateRegistry(ctx, e.root, inventory.RegistryInput{Name: "仓库", URL: "https://registry.example.com", RepositoryPrefix: "tenant", Username: "robot", Password: "registry-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.inventory.SavePolicy(ctx, e.root, inventory.PolicyInput{BuildNodeID: e.node.ID, RuntimeNodeID: e.node.ID, RegistryID: e.registry.ID, BaseDomain: "apps.example.com", ResourceDefault: inventory.Resources{CPUMillis: 100, MemoryMiB: 128}, ResourceLimits: inventory.Resources{CPUMillis: 2000, MemoryMiB: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.inventory.SetAllocation(ctx, e.root, "space", inventory.AllocationInput{AllowedNodeIDs: []string{e.node.ID}, MaxApplications: 10, BuildConcurrency: 2, CPUMillis: 4000, MemoryMiB: 4096})
	if err != nil {
		t.Fatal(err)
	}
	// 资格与产物均由测试显式注入，fake verifier 不构成任何真实 AMD64/k3s/仓库验收。
	mustExec(t, db, "UPDATE execution_nodes SET cluster_node_uid='node-uid',os='linux',arch='amd64',last_seen=NOW(),build_ready=TRUE,runtime_ready=TRUE,ingress_ready=TRUE,metrics_ready=TRUE WHERE id=?", e.node.ID)
	mustExec(t, db, "UPDATE registries SET verified_at=NOW(),verified_revision=revision WHERE id=?", e.registry.ID)
	mustExec(t, db, "INSERT INTO worker_certificates(serial,node_id,worker_id,certificate_sha256,public_key_sha256,not_before,not_after) VALUES ('a1',?,?,?, ?,NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day')", e.node.ID, e.node.WorkerID, strings.Repeat("1", 64), strings.Repeat("2", 64))
	e.worker = WorkerIdentity{NodeID: e.node.ID, WorkerID: e.node.WorkerID, CertificateSerial: "a1", CertificateSHA256: strings.Repeat("1", 64)}
	for _, app := range []string{"app_one", "app_two"} {
		input := runtimeconfig.DefaultInput()
		input.Confirmed = true
		input.Variables = []runtimeconfig.VariableInput{{Name: "SECRET", Value: "secret-v1", Sensitive: true}}
		if _, err = e.configs.Save(ctx, e.root, "space", app, input); err != nil {
			t.Fatal(err)
		}
		digest := strings.Repeat("a", 64)
		mustExec(t, db, `INSERT INTO source_uploads(id,workspace_id,application_id,created_by,expected_size,expected_sha256,dockerfile,storage_key,expires_at) VALUES (?,'space',?,'root',1,?,'Dockerfile',?,NOW()+INTERVAL '1 hour')`, "upload_"+app, app, digest, "upload_"+fingerprint(app)[:32]+".tar.gz")
		mustExec(t, db, `INSERT INTO source_snapshots(id,workspace_id,application_id,upload_id,created_by,state,storage_key,sha256,compressed_bytes,expanded_bytes,file_bytes,entry_count,dockerfile,entries,expires_at) VALUES (?,'space',?,?,'root','READY',?,?,1,1024,1,1,'Dockerfile','[{"Path":"Dockerfile","Size":1,"Directory":false}]',NOW()+INTERVAL '1 hour')`, "src_"+app, app, "upload_"+app, "snapshot_"+fingerprint(app)[:32]+".tar.gz", digest)
		builder := build.NewService(db, access, e.inventory)
		b, err := builder.CreateBuild(ctx, e.root, "space", app, build.CreateInput{SourceSnapshotID: "src_" + app, ExpectedPolicyRevision: 1, IdempotencyKey: "build-" + app})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, "UPDATE builds SET state='SUCCEEDED',finished_at=NOW() WHERE id=?", b.ID)
		mustExec(t, db, "INSERT INTO build_attempts(id,workspace_id,application_id,build_id,node_id,worker_id,job_name,occupied,stopped_at) VALUES (?,'space',?,?,?,?,?,FALSE,NOW())", "attempt_"+app, app, b.ID, e.node.ID, e.node.WorkerID, "job-"+app)
		mustExec(t, db, "INSERT INTO build_artifacts(id,workspace_id,application_id,build_id,attempt_id,registry_id,credential_version,repository,tag,digest,platform,state,verified_at) VALUES (?,'space',?,?,?,?,1,?,?,?,'linux/amd64','VERIFIED',NOW())", "artifact_"+app, app, b.ID, "attempt_"+app, e.registry.ID, "registry.example.com/tenant/"+app, b.ID, "sha256:"+strings.Repeat("b", 64))
	}
	e.artifact = "artifact_app_one"
	return e
}
func createInput(key string) CreateInput {
	return CreateInput{ArtifactID: "artifact_app_one", RuntimeConfigRevision: 1, ExpectedPolicyRevision: 1, Expected: Expected{ConfigRevision: 1}, IdempotencyKey: key}
}
func create(t *testing.T, e environment, key string) Result {
	t.Helper()
	r, err := e.service.CreateDeployment(context.Background(), e.root, "space", "app_one", createInput(key))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func status(t *testing.T, e environment) Status {
	t.Helper()
	v, err := e.service.GetStatus(context.Background(), e.root, "space", "app_one")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func expected(v Status, config int64) Expected {
	return Expected{Generation: v.Generation, ServingReleaseID: v.ServingReleaseID, ConfigRevision: config}
}
func readyEvent(r Result, seq int64) Observation {
	return Observation{WorkspaceID: "space", ApplicationID: "app_one", OperationID: r.Operation.ID, ReleaseID: r.Release.ID, Generation: r.Operation.Generation, Seq: seq, ClusterNodeUID: "node-uid", DeploymentUID: "deployment-uid", PodUID: "pod-" + r.Release.ID, Digest: r.Release.Artifact.Digest, CorrectGeneration: true, Replicas: 1, ReadyReplicas: 1, OldPodsGone: true, ServiceEndpoints: true, Kind: "OBSERVED", CollectedAt: time.Now().UTC()}
}
func finishHTTP(t *testing.T, e environment, r Result) {
	t.Helper()
	event := readyEvent(r, 1)
	if _, err := e.service.ApplyObservation(context.Background(), e.worker, event); err != nil {
		t.Fatal(err)
	}
	for _, domain := range r.Release.Domains {
		if err := e.service.ApplyProbe(context.Background(), ProbeEvidence{WorkspaceID: "space", ApplicationID: "app_one", OperationID: r.Operation.ID, Generation: r.Operation.Generation, Domain: domain, Path: "/health", PodUID: event.PodUID, TLSVerified: true, Healthy: true, ObservedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreateConcurrentCASAndIdempotency(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	results := make(chan Result, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.service.CreateDeployment(ctx, e.root, "space", "app_one", createInput("same"))
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for r := range results {
		if id == "" {
			id = r.Operation.ID
		}
		if id != r.Operation.ID {
			t.Fatal("幂等请求创建多个操作")
		}
	}
	var count int64
	e.db.Model(&operationRecord{}).Count(&count)
	if count != 1 {
		t.Fatal(count)
	}
	if _, err := e.service.CreateDeployment(ctx, e.root, "space", "app_one", createInput("other")); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("过期generation未冲突:%v", err)
	}
	if v := status(t, e); v.Generation != 1 || v.ServingReleaseID != "" || v.ObservedState != "PENDING" {
		t.Fatalf("受理冒充成功:%#v", v)
	}
}
func TestHTTPSuccessRequiresAllKubernetesAndRegisteredProbeEvidence(t *testing.T) {
	e := setup(t)
	r := create(t, e, "one")
	ctx := context.Background()
	bad := readyEvent(r, 1)
	bad.OldPodsGone = false
	if _, err := e.service.ApplyObservation(ctx, e.worker, bad); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ServingReleaseID != "" {
		t.Fatal("旧副本未退出仍标成功")
	}
	good := readyEvent(r, 2)
	if _, err := e.service.ApplyObservation(ctx, e.worker, good); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ServingReleaseID != "" {
		t.Fatal("仅Kubernetes就绪冒充HTTPS成功")
	}
	proof := ProbeEvidence{WorkspaceID: "space", ApplicationID: "app_one", OperationID: r.Operation.ID, Generation: 1, Domain: "evil.example.com", Path: "/health", PodUID: good.PodUID, Healthy: true, TLSVerified: true, ObservedAt: time.Now().UTC()}
	if err := e.service.ApplyProbe(ctx, proof); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("接受任意探测目标:%v", err)
	}
	proof.Domain = r.Release.Domains[0]
	proof.TLSVerified = false
	if err := e.service.ApplyProbe(ctx, proof); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ServingReleaseID != "" {
		t.Fatal("TLS未验证冒充成功")
	}
	proof.TLSVerified = true
	proof.ObservedAt = time.Now().UTC()
	if err := e.service.ApplyProbe(ctx, proof); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ServingReleaseID != r.Release.ID || v.ObservedState != "RUNNING" {
		t.Fatalf("完整证据未成功:%#v", v)
	}
}
func TestStopSupersedesDeploymentAndOldGenerationCannotRevive(t *testing.T) {
	e := setup(t)
	r := create(t, e, "one")
	ctx := context.Background()
	stopped, err := e.service.Stop(ctx, e.root, "space", "app_one", ChangeInput{Expected: expected(status(t, e), 1), IdempotencyKey: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Operation.Generation != 2 {
		t.Fatal("停止没有提升代际")
	}
	if _, err = e.service.ApplyObservation(ctx, e.worker, readyEvent(r, 1)); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("旧发布复活:%v", err)
	}
	v := status(t, e)
	if v.ObservedState != "STOPPING" {
		t.Fatal("停止请求被当作已停止")
	}
	event := readyEvent(stopped, 1)
	event.Kind = "STOPPED"
	event.Replicas = 0
	event.ReadyReplicas = 0
	if _, err = e.service.ApplyObservation(ctx, e.worker, event); err != nil {
		t.Fatal(err)
	}
	if v = status(t, e); v.ObservedState != Stopped {
		t.Fatalf("停止证据未收敛:%#v", v)
	}
	if err = e.service.CheckGeneration(ctx, e.worker, "space", "app_one", r.Operation.ID, r.Release.ID, 1, "node-uid"); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("旧worker写命令未拒绝:%v", err)
	}
}
func TestFailedFirstPlacementAndDefaultDomainNeverMigrate(t *testing.T) {
	e := setup(t)
	r := create(t, e, "one")
	ctx := context.Background()
	failure := readyEvent(r, 1)
	failure.Kind = "FAILED"
	failure.ErrorCode = "IMAGE_PULL_FAILED"
	if _, err := e.service.ApplyObservation(ctx, e.worker, failure); err != nil {
		t.Fatal(err)
	}
	other, err := e.inventory.CreateNode(ctx, e.root, inventory.NodeInput{Name: "新节点", WorkerID: "worker-new", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.inventory.SavePolicy(ctx, e.root, inventory.PolicyInput{BuildNodeID: e.node.ID, RuntimeNodeID: other.ID, RegistryID: e.registry.ID, BaseDomain: "changed.example.com", ResourceDefault: inventory.Resources{CPUMillis: 100, MemoryMiB: 128}, ResourceLimits: inventory.Resources{CPUMillis: 2000, MemoryMiB: 2048}, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	in := createInput("retry")
	in.ExpectedPolicyRevision = 2
	in.Expected = expected(status(t, e), 1)
	retry, err := e.service.CreateDeployment(ctx, e.root, "space", "app_one", in)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Release.Placement.NodeID != r.Release.Placement.NodeID || retry.Release.Placement.DefaultDomain != r.Release.Placement.DefaultDomain {
		t.Fatal("首发失败或全局策略变更使Placement隐式迁移")
	}
	// 默认地址统一为 <应用名>-<地址后缀>.<根域名>（与 ResolveExecution 同规则），
	// 绝不回退为内部资源 ID 形态。
	if !regexp.MustCompile(`^one-[a-z][a-z0-9]{5}\.apps\.example\.com$`).MatchString(r.Release.Placement.DefaultDomain) {
		t.Fatalf("默认地址形态不符=%q", r.Release.Placement.DefaultDomain)
	}
}
func TestRollbackFrozenConfigAndDraftPreserved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	one := create(t, e, "one")
	finishHTTP(t, e, one)
	cfg := runtimeconfig.DefaultInput()
	cfg.Confirmed = true
	cfg.ExpectedRevision = 1
	cfg.Variables = []runtimeconfig.VariableInput{{Name: "SECRET", Value: "secret-v2", Sensitive: true}}
	if _, err := e.configs.Save(ctx, e.root, "space", "app_one", cfg); err != nil {
		t.Fatal(err)
	}
	in := createInput("two")
	in.RuntimeConfigRevision = 2
	in.Expected = expected(status(t, e), 2)
	two, err := e.service.CreateDeployment(ctx, e.root, "space", "app_one", in)
	if err != nil {
		t.Fatal(err)
	}
	finishHTTP(t, e, two)
	cfg.ExpectedRevision = 2
	cfg.Variables[0].Value = "draft-v3"
	if _, err = e.configs.Save(ctx, e.root, "space", "app_one", cfg); err != nil {
		t.Fatal(err)
	}
	rolled, err := e.service.Rollback(ctx, e.root, "space", "app_one", RollbackInput{TargetReleaseID: one.Release.ID, ExpectedPolicyRevision: 1, Expected: expected(status(t, e), 3), IdempotencyKey: "rollback"})
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Release.ID == one.Release.ID || rolled.Release.RuntimeConfigRevision != 1 || rolled.Release.Artifact.ID != one.Release.Artifact.ID {
		t.Fatal("回滚未创建绑定旧配置的新release")
	}
	latest, err := e.configs.Get(ctx, e.root, "space", "app_one")
	if err != nil || latest.Revision != 3 {
		t.Fatal("回滚覆盖最新配置草稿")
	}
	dispatch, err := e.service.ClaimPending(ctx, e.worker)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Config.Variables["SECRET"] != "secret-v1" {
		t.Fatal("回滚错误使用最新秘密")
	}
}
func TestCredentialVerificationFailureLeavesCurrentServiceUntouched(t *testing.T) {
	e := setup(t)
	one := create(t, e, "one")
	finishHTTP(t, e, one)
	e.service.verifier = verifierFunc(func(context.Context, ArtifactSnapshot) error { return errors.New("private registry internal error") })
	in := createInput("two")
	in.Expected = expected(status(t, e), 1)
	if _, err := e.service.CreateDeployment(context.Background(), e.root, "space", "app_one", in); !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
	if v := status(t, e); v.ServingReleaseID != one.Release.ID || v.Generation != 1 {
		t.Fatal("失败的仓库检查改变当前服务")
	}
}
func TestPermissionsWorkerRevocationAndDuplicateObservations(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	viewer := authorization.Principal{UserID: "viewer", AuthVersion: 1}
	if _, err := e.service.CreateDeployment(ctx, viewer, "space", "app_one", createInput("viewer")); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("viewer可部署")
	}
	if _, err := e.service.CreateDeployment(ctx, e.root, "other-space", "app_one", createInput("cross")); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("跨空间可部署")
	}
	r := create(t, e, "one")
	event := readyEvent(r, 1)
	event.Kind = "LOST"
	if _, err := e.service.ApplyObservation(ctx, e.worker, event); err != nil {
		t.Fatal(err)
	}
	if ack, err := e.service.ApplyObservation(ctx, e.worker, event); err != nil || !ack.Duplicate {
		t.Fatalf("事件重放不幂等:%#v %v", ack, err)
	}
	changed := event
	changed.Kind = "OBSERVED"
	if _, err := e.service.ApplyObservation(ctx, e.worker, changed); !errors.Is(err, ErrSequence) {
		t.Fatal("同seq不同内容未拒绝")
	}
	if v := status(t, e); v.ObservedState != Unknown {
		t.Fatal("失联冒充停止")
	}
	mustExec(t, e.db, "UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a1'")
	event.Seq = 2
	if _, err := e.service.ApplyObservation(ctx, e.worker, event); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatal("撤销worker证书仍可提交")
	}
}
func TestSuccessfulOperationKeepsHistoryWhileRuntimeObservationContinues(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := create(t, e, "one")
	finishHTTP(t, e, r)
	if err := e.service.CheckGeneration(ctx, e.worker, "space", "app_one", r.Operation.ID, r.Release.ID, 1, "node-uid"); err != nil {
		t.Fatal(err)
	}
	lost := readyEvent(r, 2)
	lost.Kind = "LOST"
	if _, err := e.service.ApplyObservation(ctx, e.worker, lost); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ObservedState != Unknown || v.ServingReleaseID != r.Release.ID {
		t.Fatalf("失联抹除历史服务事实:%#v", v)
	}
	history, err := e.service.Get(ctx, e.root, "space", "app_one", r.Operation.ID)
	if err != nil || history.Operation.State != Succeeded || history.Operation.Active {
		t.Fatalf("运行失联篡改历史发布结果:%#v %v", history, err)
	}
	healthy := readyEvent(r, 3)
	healthy.PodUID = "replacement-pod"
	if _, err = e.service.ApplyObservation(ctx, e.worker, healthy); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ObservedState != Unknown {
		t.Fatal("新Pod借用旧HTTP成功")
	}
	if err = e.service.ApplyProbe(ctx, ProbeEvidence{WorkspaceID: "space", ApplicationID: "app_one", OperationID: r.Operation.ID, Generation: 1, Domain: r.Release.Domains[0], Path: "/health", PodUID: healthy.PodUID, Healthy: true, TLSVerified: true, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ObservedState != "RUNNING" {
		t.Fatal("新Pod完整证据未恢复健康")
	}
	healthy.Seq = 4
	healthy.ReadyReplicas = 0
	healthy.CollectedAt = time.Now().UTC()
	if _, err = e.service.ApplyObservation(ctx, e.worker, healthy); err != nil {
		t.Fatal(err)
	}
	if v := status(t, e); v.ObservedState != "UNHEALTHY" {
		t.Fatal("不健康未更新运行态")
	}
}

func TestAutomaticRecoveryIsOptInOnceAndKeepsServingHistory(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			one := create(t, e, "one")
			finishHTTP(t, e, one)
			mustExec(t, e.db, "UPDATE instance_execution_policies SET auto_recovery_enabled=? WHERE revision=1", enabled)
			in := createInput("two")
			in.Expected = expected(status(t, e), 1)
			two, err := e.service.CreateDeployment(ctx, e.root, "space", "app_one", in)
			if err != nil {
				t.Fatal(err)
			}
			failure := readyEvent(two, 1)
			failure.Kind = "FAILED"
			failure.ErrorCode = "IMAGE_PULL_FAILED"
			if _, err = e.service.ApplyObservation(ctx, e.worker, failure); err != nil {
				t.Fatal(err)
			}
			v := status(t, e)
			if v.ServingReleaseID != one.Release.ID {
				t.Fatal("失败发布覆盖当前服务")
			}
			if !enabled {
				if v.Generation != 2 || v.ActiveOperationID != "" {
					t.Fatal("默认策略意外恢复")
				}
				return
			}
			if v.Generation != 3 || v.ActiveOperationID == "" {
				t.Fatalf("未创建一次恢复:%#v", v)
			}
			recovery, err := e.service.Get(ctx, e.root, "space", "app_one", v.ActiveOperationID)
			if err != nil {
				t.Fatal(err)
			}
			if recovery.Operation.Action != Recovery || recovery.Operation.RecoveryOf != two.Operation.ID || recovery.Release.RuntimeConfigRevision != one.Release.RuntimeConfigRevision {
				t.Fatal("恢复快照或关系错误")
			}
			recoveryFailure := readyEvent(recovery, 1)
			recoveryFailure.Kind = "FAILED"
			if _, err = e.service.ApplyObservation(ctx, e.worker, recoveryFailure); err != nil {
				t.Fatal(err)
			}
			v = status(t, e)
			if v.Generation != 3 || v.ActiveOperationID != "" {
				t.Fatal("恢复失败无限循环")
			}
		})
	}
}

func TestStopClaimDoesNotRequireRegistryOrSecretsAndRestartMarkerIsStable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	one := create(t, e, "one")
	finishHTTP(t, e, one)
	restart, err := e.service.Restart(ctx, e.root, "space", "app_one", ChangeInput{Expected: expected(status(t, e), 1), IdempotencyKey: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.service.ClaimPending(ctx, e.worker)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE deployment_operations SET next_dispatch_at=NOW() WHERE id=?", restart.Operation.ID)
	second, err := e.service.ClaimPending(ctx, e.worker)
	if err != nil {
		t.Fatal(err)
	}
	if first.OperationID != second.OperationID || first.RestartMarker != restart.Operation.ID || first.RestartMarker != second.RestartMarker {
		t.Fatal("重投改变restart marker")
	}
	stopped, err := e.service.Stop(ctx, e.root, "space", "app_one", ChangeInput{Expected: expected(status(t, e), 1), IdempotencyKey: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	e.service.configs = nil
	e.service.verifier = nil
	dispatch, err := e.service.ClaimPending(ctx, e.worker)
	if err != nil || dispatch.OperationID != stopped.Operation.ID || dispatch.Action != Stop {
		t.Fatalf("停止被凭据或秘密阻塞:%#v %v", dispatch, err)
	}
}

func TestStopDoesNotWaitForBlockedNetworkVerification(t *testing.T) {
	e := setup(t)
	one := create(t, e, "one")
	finishHTTP(t, e, one)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	e.service.verifier = verifierFunc(func(context.Context, ArtifactSnapshot) error { close(entered); <-release; return nil })
	in := createInput("blocked")
	in.Expected = expected(status(t, e), 1)
	done := make(chan error, 1)
	go func() { _, err := e.service.CreateDeployment(ctx, e.root, "space", "app_one", in); done <- err }()
	<-entered
	_, err := e.service.Stop(ctx, e.root, "space", "app_one", ChangeInput{Expected: in.Expected, IdempotencyKey: "stop"})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("网络返回后的陈旧发布覆盖停止:%v", err)
	}
}

// WORKER 应用无域名；空域名必须落为 JSONB 空数组而非 NULL，否则首次发布
// 违反 application_releases.domains NOT NULL 约束（23502）。
func TestWorkerFirstDeploymentPersistsEmptyDomains(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cfg := runtimeconfig.DefaultInput()
	cfg.Type = runtimeconfig.TypeWorker
	cfg.Port = 0
	cfg.Probe = runtimeconfig.Probe{}
	cfg.ExpectedRevision = 1
	cfg.Confirmed = true
	if _, err := e.configs.Save(ctx, e.root, "space", "app_two", cfg); err != nil {
		t.Fatal(err)
	}
	in := createInput("worker-first")
	in.RuntimeConfigRevision = 2
	in.Expected.ConfigRevision = 2
	in.ArtifactID = "artifact_app_two"
	result, err := e.service.CreateDeployment(ctx, e.root, "space", "app_two", in)
	if err != nil {
		t.Fatalf("WORKER首次发布落库失败: %v", err)
	}
	if len(result.Release.Domains) != 0 {
		t.Fatalf("WORKER发布不应有域名: %+v", result.Release.Domains)
	}
}
