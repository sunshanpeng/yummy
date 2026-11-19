package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"gorm.io/gorm"
)

type environment struct {
	db                  *gorm.DB
	service             *Service
	inv                 *inventory.Service
	root, admin, viewer authorization.Principal
	node                inventory.NodeView
	registry            inventory.RegistryView
	worker              WorkerIdentity
}

func setup(t *testing.T) environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, v := range []any{
		&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true},
		&model.User{ID: "admin", Username: "admin", PasswordHash: "hash"},
		&model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash"},
		&model.Workspace{ID: "space", Name: "空间", Slug: "space"},
		&model.WorkspaceMember{WorkspaceID: "space", UserID: "admin", Role: "admin"},
		&model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: "member"},
		&model.Application{ID: "app_one", WorkspaceID: "space", Name: "one"},
		&model.Application{ID: "app_two", WorkspaceID: "space", Name: "two"},
		&model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app_one", UserID: "viewer", Role: "viewer"},
	} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{33}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.NewService(db, access, box)
	e := environment{db: db, service: NewService(db, access, inv), inv: inv, root: authorization.Principal{UserID: "root", AuthVersion: 1}, admin: authorization.Principal{UserID: "admin", AuthVersion: 1}, viewer: authorization.Principal{UserID: "viewer", AuthVersion: 1}}
	ctx := context.Background()
	e.node, err = inv.CreateNode(ctx, e.root, inventory.NodeInput{Name: "节点", WorkerID: "worker-one", Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.registry, err = inv.CreateRegistry(ctx, e.root, inventory.RegistryInput{Name: "仓库", URL: "https://registry.example.com", RepositoryPrefix: "tenant", Username: "robot", Password: "never-in-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.SavePolicy(ctx, e.root, inventory.PolicyInput{BuildNodeID: e.node.ID, RuntimeNodeID: e.node.ID, RegistryID: e.registry.ID, RepositoryPrefix: "apps", ResourceDefault: inventory.Resources{CPUMillis: 500, MemoryMiB: 512}, ResourceLimits: inventory.Resources{CPUMillis: 1000, MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.SetAllocation(ctx, e.root, "space", inventory.AllocationInput{AllowedNodeIDs: []string{e.node.ID}, MaxApplications: 10, BuildConcurrency: 2, CPUMillis: 2000, MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	// 只有测试显式写入 ready/verified 事实；不声称本机执行了 Linux BuildKit 或仓库验证。
	mustExec(t, db, "UPDATE execution_nodes SET cluster_node_uid='node-uid', os='linux',arch='amd64',last_seen=NOW(),build_ready=TRUE WHERE id=?", e.node.ID)
	mustExec(t, db, "UPDATE registries SET verified_at=NOW(),verified_revision=revision WHERE id=?", e.registry.ID)
	mustExec(t, db, "INSERT INTO worker_certificates(serial,node_id,worker_id,certificate_sha256,public_key_sha256,not_before,not_after) VALUES ('a1',?,?,?, ?,NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day')", e.node.ID, e.node.WorkerID, strings.Repeat("1", 64), strings.Repeat("2", 64))
	e.worker = WorkerIdentity{NodeID: e.node.ID, WorkerID: e.node.WorkerID, CertificateSerial: "a1"}
	fixtureSource(t, e, "app_one", "source_one", time.Now().Add(time.Hour))
	fixtureSource(t, e, "app_two", "source_two", time.Now().Add(time.Hour))
	return e
}
func mustExec(t *testing.T, db *gorm.DB, sql string, args ...any) {
	t.Helper()
	if err := db.Exec(sql, args...).Error; err != nil {
		t.Fatal(err)
	}
}
func fixtureSource(t *testing.T, e environment, app, id string, expiry time.Time) {
	t.Helper()
	key := hash([]byte(id))[:32]
	digest := strings.Repeat("a", 64)
	mustExec(t, e.db, `INSERT INTO source_uploads(id,workspace_id,application_id,created_by,expected_size,expected_sha256,dockerfile,storage_key,created_at,expires_at) VALUES (?, 'space',?,'admin',1,?,'Dockerfile',?,?,?)`, "upload_"+id, app, digest, "upload_"+key+".tar.gz", expiry.Add(-2*time.Hour), expiry)
	mustExec(t, e.db, `INSERT INTO source_snapshots(id,workspace_id,application_id,upload_id,created_by,state,storage_key,sha256,compressed_bytes,expanded_bytes,file_bytes,entry_count,dockerfile,entries,created_at,expires_at) VALUES (?,'space',?,?,'admin','READY',?,?,1,1024,1,1,'Dockerfile','[{"Path":"Dockerfile","Size":1,"Directory":false}]',?,?)`, id, app, "upload_"+id, "snapshot_"+key+".tar.gz", digest, expiry.Add(-2*time.Hour), expiry)
}
func input(key string) CreateInput {
	return CreateInput{SourceSnapshotID: "source_one", ExpectedPolicyRevision: 1, IdempotencyKey: key}
}
func create(t *testing.T, e environment, key string) View {
	t.Helper()
	b, err := e.service.CreateBuild(context.Background(), e.admin, "space", "app_one", input(key))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func claim(t *testing.T, e environment) Dispatch {
	t.Helper()
	d, err := e.service.Claim(context.Background(), e.worker)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func get(t *testing.T, e environment, id string) View {
	t.Helper()
	b, err := e.service.Get(context.Background(), e.admin, "space", "app_one", id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func success(t *testing.T, e environment, d Dispatch) {
	t.Helper()
	if _, err := e.service.ApplyEvent(context.Background(), e.worker, Event{AttemptID: d.AttemptID, Seq: 1, Kind: "SUCCEEDED", JobUID: "uid-" + d.BuildID, Digest: "sha256:" + strings.Repeat("b", 64), LogsComplete: true}); err != nil {
		t.Fatal(err)
	}
}

type verifier func(context.Context, VerificationRequest) (VerificationResult, error)

func (v verifier) Verify(ctx context.Context, r VerificationRequest) (VerificationResult, error) {
	return v(ctx, r)
}
func goodVerifier(_ context.Context, r VerificationRequest) (VerificationResult, error) {
	return VerificationResult{Repository: r.Repository, Digest: r.Digest, Platform: r.Platform}, nil
}

func TestIdempotencyConcurrentAndFingerprintConflict(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	const n = 12
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", input("same"))
			ids <- v.ID
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for next := range ids {
		if id == "" {
			id = next
		}
		if id != next {
			t.Fatalf("产生多个build=%s/%s", id, next)
		}
	}
	changed := input("same")
	changed.AutoDeploy = true
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", changed); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("不同fingerprint=%v", err)
	}
	cross := input("same")
	cross.SourceSnapshotID = "source_two"
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_two", cross); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("actor key跨app=%v", err)
	}
	var count int64
	e.db.Model(&buildRecord{}).Count(&count)
	if count != 1 {
		t.Fatal(count)
	}
	var row buildRecord
	if err := e.db.First(&row, "id=?", id).Error; err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(row)
	if bytes.Contains(data, []byte("never-in-snapshot")) {
		t.Fatal("明文凭证泄漏")
	}
}
func TestClaimCapacityAcrossConcurrentScannersAndRestart(t *testing.T) {
	e := setup(t)
	for i := 0; i < 12; i++ {
		create(t, e, fmt.Sprint(i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	ds := make(chan Dispatch, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := e.service.Claim(context.Background(), e.worker)
			if err == nil {
				ds <- d
			} else if !errors.Is(err, ErrNoCapacity) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(ds)
	for err := range errs {
		t.Fatal(err)
	}
	count := 0
	for range ds {
		count++
	}
	if count != 2 {
		t.Fatalf("并发占用=%d", count)
	}
	var occupancy int64
	e.db.Model(&attemptRecord{}).Where("occupied").Count(&occupancy)
	if occupancy != 2 {
		t.Fatal(occupancy)
	}
	mustExec(t, e.db, "UPDATE build_attempts SET next_dispatch_at=NOW()-INTERVAL '1 minute'")
	access, err := authorization.NewAccess(e.db)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewService(e.db, access, e.inv)
	items, err := restarted.PendingDispatch(context.Background(), e.worker, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("恢复=%#v %v", items, err)
	}
	for _, d := range items {
		var a attemptRecord
		if err := e.db.First(&a, "build_id=?", d.BuildID).Error; err != nil {
			t.Fatal(err)
		}
		if a.ID != d.AttemptID || a.JobName != d.JobName || a.DispatchCount != 2 {
			t.Fatalf("重投身份变化=%#v", a)
		}
	}
}
func TestAllocationConcurrencyIsIndependentFromNodeCapacity(t *testing.T) {
	e := setup(t)
	mustExec(t, e.db, "UPDATE workspace_allocations SET build_concurrency=1")
	create(t, e, "one")
	create(t, e, "two")
	claim(t, e)
	if _, err := e.service.Claim(context.Background(), e.worker); !errors.Is(err, ErrNoCapacity) {
		t.Fatal(err)
	}
}
func TestEventSequenceDedupeConflictIdentityAndLateTerminal(t *testing.T) {
	e := setup(t)
	b := create(t, e, "event")
	d := claim(t, e)
	ctx := context.Background()
	ev := Event{AttemptID: d.AttemptID, Seq: 1, Kind: "RUNNING", JobUID: "uid", Stage: "BUILDING"}
	ack, err := e.service.ApplyEvent(ctx, e.worker, ev)
	if err != nil || ack.State != Running {
		t.Fatalf("event=%#v %v", ack, err)
	}
	ack, err = e.service.ApplyEvent(ctx, e.worker, ev)
	if err != nil || !ack.Duplicate {
		t.Fatal(ack, err)
	}
	ev.Stage = "PUSHING"
	if _, err := e.service.ApplyEvent(ctx, e.worker, ev); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
	ev.Seq = 3
	if _, err := e.service.ApplyEvent(ctx, e.worker, ev); !errors.Is(err, ErrSequence) {
		t.Fatal(err)
	}
	wrong := e.worker
	wrong.WorkerID = "other"
	if _, err := e.service.ApplyEvent(ctx, wrong, ev); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatal(err)
	}
	ev = Event{AttemptID: d.AttemptID, Seq: 2, Kind: "FAILED", JobUID: "wrong"}
	if _, err := e.service.ApplyEvent(ctx, e.worker, ev); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
	ev.JobUID = "uid"
	if _, err := e.service.ApplyEvent(ctx, e.worker, ev); err != nil {
		t.Fatal(err)
	}
	ev = Event{AttemptID: d.AttemptID, Seq: 3, Kind: "RUNNING", JobUID: "uid"}
	if _, err := e.service.ApplyEvent(ctx, e.worker, ev); err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Failed {
		t.Fatal(v)
	}
	mustExec(t, e.db, "UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a1'")
	if _, err := e.service.AppendLogs(ctx, e.worker, LogInput{AttemptID: d.AttemptID, Seq: 1, Content: []byte("x")}); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatal(err)
	}
}
func TestWorkerSuccessOnlyVerifiesAndBadPlatformFails(t *testing.T) {
	e := setup(t)
	b := create(t, e, "success")
	d := claim(t, e)
	success(t, e, d)
	if v := get(t, e, b.ID); v.State == Succeeded || v.Artifact == nil || v.Artifact.State != "VERIFYING" {
		t.Fatal(v)
	}
	wrong := verifier(func(_ context.Context, r VerificationRequest) (VerificationResult, error) {
		return VerificationResult{Repository: r.Repository, Digest: r.Digest, Platform: "linux/arm64"}, nil
	})
	if err := e.service.VerifyArtifact(context.Background(), b.ID, wrong); err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Failed || v.Artifact.State != "INVALID" {
		t.Fatal(v)
	}
}
func TestCancelBeforeAndDuringVerificationCannotBecomeSuccess(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	b := create(t, e, "cancel-first")
	d := claim(t, e)
	if v, err := e.service.Cancel(ctx, e.admin, "space", "app_one", b.ID); err != nil || v.State != Canceling {
		t.Fatal(v, err)
	}
	success(t, e, d)
	if err := e.service.VerifyArtifact(ctx, b.ID, verifier(goodVerifier)); err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Canceled || v.Artifact.State != "RESIDUAL" {
		t.Fatal(v)
	}
	b = create(t, e, "verify-race")
	d = claim(t, e)
	success(t, e, d)
	entered := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.service.VerifyArtifact(ctx, b.ID, verifier(func(ctx context.Context, r VerificationRequest) (VerificationResult, error) {
			close(entered)
			<-resume
			return goodVerifier(ctx, r)
		}))
	}()
	<-entered
	if v, err := e.service.Cancel(ctx, e.admin, "space", "app_one", b.ID); err != nil || v.State != Canceled {
		t.Fatal(v, err)
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Canceled || v.Artifact.State != "RESIDUAL" {
		t.Fatal(v)
	}
	b = create(t, e, "success-first")
	d = claim(t, e)
	success(t, e, d)
	if err := e.service.VerifyArtifact(ctx, b.ID, verifier(goodVerifier)); err != nil {
		t.Fatal(err)
	}
	if v, err := e.service.Cancel(ctx, e.admin, "space", "app_one", b.ID); err != nil || v.State != Succeeded {
		t.Fatal(v, err)
	}
}
func TestLostKeepsOccupancyAndLateStopNeverRewritesTerminal(t *testing.T) {
	e := setup(t)
	mustExec(t, e.db, "UPDATE execution_nodes SET capacity=1")
	b := create(t, e, "lost")
	d := claim(t, e)
	create(t, e, "waiting")
	now := time.Now().UTC()
	mustExec(t, e.db, "UPDATE execution_nodes SET last_seen=?", now.Add(-2*time.Minute))
	if err := e.service.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Reconciling {
		t.Fatal(v)
	}
	if err := e.service.Reconcile(context.Background(), now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Lost {
		t.Fatal(v)
	}
	var a attemptRecord
	e.db.First(&a, "id=?", d.AttemptID)
	if !a.Occupied {
		t.Fatal("LOST错误释放占用")
	}
	mustExec(t, e.db, "UPDATE execution_nodes SET last_seen=NOW()")
	if _, err := e.service.Claim(context.Background(), e.worker); !errors.Is(err, ErrNoCapacity) {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE build_attempts SET next_dispatch_at=NOW() WHERE id=?", d.AttemptID)
	items, err := e.service.PendingDispatch(context.Background(), e.worker, 10)
	if err != nil || len(items) != 1 || items[0].Command != "CANCEL" {
		t.Fatal(items, err)
	}
	if _, err := e.service.ApplyEvent(context.Background(), e.worker, Event{AttemptID: d.AttemptID, Seq: 1, Kind: "STOPPED"}); err != nil {
		t.Fatal(err)
	}
	e.db.First(&a, "id=?", d.AttemptID)
	if a.Occupied {
		t.Fatal("确认停止后未释放")
	}
	if v := get(t, e, b.ID); v.State != Lost {
		t.Fatal(v)
	}
	claim(t, e)
}
func TestSourceScopeExpiryReadinessAndRevisionCAS(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := input("cross")
	in.SourceSnapshotID = "source_two"
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal(err)
	}
	fixtureSource(t, e, "app_one", "expired", time.Now().Add(-time.Minute))
	in = input("expired")
	in.SourceSnapshotID = "expired"
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, ErrSourceExpired) {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE registries SET verified_at=NULL,verified_revision=NULL")
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", input("unverified")); !errors.Is(err, inventory.ErrNotReady) {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE registries SET verified_at=NOW(),verified_revision=revision")
	mustExec(t, e.db, "UPDATE execution_nodes SET build_ready=FALSE")
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", input("unready")); !errors.Is(err, inventory.ErrNotReady) {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE execution_nodes SET build_ready=TRUE")
	if _, err := e.service.CreatePreview(ctx, e.admin, "space", "app_one", input("preview")); err != nil {
		t.Fatal(err)
	}
	b := create(t, e, "frozen")
	_, err := e.inv.SavePolicy(ctx, e.root, inventory.PolicyInput{ExpectedRevision: 1, BuildNodeID: e.node.ID, RuntimeNodeID: e.node.ID, RegistryID: e.registry.ID, RepositoryPrefix: "changed", ResourceDefault: inventory.Resources{CPUMillis: 500, MemoryMiB: 512}, ResourceLimits: inventory.Resources{CPUMillis: 1000, MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", input("stale")); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.PolicyRevision != 1 || v.Repository != "registry.example.com/tenant/apps/app_one" {
		t.Fatal(v)
	}
	claim(t, e)
	revision, err := e.service.SaveConfig(ctx, e.admin, "space", "app_one", 0, Config{Dockerfile: "Dockerfile", Context: ".", Target: "test"})
	if err != nil || revision != 1 {
		t.Fatal(revision, err)
	}
	in = input("stale-config")
	in.ExpectedPolicyRevision = 2
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
}
func TestRuntimeBindingAndOnlyBuildDoesNotRequireConfirmation(t *testing.T) {
	e := setup(t)
	create(t, e, "only-build")
	ctx := context.Background()
	in := input("deploy")
	in.AutoDeploy = true
	in.RuntimeConfigRevision = 1
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); err == nil {
		t.Fatal("缺失运行配置接受")
	}
	mustExec(t, e.db, "INSERT INTO runtime_config_revisions(id,workspace_id,application_id,revision,confirmed,spec,created_by) VALUES ('cfg1','space','app_one',1,FALSE,'{}','admin')")
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE runtime_config_revisions SET confirmed=TRUE WHERE id='cfg1'")
	b, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.db, "INSERT INTO runtime_config_revisions(id,workspace_id,application_id,revision,confirmed,spec,created_by) VALUES ('cfg2','space','app_one',2,TRUE,'{}','admin')")
	if v := get(t, e, b.ID); v.RuntimeConfigRevision == nil || *v.RuntimeConfigRevision != 1 {
		t.Fatal(v)
	}
	in.IdempotencyKey = "stale-runtime"
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
	in.RuntimeConfigRevision = 2
	if _, err := e.service.CreateBuild(ctx, e.viewer, "space", "app_one", in); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal(err)
	}
}
func TestLogsLimitsDedupePaginationAndStateIndependence(t *testing.T) {
	e := setup(t)
	b := create(t, e, "logs")
	d := claim(t, e)
	ctx := context.Background()
	in := LogInput{AttemptID: d.AttemptID, Seq: 1, Content: []byte("hello")}
	if _, err := e.service.AppendLogs(ctx, e.worker, in); err != nil {
		t.Fatal(err)
	}
	if ack, err := e.service.AppendLogs(ctx, e.worker, in); err != nil || !ack.Duplicate {
		t.Fatal(ack, err)
	}
	in.Content = []byte("changed")
	if _, err := e.service.AppendLogs(ctx, e.worker, in); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
	in.Seq = 3
	if _, err := e.service.AppendLogs(ctx, e.worker, in); !errors.Is(err, ErrSequence) {
		t.Fatal(err)
	}
	in.Seq = 2
	in.Content = make([]byte, MaxLogChunk+1)
	if _, err := e.service.AppendLogs(ctx, e.worker, in); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal(err)
	}
	// 显式接近容量的 fixture，验证最后块截断，不用反复写20MiB放慢并发库测试。
	mustExec(t, e.db, "UPDATE builds SET logs_bytes=? WHERE id=?", MaxLogBytes-3, b.ID)
	in.Content = []byte("abcdef")
	if _, err := e.service.AppendLogs(ctx, e.worker, in); err != nil {
		t.Fatal(err)
	}
	in.Seq = 3
	if _, err := e.service.AppendLogs(ctx, e.worker, in); err != nil {
		t.Fatal(err)
	}
	success(t, e, d)
	if err := e.service.VerifyArtifact(ctx, b.ID, verifier(goodVerifier)); err != nil {
		t.Fatal(err)
	}
	logs, err := e.service.ReadLogs(ctx, e.viewer, "space", "app_one", b.ID, 0, 2)
	if err != nil || len(logs.Items) != 2 || logs.Complete || !logs.Truncated || logs.Items[1].Content != "abc" {
		t.Fatal(logs, err)
	}
	logs, err = e.service.ReadLogs(ctx, e.viewer, "space", "app_one", b.ID, logs.NextSeq, 2)
	if err != nil || !logs.Complete || len(logs.Items) != 1 {
		t.Fatal(logs, err)
	}
}
func TestPATRefreshReadScopesCancelQueuedAndStopActor(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	b := create(t, e, "queued")
	token := model.AccessToken{ID: "pat", UserID: "admin", Name: "token", TokenHash: "hash", Scopes: []string{"build:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := e.db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.db.Create(&model.AccessTokenApplication{TokenID: "pat", WorkspaceID: "space", ApplicationID: "app_one"}).Error; err != nil {
		t.Fatal(err)
	}
	p := authorization.Principal{UserID: "admin", AuthVersion: 1, TokenID: "pat"}
	if _, err := e.service.Get(ctx, p, "space", "app_one", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.ReadLogs(ctx, p, "space", "app_one", b.ID, 0, 10); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal(err)
	}
	mustExec(t, e.db, "UPDATE access_tokens SET revoked_at=NOW() WHERE id='pat'")
	if _, err := e.service.Get(ctx, p, "space", "app_one", b.ID); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatal(err)
	}
	if v, err := e.service.Cancel(ctx, e.admin, "space", "app_one", b.ID); err != nil || v.State != Canceled {
		t.Fatal(v, err)
	}
	b = create(t, e, "running")
	claim(t, e)
	queued := create(t, e, "will-stop")
	if err := e.service.StopActor(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if v := get(t, e, b.ID); v.State != Canceling {
		t.Fatal(v)
	}
	if v := get(t, e, queued.ID); v.State != Canceled {
		t.Fatal(v)
	}
}
