package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
)

func TestBuildGrantBoundToAttemptAndRevocable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	snapshot := e.ready(t)
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.NewService(e.db, e.access, box)
	node, err := inv.CreateNode(ctx, e.admin, inventory.NodeInput{Name: "构建节点", WorkerID: "worker-source", Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := inv.CreateRegistry(ctx, e.admin, inventory.RegistryInput{Name: "仓库", URL: "https://registry.example.com", Username: "robot", Password: "private"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.SavePolicy(ctx, e.admin, inventory.PolicyInput{BuildNodeID: node.ID, RuntimeNodeID: node.ID, RegistryID: registry.ID, ResourceDefault: inventory.Resources{CPUMillis: 100, MemoryMiB: 128}, ResourceLimits: inventory.Resources{CPUMillis: 1000, MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.SetAllocation(ctx, e.admin, "space", inventory.AllocationInput{AllowedNodeIDs: []string{node.ID}, MaxApplications: 10, BuildConcurrency: 2, CPUMillis: 2000, MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		query string
		args  []any
	}{
		{"UPDATE execution_nodes SET cluster_node_uid='node-uid',os='linux',arch='amd64',last_seen=NOW(),build_ready=TRUE WHERE id=?", []any{node.ID}},
		{"UPDATE registries SET verified_at=NOW(),verified_revision=revision WHERE id=?", []any{registry.ID}},
		{"INSERT INTO worker_certificates(id,serial,node_id,worker_id,certificate_sha256,public_key_sha256,not_before,not_after) VALUES ('a1','a1',?,?,?,?,NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day')", []any{node.ID, node.WorkerID, strings.Repeat("1", 64), strings.Repeat("2", 64)}},
	} {
		if err := e.db.Exec(item.query, item.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := build.NewService(e.db, e.access, inv, nil)
	created, err := service.CreateBuild(ctx, e.admin, "space", "app", build.CreateInput{SourceSnapshotID: snapshot.ID, ExpectedPolicyRevision: 1, IdempotencyKey: "grant-build"})
	if err != nil {
		t.Fatal(err)
	}
	worker := build.WorkerIdentity{NodeID: node.ID, WorkerID: node.WorkerID, CertificateSerial: "a1"}
	dispatch, err := service.Claim(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.BuildID != created.ID || dispatch.Command != "RUN" {
		t.Fatal(dispatch)
	}
	var wg sync.WaitGroup
	grants, errs := make(chan BuildGrant, 6), make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var grant BuildGrant
			err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var err error
				grant, err = e.service.GrantForBuild(tx, box, worker, dispatch.AttemptID)
				return err
			})
			grants <- grant
			errs <- err
		}()
	}
	wg.Wait()
	close(grants)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var grant BuildGrant
	for next := range grants {
		if grant.ID == "" {
			grant = next
		}
		if next.ID != grant.ID || next.Token != grant.Token {
			t.Fatal("同一 attempt 重投产生不同令牌")
		}
	}
	if grant.SHA256 != snapshot.SHA256 || grant.Size != int64(len(e.data)) || grant.ExpiresAt.After(dispatch.DeadlineAt) {
		t.Fatal("授权摘要、大小或截止时间错误")
	}
	encoded, err := json.Marshal(grant)
	if err != nil || bytes.Contains(encoded, []byte(grant.Token)) {
		t.Fatalf("授权 JSON 泄漏令牌: %v", err)
	}
	var stored buildGrantRecord
	if err := e.db.Where("id = ?", grant.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.EncryptedToken, []byte(grant.Token)) || stored.TokenHash == grant.Token {
		t.Fatal("令牌明文落库")
	}
	read := func(id, token string) ([]byte, error) {
		r, _, err := e.service.OpenBuildGrant(ctx, id, token)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	}
	data, err := read(grant.ID, grant.Token)
	if err != nil || !bytes.Equal(data, e.data) {
		t.Fatalf("读取快照: %v", err)
	}
	if _, err = read("sgr_"+strings.Repeat("0", 32), grant.Token); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("跨授权访问: %v", err)
	}
	if _, err = read(grant.ID, grant.Token[:len(grant.Token)-1]+"!"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("伪造令牌: %v", err)
	}
	wrong := worker
	wrong.NodeID = "other"
	if err = e.db.Transaction(func(tx *gorm.DB) error {
		_, err := e.service.GrantForBuild(tx, box, wrong, dispatch.AttemptID)
		return err
	}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("跨节点发放: %v", err)
	}
	if err = e.db.Exec("UPDATE source_build_grants SET expires_at=NOW()-INTERVAL '1 second',created_at=NOW()-INTERVAL '2 seconds' WHERE id=?", grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = read(grant.ID, grant.Token); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("过期令牌有效: %v", err)
	}
	if err = e.db.Exec("UPDATE source_build_grants SET expires_at=NOW()+INTERVAL '1 hour' WHERE id=?", grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err = e.db.Exec("UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a1'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = read(grant.ID, grant.Token); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("证书撤销后仍可下载: %v", err)
	}
	if err = e.db.Exec("UPDATE worker_certificates SET revoked_at=NULL WHERE serial='a1'").Error; err != nil {
		t.Fatal(err)
	}
	if err = e.db.Exec("UPDATE builds SET desired_state='CANCEL' WHERE id=?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = read(grant.ID, grant.Token); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("取消后仍可下载: %v", err)
	}
}

// 证书轮换恢复：节点换发新证书后，未确认 Job 的 attempt 原子换绑新序列号重新发放；
// 已确认 Job 的执行不换发，但下载按节点当前有效证书放行，节点全部撤销才阻断。
func TestBuildGrantSurvivesCertificateRotation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	snapshot := e.ready(t)
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.NewService(e.db, e.access, box)
	node, err := inv.CreateNode(ctx, e.admin, inventory.NodeInput{Name: "轮换节点", WorkerID: "worker-rotate", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := inv.CreateRegistry(ctx, e.admin, inventory.RegistryInput{Name: "仓库", URL: "https://registry.example.com", Username: "robot", Password: "private"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.SavePolicy(ctx, e.admin, inventory.PolicyInput{BuildNodeID: node.ID, RuntimeNodeID: node.ID, RegistryID: registry.ID, ResourceDefault: inventory.Resources{CPUMillis: 100, MemoryMiB: 128}, ResourceLimits: inventory.Resources{CPUMillis: 1000, MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inv.SetAllocation(ctx, e.admin, "space", inventory.AllocationInput{AllowedNodeIDs: []string{node.ID}, MaxApplications: 10, BuildConcurrency: 1, CPUMillis: 2000, MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	insertCert := func(serial, sha1, sha2 string) {
		t.Helper()
		if err := e.db.Exec("INSERT INTO worker_certificates(id,serial,node_id,worker_id,certificate_sha256,public_key_sha256,not_before,not_after) VALUES ('wct-'||?, ?, ?, ?, ?, ?, NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day')", serial, serial, node.ID, node.WorkerID, sha1, sha2).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		query string
		args  []any
	}{
		{"UPDATE execution_nodes SET cluster_node_uid='node-uid',os='linux',arch='amd64',last_seen=NOW(),build_ready=TRUE WHERE id=?", []any{node.ID}},
		{"UPDATE registries SET verified_at=NOW(),verified_revision=revision WHERE id=?", []any{registry.ID}},
	} {
		if err := e.db.Exec(item.query, item.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	insertCert("a1", strings.Repeat("1", 64), strings.Repeat("2", 64))
	service := build.NewService(e.db, e.access, inv, nil)
	if _, err := service.CreateBuild(ctx, e.admin, "space", "app", build.CreateInput{SourceSnapshotID: snapshot.ID, ExpectedPolicyRevision: 1, IdempotencyKey: "rotate-build"}); err != nil {
		t.Fatal(err)
	}
	worker1 := build.WorkerIdentity{NodeID: node.ID, WorkerID: node.WorkerID, CertificateSerial: "a1"}
	var dispatch build.Dispatch
	grantInTx := func(identity build.WorkerIdentity) (BuildGrant, error) {
		var grant BuildGrant
		err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var err error
			grant, err = e.service.GrantForBuild(tx, box, identity, dispatch.AttemptID)
			return err
		})
		return grant, err
	}
	dispatch, err = service.Claim(ctx, worker1)
	if err != nil {
		t.Fatal(err)
	}
	original, err := grantInTx(worker1)
	if err != nil {
		t.Fatal(err)
	}
	read := func(id, token string) error {
		r, _, err := e.service.OpenBuildGrant(ctx, id, token)
		if err != nil {
			return err
		}
		return r.Close()
	}
	// 轮换：撤销 a1、登记 a2。attempt 尚无 JobUID → 换绑 a2 重新发放。
	if err = e.db.Exec("UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a1'").Error; err != nil {
		t.Fatal(err)
	}
	insertCert("a2", strings.Repeat("3", 64), strings.Repeat("4", 64))
	worker2 := worker1
	worker2.CertificateSerial = "a2"
	reissued, err := grantInTx(worker2)
	if err != nil {
		t.Fatalf("轮换后未确认Job的attempt应换绑重新发放: %v", err)
	}
	if reissued.ID == original.ID || reissued.Token == original.Token {
		t.Fatal("换绑应产生新授权与新令牌")
	}
	if err = read(reissued.ID, reissued.Token); err != nil {
		t.Fatalf("新令牌下载失败: %v", err)
	}
	if err = read(original.ID, original.Token); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("旧令牌应随旧行删除失效: %v", err)
	}
	// 确认 JobUID 后再次轮换：不再换发（旧令牌已进入 Secret），但节点存在 a3
	// 有效证书时，旧令牌下载按节点粒度放行，在途构建不因轮换中断。
	if err = e.db.Exec("UPDATE build_attempts SET job_uid='persisted-job-uid' WHERE id=?", dispatch.AttemptID).Error; err != nil {
		t.Fatal(err)
	}
	if err = e.db.Exec("UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a2'").Error; err != nil {
		t.Fatal(err)
	}
	insertCert("a3", strings.Repeat("5", 64), strings.Repeat("6", 64))
	worker3 := worker1
	worker3.CertificateSerial = "a3"
	if _, err = grantInTx(worker3); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("已确认Job的attempt不得换发: %v", err)
	}
	if err = read(reissued.ID, reissued.Token); err != nil {
		t.Fatalf("节点存在有效证书时在途下载不应被轮换阻断: %v", err)
	}
	// 节点全部证书撤销后才阻断下载（节点粒度撤销语义）。
	if err = e.db.Exec("UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a3'").Error; err != nil {
		t.Fatal(err)
	}
	if err = read(reissued.ID, reissued.Token); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("节点全部证书撤销后仍可下载: %v", err)
	}
}
