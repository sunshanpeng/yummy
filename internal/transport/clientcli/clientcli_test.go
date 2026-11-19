package clientcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
)

type testWorkspace struct {
	platformv1connect.UnimplementedWorkspaceServiceHandler
}

func (testWorkspace) ListWorkspaces(_ context.Context, r *connect.Request[platformv1.ListWorkspacesRequest]) (*connect.Response[platformv1.ListWorkspacesResponse], error) {
	if r.Header().Get("Authorization") != "Bearer sample-secret" || r.Header().Get("Cookie") != "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("bad auth"))
	}
	return connect.NewResponse(&platformv1.ListWorkspacesResponse{Workspaces: []*platformv1.Workspace{{Id: "w1", Name: "space"}}}), nil
}
func runCLI(ctx context.Context, dir, server string, args ...string) (string, string, error) {
	cmd := Command()
	var out, errout bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errout)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"--config-dir", dir, "--server", server, "--allow-local-http"}, args...))
	err := cmd.ExecuteContext(ctx)
	return out.String(), errout.String(), err
}

// testing.TempDir 的子目录受 umask 影响可能为 0755，配置目录须由客户端以 0700 创建。
func configDir(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "config") }

func TestWorkspaceConnectAndTokenIsolation(t *testing.T) {
	t.Setenv("YUMMY_TOKEN", "sample-secret")
	path, h := platformv1connect.NewWorkspaceServiceHandler(testWorkspace{})
	mux := http.NewServeMux()
	mux.Handle("/api"+path, http.StripPrefix("/api", h))
	server := httptest.NewServer(mux)
	defer server.Close()
	out, diagnostics, err := runCLI(context.Background(), configDir(t), server.URL, "workspace", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id":"w1"`) || strings.Contains(out, "sample-secret") || strings.Contains(diagnostics, "sample-secret") {
		t.Fatalf("unexpected output: %s %s", out, diagnostics)
	}
}
func TestConfigPermissionsLinksAndOrigin(t *testing.T) {
	dir := configDir(t)
	s := configStore{dir: dir}
	if err := s.update("main", func(p *profile) error { p.Server = "https://example.test"; p.Token = "private"; return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "main.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("profile mode %v %v", info, err)
	}
	if _, err = s.read("../bad"); err == nil {
		t.Fatal("traversal accepted")
	}
	if err = os.Chmod(filepath.Join(dir, "main.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = s.read("main"); err == nil {
		t.Fatal("world-readable accepted")
	}
	if err = os.Chmod(filepath.Join(dir, "main.json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("main.json", filepath.Join(dir, "sym.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.read("sym"); err == nil {
		t.Fatal("symlink accepted")
	}
	if err = os.Link(filepath.Join(dir, "main.json"), filepath.Join(dir, "hard.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.read("main"); err == nil {
		t.Fatal("hardlink accepted")
	}
	for _, raw := range []string{"http://example.com", "http://127.0.0.2", "https://user:pass@example.com", "https://example.com/path", "https://example.com?token=x"} {
		if _, err := validateServer(raw, true); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080", "https://example.com"} {
		if _, err := validateServer(raw, true); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://evil.test/transfer/a", "//evil.test/transfer/a", "/transfer/../evil", "/transfer/a?token=x", "/transfer/%2e%2e/evil"} {
		if _, err := transferURL("https://safe.test", raw); err == nil {
			t.Errorf("accepted transfer %q", raw)
		}
	}
}
func TestAtomicConcurrentProfileUpdates(t *testing.T) {
	s := configStore{dir: configDir(t)}
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.update("main", func(p *profile) error { p.Server = "https://example.test"; p.Workspace += "x"; return nil })
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.read("main")
	if err != nil || len(p.Workspace) != n {
		t.Fatalf("lost write: %q %v", p.Workspace, err)
	}
}

func TestOAuthPKCECallbackAndRotation(t *testing.T) {
	var base string
	var refreshes atomic.Int64
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token", "code_challenge_methods_supported": []string{"S256"}})
	})
	var challenge string
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		if r.Form.Get("resource") != base+"/api" || r.Form.Get("client_id") != "yummy-cli" {
			http.Error(w, "bad resource", 400)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("code") != "test-code" {
				http.Error(w, "pkce", 400)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"oauth-access","token_type":"Bearer","expires_in":3600,"refresh_token":"oauth-refresh"}`)
		case "refresh_token":
			refreshes.Add(1)
			if r.Form.Get("refresh_token") != "oauth-refresh" {
				http.Error(w, "reuse", 400)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"next-access","token_type":"Bearer","expires_in":3600,"refresh_token":"next-refresh"}`)
		default:
			http.Error(w, "grant", 400)
		}
	})
	var out, diagnostics bytes.Buffer
	cmd := Command()
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	dir := configDir(t)
	cmd.SetArgs([]string{"--config-dir", dir, "--server", base, "--allow-local-http", "auth", "login", "--no-browser", "--offline"})
	// 使用线程安全的管道读取授权 URL，不并发访问 bytes.Buffer。
	reader, writer := io.Pipe()
	cmd.SetErr(writer)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(context.Background()); _ = writer.Close() }()
	line := make(chan string, 1)
	go func() { b := make([]byte, 8192); n, _ := reader.Read(b); line <- string(b[:n]) }()
	var text string
	select {
	case text = <-line:
	case <-time.After(5 * time.Second):
		t.Fatal("missing authorize URL")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(text, "请在浏览器打开授权地址："))
	authURL, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := authURL.Query()
	challenge = q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || q.Get("resource") != base+"/api" || q.Get("state") == "" {
		t.Fatalf("bad authorize URL: %s", raw)
	}
	callback, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	bad := callback.Query()
	bad.Set("state", "wrong")
	bad.Set("iss", base)
	bad.Set("code", "test-code")
	callback.RawQuery = bad.Encode()
	res, err := http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("csrf accepted %d", res.StatusCode)
	}
	bad.Set("state", q.Get("state"))
	bad.Set("iss", "https://evil.test")
	callback.RawQuery = bad.Encode()
	res, err = http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal("issuer accepted")
	}
	bad.Set("iss", base)
	callback.RawQuery = bad.Encode()
	res, err = http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "oauth-access") || strings.Contains(out.String(), "oauth-refresh") {
		t.Fatal("token leaked")
	}
	s := configStore{dir: dir}
	err = s.update("default", func(p *profile) error { p.TokenExpiry = time.Now().Add(-time.Minute); return nil })
	if err != nil {
		t.Fatal(err)
	}
	app := &clientApp{store: s}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := app.refreshToken(context.Background(), "default", base)
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
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count %d", refreshes.Load())
	}
	p, err := s.read("default")
	if err != nil || p.Token != "next-access" || p.RefreshToken != "next-refresh" {
		t.Fatalf("bad rotation %v", err)
	}
}
