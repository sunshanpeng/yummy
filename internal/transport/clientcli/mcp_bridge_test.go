package clientcli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
)

// TestResolveClientBridge 验证 MCP 桥接客户端按 CLI 相同规则解析凭证与默认目标；
// 未登录返回 UNAUTHENTICATED 的 *Error。
func TestResolveClientBridge(t *testing.T) {
	t.Setenv("YUMMY_TOKEN", "bridge-secret")
	path, h := platformv1connect.NewWorkspaceServiceHandler(testWorkspace{})
	mux := http.NewServeMux()
	mux.Handle("/api"+path, http.StripPrefix("/api", h))
	server := httptest.NewServer(mux)
	defer server.Close()
	dir := configDir(t)
	client, err := ResolveClient(context.Background(), ClientOptions{ConfigDir: dir, Server: server.URL, AllowLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if client.Token != "bridge-secret" || client.DefaultWorkspace != "" {
		t.Fatalf("凭证或默认目标错误: %q %q", client.Token, client.DefaultWorkspace)
	}
	if client.Workspace == nil || client.Build == nil || client.Deployment == nil {
		t.Fatal("资源客户端未装配")
	}
	if err = (configStore{dir: dir}).update("default", func(p *profile) error {
		p.Server = server.URL
		p.Workspace = "w1"
		p.Application = "a1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client, err = ResolveClient(context.Background(), ClientOptions{ConfigDir: dir, Server: server.URL, AllowLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if client.DefaultWorkspace != "w1" || client.DefaultApplication != "a1" {
		t.Fatalf("profile 默认目标未读取: %+v", client)
	}
	if _, err = ResolveClient(context.Background(), ClientOptions{ConfigDir: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("无凭证必须报错")
	}
	var clientErr *Error
	if !errors.As(err, &clientErr) || clientErr.Code != "UNAUTHENTICATED" {
		t.Fatalf("未登录错误码错误: %v", err)
	}
	if got := ErrorJSON(clientErr); got == "" || containsSecret(got, "bridge-secret") {
		t.Fatalf("错误输出异常: %q", got)
	}
}

func ErrorJSON(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Error()
	}
	return ""
}

func containsSecret(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
