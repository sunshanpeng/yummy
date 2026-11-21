package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/buildsecret"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/lib/security"
)

func TestSecretReferencesFreezeVersionAndRejectCrossApplication(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	secrets := buildsecret.NewService(e.db, e.service.access, box)
	e.service.SetSecretResolver(secrets)
	ref, err := secrets.Create(ctx, e.admin, "space", "app_one", buildsecret.CreateInput{Name: "npm_token", Value: "secret-plain-never-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := secrets.Create(ctx, e.admin, "space", "app_two", buildsecret.CreateInput{Name: "private_token", Value: "other-app-secret"})
	if err != nil {
		t.Fatal(err)
	}
	in := input("secret-build")
	in.Overrides.SecretRefs = []SecretRef{{SecretID: ref.ID}}
	b, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Config.SecretRefs) != 1 || b.Config.SecretRefs[0].Name != "npm_token" || b.Config.SecretRefs[0].Version != 1 {
		t.Fatal(b.Config)
	}
	var stored []secretReferenceRecord
	if err := e.db.Where("build_id=?", b.ID).Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].SecretID != ref.ID || stored[0].Version != 1 {
		t.Fatal(stored)
	}
	if _, err := secrets.Replace(ctx, e.admin, "space", "app_one", ref.ID, buildsecret.ReplaceInput{ExpectedVersion: 1, Value: "second-value"}); err != nil {
		t.Fatal(err)
	}
	if old := get(t, e, b.ID); old.Config.SecretRefs[0].Version != 1 {
		t.Fatal(old.Config)
	}
	in.IdempotencyKey = "secret-build-two"
	next, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in)
	if err != nil {
		t.Fatal(err)
	}
	if next.Config.SecretRefs[0].Version != 2 {
		t.Fatal(next.Config)
	}
	in.IdempotencyKey = "cross-secret"
	in.Overrides.SecretRefs = []SecretRef{{SecretID: foreign.ID}}
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal(err)
	}
	// 数据库组合FK也拒绝绕开服务把其他应用密钥挂进同一build。
	if err := e.db.Create(&secretReferenceRecord{WorkspaceID: "space", ApplicationID: "app_one", BuildID: b.ID, SecretID: foreign.ID, Version: 1, Name: "forged"}).Error; err == nil {
		t.Fatal("组合FK未拒绝跨app引用")
	}
	if err := secrets.Deactivate(ctx, e.admin, "space", "app_one", ref.ID, 2); err != nil {
		t.Fatal(err)
	}
	in.IdempotencyKey = "inactive-secret"
	in.Overrides.SecretRefs = []SecretRef{{SecretID: ref.ID, Version: 1}}
	if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal(err)
	}
	material, err := secrets.ReadVersion(e.db, "space", "app_one", buildsecret.Ref{SecretID: ref.ID, Version: 1, Name: "npm_token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(material.Value) != "secret-plain-never-snapshot" {
		t.Fatal("旧版本不可恢复")
	}
	material.Clear()
	rows := []buildRecord{}
	if err := e.db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(rows)
	if bytes.Contains(data, []byte("secret-plain")) || bytes.Contains(data, []byte("second-value")) {
		t.Fatal("build快照含秘密")
	}
}

func TestConfigVersionResponseAndPaginationRemainScoped(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	initial, err := e.service.GetConfig(ctx, e.admin, "space", "app_one")
	if err != nil || initial.Revision != 0 || initial.Config.Dockerfile != "Dockerfile" {
		t.Fatal(initial, err)
	}
	first, err := e.service.SaveConfig(ctx, e.admin, "space", "app_one", 0, Config{Target: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.service.SaveConfig(ctx, e.admin, "space", "app_one", first, Config{Target: "second"})
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := e.service.GetConfigRevision(ctx, e.admin, "space", "app_one", first)
	if err != nil || immutable.Revision != first || immutable.Config.Target != "first" {
		t.Fatal(immutable, err)
	}
	latest, err := e.service.GetConfig(ctx, e.admin, "space", "app_one")
	if err != nil || latest.Revision != second || latest.Config.Target != "second" {
		t.Fatal(latest, err)
	}
	if _, err := e.service.SaveConfig(ctx, e.admin, "space", "app_one", first, Config{}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		in := input(strings.Repeat("p", i+1))
		in.ConfigRevision = second
		if _, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", in); err != nil {
			t.Fatal(err)
		}
	}
	page, err := e.service.List(ctx, e.viewer, "space", "app_one", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	tail, err := e.service.List(ctx, e.viewer, "space", "app_one", 2, page.NextCursor)
	if err != nil || len(tail.Items) != 1 || tail.Items[0].ID == page.Items[0].ID || tail.NextCursor != "" {
		t.Fatal(tail, err)
	}
	if _, err := e.service.List(ctx, e.admin, "space", "app_two", 2, page.NextCursor); err == nil {
		t.Fatal("游标跨app使用")
	}
}

func TestTimeoutRequiresStopAndDisabledActorNeverStarts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	queued := create(t, e, "timeout-queue")
	now := time.Now().UTC()
	mustExec(t, e.db, "UPDATE builds SET created_at=?,deadline_at=? WHERE id=?", now.Add(-2*time.Hour), now.Add(-time.Hour), queued.ID)
	if err := e.service.Reconcile(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := get(t, e, queued.ID); got.State != TimedOut {
		t.Fatal(got)
	}
	running := create(t, e, "timeout-running")
	d := claim(t, e)
	mustExec(t, e.db, "UPDATE builds SET created_at=?,deadline_at=? WHERE id=?", now.Add(-2*time.Hour), now.Add(-time.Minute), running.ID)
	if err := e.service.Reconcile(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := get(t, e, running.ID); got.State != Canceling || got.DesiredState != "CANCEL" {
		t.Fatal(got)
	}
	var a attemptRecord
	e.db.First(&a, "id=?", d.AttemptID)
	if !a.Occupied {
		t.Fatal("超时无停止证明即释放")
	}
	if _, err := e.service.ApplyEvent(ctx, e.worker, Event{AttemptID: d.AttemptID, Seq: 1, Kind: "STOPPED"}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, e, running.ID); got.State != TimedOut {
		t.Fatal(got)
	}
	pending := create(t, e, "actor-disabled")
	mustExec(t, e.db, "UPDATE users SET status='DISABLED',auth_version=auth_version+1 WHERE id='admin'")
	if _, err := e.service.Claim(ctx, e.worker); !errors.Is(err, ErrNoCapacity) {
		t.Fatal(err)
	}
	// 原用户已失效，用全局管理员观察系统取消结果。
	got, err := e.service.Get(ctx, e.root, "space", "app_one", pending.ID)
	if err != nil || got.State != Canceled {
		t.Fatal(got, err)
	}
	if _, err := e.service.Get(ctx, e.admin, "space", "app_one", pending.ID); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatal(err)
	}
}

func TestGetAttemptRecoversUIDAndRequestCancellationDoesNotCancelBuild(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	b, err := e.service.CreateBuild(ctx, e.admin, "space", "app_one", input("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	d := claim(t, e)
	event := Event{AttemptID: d.AttemptID, Seq: 1, Kind: "ACCEPTED", JobUID: "persisted-job-uid"}
	if _, err := e.service.ApplyEvent(context.Background(), e.worker, event); err != nil {
		t.Fatal(err)
	}
	access, err := authorization.NewAccess(e.db)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewService(e.db, access, e.inv, nil)
	recovered, err := restarted.GetAttempt(context.Background(), e.worker, d.AttemptID)
	if err != nil || recovered.AttemptID != d.AttemptID || recovered.JobName != d.JobName || recovered.JobUID != "persisted-job-uid" || recovered.Command != "RECONCILE" {
		t.Fatal(recovered, err)
	}
	got := get(t, e, b.ID)
	if got.DesiredState != "RUN" || got.State != Starting {
		t.Fatal(got)
	}
	mustExec(t, e.db, "UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a1'")
	if _, err := restarted.GetAttempt(context.Background(), e.worker, d.AttemptID); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatal(err)
	}
}

func TestConcurrentEventAndLogReplaysAreIdempotent(t *testing.T) {
	e := setup(t)
	create(t, e, "parallel-events")
	d := claim(t, e)
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := e.service.ApplyEvent(context.Background(), e.worker, Event{AttemptID: d.AttemptID, Seq: 1, Kind: "RUNNING", JobUID: "uid", Stage: "BUILDING"})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := e.service.AppendLogs(context.Background(), e.worker, LogInput{AttemptID: d.AttemptID, Seq: 1, Content: []byte("same-log")})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var events, logs int64
	e.db.Model(&eventRecord{}).Count(&events)
	e.db.Model(&logRecord{}).Count(&logs)
	if events != 1 || logs != 1 {
		t.Fatal(events, logs)
	}
}

func TestRetryCreatesNewIdentityAndCrossAppBuildFKRejects(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	b := create(t, e, "original")
	if _, err := e.service.Cancel(ctx, e.admin, "space", "app_one", b.ID); err != nil {
		t.Fatal(err)
	}
	next, err := e.service.Retry(ctx, e.admin, "space", "app_one", b.ID, input("retry"))
	if err != nil || next.ID == b.ID || next.RetryOf == nil || *next.RetryOf != b.ID {
		t.Fatal(next, err)
	}
	d := claim(t, e)
	if d.BuildID != next.ID || d.Tag != next.ID {
		t.Fatal(d)
	}
	var original buildRecord
	if err := e.db.First(&original, "id=?", b.ID).Error; err != nil {
		t.Fatal(err)
	}
	original.ID = "forged"
	original.Tag = "forged"
	original.IdempotencyKey = "forged"
	original.SourceSnapshotID = "source_two"
	if err := e.db.Create(&original).Error; err == nil {
		t.Fatal("source组合FK未拒绝跨app")
	}
	var audits []model.AuditEvent
	if err := e.db.Where("object_id = ?", b.ID).Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) < 2 {
		t.Fatal("受理和取消缺审计")
	}
}
