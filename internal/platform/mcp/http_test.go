package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/platform/application"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
)

type fakeApplications struct {
	mu                     sync.Mutex
	revoked                bool
	lastApp, lastWorkspace string
}

func (f *fakeApplications) ResolveWorkspace(_ context.Context, p authorization.Principal, app string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked || p.UserID != "user" || app != "app_a" {
		return "", authorization.ErrNotFound
	}
	return "ws_a", nil
}
func (f *fakeApplications) Get(_ context.Context, p authorization.Principal, ws, app, name string) (application.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastApp = app
	f.lastWorkspace = ws
	if f.revoked || app != "app_a" || ws != "ws_a" {
		return application.View{}, authorization.ErrNotFound
	}
	return application.View{ID: app, WorkspaceID: ws, Name: "test", Capabilities: []string{"application:read"}}, nil
}

type fakeConfig struct {
	mu    sync.Mutex
	calls int
	saved map[string]runtimeconfig.Input
}

func (f *fakeConfig) Get(context.Context, authorization.Principal, string, string) (runtimeconfig.View, error) {
	return runtimeconfig.View{Variables: []runtimeconfig.Variable{{Name: "SECRET", Value: "do-not-return", Sensitive: true, Configured: true}}, Files: []runtimeconfig.ConfigFile{{Path: "/app/secret", Content: "do-not-return", Sensitive: true, Configured: true}}}, nil
}
func (f *fakeConfig) Save(_ context.Context, _ authorization.Principal, _, _ string, in runtimeconfig.Input) (runtimeconfig.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.saved == nil {
		f.saved = map[string]runtimeconfig.Input{}
	}
	if prev, ok := f.saved[in.IdempotencyKey]; ok && prev.Type != in.Type {
		return runtimeconfig.View{}, authorization.ErrConflict
	}
	f.saved[in.IdempotencyKey] = in
	return runtimeconfig.View{Revision: 1}, nil
}

type fakeBuild struct{ last build.CreateInput }

func (*fakeBuild) GetConfig(context.Context, authorization.Principal, string, string) (build.ConfigView, error) {
	return build.ConfigView{}, nil
}
func (*fakeBuild) CreatePreview(context.Context, authorization.Principal, string, string, build.CreateInput) (build.Preview, error) {
	return build.Preview{}, nil
}
func (f *fakeBuild) CreateBuild(_ context.Context, _ authorization.Principal, _, _ string, v build.CreateInput) (build.View, error) {
	f.last = v
	return build.View{ID: "bld_1", State: build.Queued}, nil
}
func (*fakeBuild) Get(_ context.Context, _ authorization.Principal, _, _, id string) (build.View, error) {
	if id != "bld_1" {
		return build.View{}, authorization.ErrNotFound
	}
	return build.View{ID: id, State: build.Queued}, nil
}
func (*fakeBuild) List(context.Context, authorization.Principal, string, string, int, string) (build.Page, error) {
	return build.Page{}, nil
}
func (*fakeBuild) ReadLogs(context.Context, authorization.Principal, string, string, string, int64, int) (build.Logs, error) {
	return build.Logs{}, nil
}
func (*fakeBuild) Cancel(_ context.Context, _ authorization.Principal, _, _, id string) (build.View, error) {
	return build.View{ID: id, State: build.Canceling}, nil
}
func (f *fakeBuild) Retry(_ context.Context, _ authorization.Principal, _, _, id string, v build.CreateInput) (build.View, error) {
	f.last = v
	return build.View{ID: "bld_retry", State: build.Queued}, nil
}

type authRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (t authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

func fixture(t *testing.T, scopeList []string) (*httptest.Server, *fakeApplications, *fakeConfig, *fakeBuild) {
	t.Helper()
	apps := &fakeApplications{}
	configs := &fakeConfig{}
	builds := &fakeBuild{}
	auth := func(_ context.Context, token, resource string) (authorization.Principal, error) {
		if token != "good" || resource != "https://public.example/mcp/apps/app_a" {
			return authorization.Principal{}, authorization.ErrUnauthenticated
		}
		return authorization.Principal{UserID: "user", AuthKind: "oauth", OAuthGrantID: "grant", OAuthTokenSignature: "signature", Audience: resource, ApplicationIDs: []string{"app_a"}, Scopes: scopeList}, nil
	}
	h, err := NewHTTPHandler(Config{PublicBaseURL: "https://public.example", Authenticate: auth}, Services{Applications: apps, RuntimeConfigs: configs, Builds: builds})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(h), apps, configs, builds
}
func connectClient(t *testing.T, url, token string) *sdk.ClientSession {
	t.Helper()
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: authRoundTripper{token, http.DefaultTransport}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("SDK initialize: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func call(t *testing.T, s *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	v, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return v
}
func contentJSON(t *testing.T, v *sdk.CallToolResult) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func allScopes() []string {
	return []string{"application:read", "config:read", "config:write", "source:read", "build:read", "build:write", "logs:read", "deployment:read", "deployment:write", "runtime:read", "runtime:write"}
}

func TestHTTPAuthenticateResourceAndMetadata(t *testing.T) {
	ts, _, _, _ := fixture(t, allScopes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + metadataPrefix + "app_a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"resource":"https://public.example/mcp/apps/app_a"`)) {
		t.Fatalf("metadata: %d %s", resp.StatusCode, body)
	}
	for _, tc := range []struct {
		path, auth string
		code       int
	}{
		{resourcePrefix + "app_a", "", 401}, {resourcePrefix + "app_a", "Bearer bad", 401}, {resourcePrefix + "app_b", "Bearer good", 401},
		{resourcePrefix + "app_a?access_token=good", "Bearer good", 400}, {resourcePrefix + "bad%25path", "Bearer good", 400},
	} {
		req, _ := http.NewRequest("POST", ts.URL+tc.path, strings.NewReader("{}"))
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Errorf("%s: got %d want %d", tc.path, resp.StatusCode, tc.code)
		}
		if tc.code == 401 && !strings.Contains(resp.Header.Get("WWW-Authenticate"), metadataPrefix) {
			t.Error("缺少 resource_metadata")
		}
	}
}
func TestSDKToolsIsolationSchemaAndRevocation(t *testing.T) {
	ts, apps, _, _ := fixture(t, allScopes())
	defer ts.Close()
	s := connectClient(t, ts.URL+resourcePrefix+"app_a", "good")
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*sdk.Tool{}
	for _, tool := range list.Tools {
		seen[tool.Name] = tool
	}
	for _, name := range []string{"app_get", "config_get", "config_save", "build_create", "build_cancel", "build_retry", "build_logs"} {
		if seen[name] == nil {
			t.Errorf("缺少工具 %s", name)
		}
	}
	if seen["source_preview"] != nil {
		t.Fatal("远程不允许读取本地文件")
	}
	if a := seen["build_create"].Annotations; a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
		t.Fatalf("不正确标注: %+v", a)
	}
	got := call(t, s, "app_get", map[string]any{})
	if got.IsError || !strings.Contains(contentJSON(t, got), "app_a") {
		t.Fatalf("app_get: %+v", got)
	}
	if got = call(t, s, "app_get", map[string]any{"applicationId": "app_b"}); !got.IsError {
		t.Fatal("未知参数必须拒绝")
	}
	if apps.lastApp != "app_a" || apps.lastWorkspace != "ws_a" {
		t.Fatal("应用或空间未固定")
	}
	got = call(t, s, "config_get", map[string]any{})
	if got.IsError || strings.Contains(contentJSON(t, got), "do-not-return") {
		t.Fatal("敏感配置泄漏")
	}
	apps.mu.Lock()
	apps.revoked = true
	apps.mu.Unlock()
	_, err = s.CallTool(context.Background(), &sdk.CallToolParams{Name: "app_get", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("撤权后下一 HTTP 请求必须失败")
	}
}
func TestScopesAndWriteInputs(t *testing.T) {
	ts, _, _, _ := fixture(t, []string{"application:read"})
	defer ts.Close()
	s := connectClient(t, ts.URL+resourcePrefix+"app_a", "good")
	got := call(t, s, "build_cancel", map[string]any{"id": "bld_1"})
	if !got.IsError || !strings.Contains(contentJSON(t, got), "INSUFFICIENT_SCOPE") {
		t.Fatalf("未报告缺失范围: %s", contentJSON(t, got))
	}
	ts2, _, _, builds := fixture(t, allScopes())
	defer ts2.Close()
	s2 := connectClient(t, ts2.URL+resourcePrefix+"app_a", "good")
	in := map[string]any{"sourceSnapshotId": "src_1", "configRevision": 0, "expectedPolicyRevision": 1, "overrides": map[string]any{}, "idempotencyKey": "key-1", "autoDeploy": false, "runtimeConfigRevision": 0, "expectedServingRelease": ""}
	if v := call(t, s2, "build_create", in); v.IsError || builds.last.IdempotencyKey != "key-1" {
		t.Fatalf("创建未传入完整绑定: %s", contentJSON(t, v))
	}
	if v := call(t, s2, "build_retry", map[string]any{"id": "bld_1", "input": in}); v.IsError {
		t.Fatalf("重试失败: %s", contentJSON(t, v))
	}
	if v := call(t, s2, "build_cancel", map[string]any{"id": "bld_1"}); v.IsError || !strings.Contains(contentJSON(t, v), "CANCELING") {
		t.Fatalf("取消状态错误: %s", contentJSON(t, v))
	}
	delete(in, "expectedServingRelease")
	if v := call(t, s2, "build_create", in); !v.IsError {
		t.Fatal("缺失预期 serving 不应允许")
	}
}
func TestNewHandlerRejectsInvalidConfiguration(t *testing.T) {
	apps := &fakeApplications{}
	auth := func(context.Context, string, string) (authorization.Principal, error) {
		return authorization.Principal{}, errors.New("invalid")
	}
	for _, base := range []string{"http://public.example", "https://user:pass@example.com", "https://example.com/sub", "https://example.com?token=x"} {
		if _, err := NewHTTPHandler(Config{PublicBaseURL: base, Authenticate: auth}, Services{Applications: apps}); err == nil {
			t.Fatalf("错误接受 %s", base)
		}
	}
	if _, err := NewHTTPHandler(Config{PublicBaseURL: "https://example.com"}, Services{Applications: apps}); err == nil {
		t.Fatal("必须提供认证器")
	}
}
func TestLocalPreviewRequiresAllowRoots(t *testing.T) {
	if _, err := NewLocalServer(LocalConfig{}); err == nil {
		t.Fatal("必须提供授权根")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server, err := NewLocalServer(LocalConfig{AllowRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := sdk.NewInMemoryTransports()
	if _, err = server.Connect(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	s, err := client.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v := call(t, s, "source_preview", map[string]any{"root": root})
	if v.IsError || strings.Contains(contentJSON(t, v), root) {
		t.Fatalf("预览失败或泄露根目录：%s", contentJSON(t, v))
	}
	outside := t.TempDir()
	if v = call(t, s, "source_preview", map[string]any{"root": outside}); !v.IsError {
		t.Fatal("越过授权目录")
	}
}
