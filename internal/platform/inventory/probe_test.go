package inventory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/registryclient"
)

func TestRegistryProbeUsesTLSAndNeverSetsQualification(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "robot" || password != "stored-secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="local-protocol-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	registry, err := env.service.CreateRegistry(ctx, env.root, RegistryInput{Name: "本地TLS协议测试", URL: server.URL, Username: "robot", Password: "stored-secret"})
	if err != nil {
		t.Fatal(err)
	}
	config := registryclient.Config{AllowPrivate: true, TLSConfig: server.Client().Transport.(*http.Transport).TLSClientConfig}
	probe, err := env.service.probeRegistry(ctx, env.root, registry.ID, 1, "", config)
	if err != nil {
		t.Fatal(err)
	}
	if !probe.NetworkReachable || !probe.AuthenticationAccepted || probe.ReadVerified || probe.PushVerified || probe.WorkerPushPullVerified {
		t.Fatalf("协议探测冒充真实worker资格=%#v", probe)
	}
	stored, err := env.service.GetRegistry(ctx, env.root, registry.ID)
	if err != nil || stored.VerifiedAt != nil {
		t.Fatalf("控制面Probe不应设置verified_at=%#v,%v", stored, err)
	}
	if _, err := env.service.ProbeRegistry(ctx, env.admin, registry.ID, 1, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("空间管理员probe=%v", err)
	}
	if _, err := env.service.probeRegistry(ctx, env.root, registry.ID, 2, "", config); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("过期Probe=%v", err)
	}
}
func TestCredentialVersionKeepsOriginalRegistryTarget(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	registry, err := env.service.CreateRegistry(ctx, env.root, RegistryInput{Name: "旧仓库", URL: "https://old.example.com", Username: "old-robot", Password: "old-secret"})
	if err != nil {
		t.Fatal(err)
	}
	input := UpdateRegistryInput{Name: "新仓库", URL: "https://new.example.com", Status: "ACTIVE", ExpectedRevision: 1}
	if _, err := env.service.UpdateRegistry(ctx, env.root, registry.ID, input); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("不能把旧口令静默发往新origin=%v", err)
	}
	username, password := "new-robot", "new-secret"
	input.Username = &username
	input.Password = &password
	if _, err := env.service.UpdateRegistry(ctx, env.root, registry.ID, input); err != nil {
		t.Fatal(err)
	}
	old, err := env.service.GetCredential(env.db, registry.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Clear()
	current, err := env.service.GetCredential(env.db, registry.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Clear()
	if old.URL != "https://old.example.com" || old.Username != "old-robot" || string(old.Password) != "old-secret" || current.URL != "https://new.example.com" || string(current.Password) != "new-secret" {
		t.Fatal("版本凭据未固定原始origin")
	}
}
func TestDisabledNodeCannotBeReactivatedViaConfiguration(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	node, err := env.service.CreateNode(ctx, env.root, NodeInput{Name: "节点", WorkerID: "node", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.UpdateNode(ctx, env.root, node.ID, UpdateNodeInput{Name: node.Name, Capacity: 1, Status: "DISABLED", ExpectedRevision: 1}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("应由enrollment统一停用=%v", err)
	}
	if err := env.db.Model(&nodeRecord{}).Where("id = ?", node.ID).Update("status", "DISABLED").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.UpdateNode(ctx, env.root, node.ID, UpdateNodeInput{Name: node.Name, Capacity: 1, Status: "ACTIVE", ExpectedRevision: 1}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("普通配置复活旧证书=%v", err)
	}
}

// VerifyRegistry 是凭据验证的唯一写入点：真实探测全部通过才标记 verified，
// 失败关闭不标记；凭据版本冲突拒绝。构建/部署的资格检查以该标记为准。
func TestVerifyRegistryMarksVerifiedOnlyAfterRealProbe(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		user, password, ok := r.BasicAuth()
		if !ok || user != "robot" || password != "stored-secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="verify-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/yummy/runner/manifests/v1" {
			w.Header().Set("Docker-Content-Digest", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	registry, err := env.service.CreateRegistry(ctx, env.root, RegistryInput{Name: "验证写入测试", URL: server.URL, Username: "robot", Password: "stored-secret"})
	if err != nil {
		t.Fatal(err)
	}
	config := registryclient.Config{AllowPrivate: true, TLSConfig: server.Client().Transport.(*http.Transport).TLSClientConfig}

	// 成功路径：网络+认证通过（无读取引用）→ 标记 verified。
	registryView, probe, ok, err := env.service.verifyRegistry(ctx, env.root, registry.ID, 1, "", config)
	if err != nil || !ok {
		t.Fatalf("验证应成功 ok=%v err=%v probe=%#v", ok, err, probe)
	}
	if !probe.NetworkReachable || !probe.AuthenticationAccepted {
		t.Fatalf("探测结果不完整=%#v", probe)
	}
	stored, err := env.service.GetRegistry(ctx, env.root, registry.ID)
	if err != nil || stored.VerifiedAt == nil {
		t.Fatalf("验证成功必须标记 verified_at=%#v,%v", stored, err)
	}

	// 失败关闭：认证被拒（错误密码）→ 不标记。
	if _, err := env.service.CreateRegistry(ctx, env.root, RegistryInput{Name: "错误凭据", URL: server.URL, Username: "robot", Password: "wrong"}); err != nil {
		t.Fatal(err)
	}
	bad, _ := env.service.ListRegistries(ctx, env.root, 10, "")
	var badID string
	for _, r := range bad.Items {
		if r.Name == "错误凭据" {
			badID = r.ID
		}
	}
	_, _, ok2, err := env.service.verifyRegistry(ctx, env.root, badID, 1, "", config)
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Fatal("认证失败时 ok 必须为 false")
	}
	storedBad, err := env.service.GetRegistry(ctx, env.root, badID)
	if err != nil || storedBad.VerifiedAt != nil {
		t.Fatalf("认证失败不得标记 verified_at=%#v,%v", storedBad, err)
	}

	// 失败关闭：带读取引用但 manifest 不存在 → ok=false，且不得改动既有 verified 标记。
	fullRef := strings.TrimPrefix(server.URL, "https://") + "/yummy/runner:missing"
	_, _, okRef, err := env.service.verifyRegistry(ctx, env.root, registry.ID, 1, fullRef, config)
	if err != nil {
		t.Fatal(err)
	}
	if okRef {
		t.Fatal("引用读取失败时 ok 必须为 false")
	}
	storedAfterRef, err := env.service.GetRegistry(ctx, env.root, registry.ID)
	if err != nil || storedAfterRef.VerifiedAt == nil || !storedAfterRef.VerifiedAt.Equal(*stored.VerifiedAt) {
		t.Fatalf("失败的重复验证不得改动 verified_at: 之前=%v 之后=%v,%v", stored.VerifiedAt, storedAfterRef.VerifiedAt, err)
	}

	// 版本冲突：expectedRevision 过期 → 拒绝。
	if _, _, _, err := env.service.verifyRegistry(ctx, env.root, registry.ID, 2, "", config); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("过期expectedRevision必须冲突: %v", err)
	}
	_ = registryView
}
