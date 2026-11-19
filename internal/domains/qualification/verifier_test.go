package qualification

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
)

type environment struct {
	db       *gorm.DB
	service  *inventory.Service
	verifier *Verifier
	root     authorization.Principal
	node     inventory.NodeView
	other    inventory.NodeView
}

// 经真实 inventory 服务登记验收记录，再由 Verifier 核对；两包互不import主代码，仅测试同库装配。
func setup(t *testing.T) environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, value := range []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}} {
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
	service := inventory.NewService(db, access, box)
	root := authorization.Principal{UserID: "root", AuthVersion: 1}
	ctx := context.Background()
	node, err := service.CreateNode(ctx, root, inventory.NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.CreateNode(ctx, root, inventory.NodeInput{Name: "备用节点", WorkerID: "worker-2", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	return environment{db: db, service: service, verifier: NewVerifier(db), root: root, node: node, other: other}
}

func report(qualificationID, nonce, uid, os, arch string, build, runtime, ingress, metrics bool) *platformv1.WorkerQualification {
	return &platformv1.WorkerQualification{QualificationId: qualificationID, Nonce: nonce, ClusterNodeUid: uid, Os: os, Arch: arch, Build: build, Runtime: runtime, Ingress: ingress, Metrics: metrics}
}

func TestVerifierFailsClosedOnEveryMismatch(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	identity := enrollment.Identity{NodeID: env.node.ID, WorkerID: "worker-1"}
	matching := report("install-accept-1", "n1", "uid-a", "linux", "amd64", true, true, true, true)
	registered, err := env.service.RegisterQualification(ctx, env.root, inventory.QualificationInput{NodeID: env.node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Runtime: true, Ingress: true, Metrics: true})
	if err != nil {
		t.Fatal(err)
	}

	// 无任何登记记录：拒。
	if _, err := env.verifier.Verify(ctx, identity, "n1", report("missing", "n1", "uid-a", "linux", "amd64", true, true, true, true)); !errors.Is(err, ErrMatch) {
		t.Fatalf("无记录放行=%v", err)
	}
	// 跨节点/跨worker复用资格：拒。
	cross := enrollment.Identity{NodeID: env.other.ID, WorkerID: "worker-2"}
	if _, err := env.verifier.Verify(ctx, cross, "n1", matching); !errors.Is(err, ErrMatch) {
		t.Fatalf("跨节点放行=%v", err)
	}
	wrongWorker := enrollment.Identity{NodeID: env.node.ID, WorkerID: "worker-2"}
	if _, err := env.verifier.Verify(ctx, wrongWorker, "n1", matching); !errors.Is(err, ErrMatch) {
		t.Fatalf("跨worker放行=%v", err)
	}
	// 上报与登记不一致（UID/OS/ARCH/能力位）：逐项拒。
	for _, message := range []*platformv1.WorkerQualification{
		report("install-accept-1", "n1", "uid-b", "linux", "amd64", true, true, true, true),
		report("install-accept-1", "n1", "uid-a", "darwin", "amd64", true, true, true, true),
		report("install-accept-1", "n1", "uid-a", "linux", "arm64", true, true, true, true),
		report("install-accept-1", "n1", "uid-a", "linux", "amd64", false, true, true, true),
		report("install-accept-1", "n1", "uid-a", "linux", "amd64", true, false, true, true),
		report("install-accept-1", "n1", "uid-a", "linux", "amd64", true, true, false, true),
		report("install-accept-1", "n1", "uid-a", "linux", "amd64", true, true, true, false),
	} {
		if _, err := env.verifier.Verify(ctx, identity, "n1", message); !errors.Is(err, ErrMatch) {
			t.Fatalf("不一致上报放行=%#v,%v", message, err)
		}
	}
	// nonce 防御：空nonce、上报nonce不一致、空上报、缺标识均拒。
	if _, err := env.verifier.Verify(ctx, identity, "", matching); !errors.Is(err, ErrReport) {
		t.Fatalf("空nonce放行=%v", err)
	}
	if _, err := env.verifier.Verify(ctx, identity, "n1", report("install-accept-1", "other", "uid-a", "linux", "amd64", true, true, true, true)); !errors.Is(err, ErrReport) {
		t.Fatalf("nonce不一致放行=%v", err)
	}
	if _, err := env.verifier.Verify(ctx, identity, "n1", nil); !errors.Is(err, ErrReport) {
		t.Fatalf("空上报放行=%v", err)
	}
	if _, err := env.verifier.Verify(ctx, identity, "n1", report("", "n1", "uid-a", "linux", "amd64", true, true, true, true)); !errors.Is(err, ErrReport) {
		t.Fatalf("缺标识放行=%v", err)
	}
	// 已撤销：拒。
	if _, err := env.service.RevokeQualification(ctx, env.root, registered.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.verifier.Verify(ctx, identity, "n1", matching); !errors.Is(err, ErrMatch) {
		t.Fatalf("已撤销放行=%v", err)
	}
	// 节点停用（DISABLED）：拒，即便记录活动。
	if _, err := env.service.RegisterQualification(ctx, env.root, inventory.QualificationInput{NodeID: env.node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Metrics: true}); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Exec(`UPDATE execution_nodes SET status = 'DISABLED' WHERE id = ?`, env.node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := env.verifier.Verify(ctx, identity, "n1", report("install-accept-1", "n1", "uid-a", "linux", "amd64", true, false, false, true)); !errors.Is(err, ErrMatch) {
		t.Fatalf("停用节点放行=%v", err)
	}
}

func TestVerifierReturnsRegisteredFactsOnExactMatch(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	if _, err := env.service.RegisterQualification(ctx, env.root, inventory.QualificationInput{NodeID: env.node.ID, WorkerID: "worker-1", ClusterNodeUID: "uid-a", QualificationID: "install-accept-1", Build: true, Runtime: true, Ingress: true, Metrics: true, Evidence: map[string]string{"验收": "线下安装验收"}}); err != nil {
		t.Fatal(err)
	}
	identity := enrollment.Identity{NodeID: env.node.ID, WorkerID: "worker-1"}
	proof, err := env.verifier.Verify(ctx, identity, "session-nonce", report("install-accept-1", "session-nonce", "uid-a", "linux", "amd64", true, true, true, true))
	if err != nil {
		t.Fatal(err)
	}
	want := Qualification{ClusterNodeUID: "uid-a", OS: "linux", Arch: "amd64", Build: true, Runtime: true, Ingress: true, Metrics: true}
	if proof != want {
		t.Fatalf("返回值=%#v want %#v", proof, want)
	}
}
