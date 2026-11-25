package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/oauth"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
)

// 真正的 HTTP 适配、登录会话、OAuth 协议和业务授权共同运行；PG schema 独立隔离。
// 这个测试不执行 Kubernetes 或公网 HTTPS。
func TestOAuthHTTPAndMCPWiring(t *testing.T) {
	redis := os.Getenv("TEST_REDIS_ADDR")
	if redis == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
	}
	db := testkit.OpenDatabase(t)
	web := httptest.NewUnstartedServer(nil)
	base := "http://" + web.Listener.Addr().String()
	key := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	product, err := openWithDatabase(context.Background(), db, Config{RedisAddress: redis, PublicBaseURL: base, SessionKey: key(71), CSRFKey: key(72), MasterKey: key(73), OAuthKey: key(74), SourceRoot: t.TempDir(), AllowLocalHTTP: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = product.Close() })
	admin, err := product.Identity.InitializeAdmin(context.Background(), "oauthadmin", "oauth-integration-password")
	if err != nil {
		t.Fatal(err)
	}
	web.Config.Handler = product.Handler
	web.Start()
	t.Cleanup(web.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(base + "/auth/csrf")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap struct {
		CSRF string `json:"csrfToken"`
	}
	if err = json.NewDecoder(response.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	csrf := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			r.Header().Set("X-CSRF-Token", bootstrap.CSRF)
			return next(ctx, r)
		}
	}))
	identity := platformv1connect.NewIdentityServiceClient(client, base+"/api", csrf)
	if _, err = identity.Login(context.Background(), connect.NewRequest(&platformv1.LoginRequest{Username: "oauthadmin", Password: "oauth-integration-password"})); err != nil {
		t.Fatal(err)
	}
	spaces := platformv1connect.NewWorkspaceServiceClient(client, base+"/api", csrf)
	ws, err := spaces.CreateWorkspace(context.Background(), connect.NewRequest(&platformv1.CreateWorkspaceRequest{Name: "OAuth 空间", Slug: "oauth-integration"}))
	if err != nil {
		t.Fatal(err)
	}
	apps := platformv1connect.NewApplicationServiceClient(client, base+"/api", csrf)
	app, err := apps.CreateApplication(context.Background(), connect.NewRequest(&platformv1.CreateApplicationRequest{WorkspaceId: ws.Msg.Workspace.Id, Name: "oauth-app", IdempotencyKey: "create-oauth-app"}))
	if err != nil {
		t.Fatal(err)
	}
	appID, workspaceID := app.Msg.Application.Id, ws.Msg.Workspace.Id
	verifier := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHijklmn"
	hash := sha256.Sum256([]byte(verifier))
	callback := "http://127.0.0.1:45678/callback"
	login := func(clientID, resource, scope string) string {
		t.Helper()
		query := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback}, "scope": {scope}, "state": {"integration-long-random-state-12345"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "resource": {resource}}
		res, err := client.Get(base + "/oauth/authorize?" + query.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/authorize?request_id=") {
			t.Fatalf("授权跳转: %d %q", res.StatusCode, res.Header.Get("Location"))
		}
		location, _ := url.Parse(res.Header.Get("Location"))
		res, err = client.Get(base + "/oauth/consent?request_id=" + url.QueryEscape(location.Query().Get("request_id")))
		if err != nil {
			t.Fatal(err)
		}
		var view oauth.ConsentView
		if err = json.NewDecoder(res.Body).Decode(&view); err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || view.FormCSRFToken == "" || view.CSRFToken == "" {
			t.Fatalf("同意数据: %d %+v", res.StatusCode, view)
		}
		form := url.Values{"request_id": {view.RequestID}, "csrf_token": {view.CSRFToken}, "decision": {"allow"}, "application_id": {appID}}
		makeConsent := func(withHeader bool) *http.Response {
			r, _ := http.NewRequest(http.MethodPost, base+"/oauth/consent", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", base)
			r.Header.Set("X-CSRF-Token", view.FormCSRFToken)
			if withHeader {
				r.Header.Set("X-Yummy-Response-Mode", "manual")
			}
			result, e := client.Do(r)
			if e != nil {
				t.Fatal(e)
			}
			return result
		}
		res = makeConsent(false)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("未显式请求手动跳转: %d", res.StatusCode)
		}
		res = makeConsent(true)
		var decision struct {
			Redirect string `json:"redirect"`
		}
		if err = json.NewDecoder(res.Body).Decode(&decision); err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		redirect, err := url.Parse(decision.Redirect)
		if err != nil || res.StatusCode != 200 || redirect.Scheme+"://"+redirect.Host+redirect.Path != callback || redirect.Query().Get("iss") != base {
			t.Fatalf("授权回调: %d %q %v", res.StatusCode, decision.Redirect, err)
		}
		form = url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {redirect.Query().Get("code")}, "redirect_uri": {callback}, "code_verifier": {verifier}, "resource": {resource}}
		res, err = client.PostForm(base+"/oauth/token", form)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var token struct {
			Access string `json:"access_token"`
		}
		if err = json.NewDecoder(res.Body).Decode(&token); err != nil || res.StatusCode != 200 || token.Access == "" {
			t.Fatalf("兑换令牌: %d %+v %v", res.StatusCode, token, err)
		}
		return token.Access
	}
	token := login("yummy-cli", base+"/api", "application:read")
	bearer := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			r.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, r)
		}
	}))
	oauthApps := platformv1connect.NewApplicationServiceClient(http.DefaultClient, base+"/api", bearer)
	if _, err = oauthApps.GetApplication(context.Background(), connect.NewRequest(&platformv1.GetApplicationRequest{WorkspaceId: workspaceID, ApplicationId: appID})); err != nil {
		t.Fatalf("OAuth API 访问: %v", err)
	}
	oauthRuntime := platformv1connect.NewDeploymentServiceClient(http.DefaultClient, base+"/api", bearer)
	if _, err := oauthRuntime.GetRuntimeLogs(context.Background(), connect.NewRequest(&platformv1.GetRuntimeLogsRequest{WorkspaceId: workspaceID, ApplicationId: appID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("仅应用读取权限不能查看运行日志: %v", err)
	}
	if _, err := oauthRuntime.GetRuntimeMetrics(context.Background(), connect.NewRequest(&platformv1.GetRuntimeMetricsRequest{WorkspaceId: workspaceID, ApplicationId: appID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("仅应用读取权限不能查看运行指标: %v", err)
	}
	resource := base + "/mcp/apps/" + appID
	request, _ := http.NewRequest(http.MethodPost, resource, strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	res, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API token 跨资源访问 MCP: %d", res.StatusCode)
	}
	principal := authorization.Principal{UserID: admin.ID, AuthVersion: admin.AuthVersion}
	if err = product.OAuth.RegisterClient(context.Background(), principal, oauth.ClientInput{ID: "mcp-test", Name: "集成 MCP", RedirectURIs: []string{callback}, Scopes: []string{"application:read", "logs:read", "runtime:read"}, Resources: []string{resource}}); err != nil {
		t.Fatal(err)
	}
	mcpToken := login("mcp-test", resource, "application:read logs:read runtime:read")
	request, _ = http.NewRequest(http.MethodPost, resource, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	request.Header.Set("Authorization", "Bearer "+mcpToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	res, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 || !bytes.Contains(data, []byte("yummy-application")) {
		t.Fatalf("MCP initialize: %d %s", res.StatusCode, data)
	}
	for tool, field := range map[string]string{"runtime_logs": "resumeCursor", "runtime_metrics": "samples"} {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{}}})
		req, _ := http.NewRequest(http.MethodPost, resource, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+mcpToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(result, []byte(`"`+field+`"`)) || bytes.Contains(result, []byte(`"isError":true`)) {
			t.Fatalf("MCP %s: %d %s", tool, resp.StatusCode, result)
		}
	}
	connections, err := product.OAuth.ListConnections(context.Background(), principal)
	if err != nil || len(connections) != 2 {
		t.Fatalf("查询连接: %+v %v", connections, err)
	}
	listResponse, err := client.Get(base + "/oauth/connections")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Connections []oauth.Connection `json:"connections"`
	}
	if err = json.NewDecoder(listResponse.Body).Decode(&listed); err != nil || listResponse.StatusCode != http.StatusOK || len(listed.Connections) != 2 {
		t.Fatalf("HTTP 个人连接: %d %+v %v", listResponse.StatusCode, listed, err)
	}
	_ = listResponse.Body.Close()
	for _, connection := range connections {
		body, _ := json.Marshal(map[string]string{"id": connection.ID})
		req, _ := http.NewRequest(http.MethodPost, base+"/oauth/connections/revoke", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		req.Header.Set("X-CSRF-Token", bootstrap.CSRF)
		res, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("撤销连接: %d", res.StatusCode)
		}
	}
	if _, err = oauthApps.GetApplication(context.Background(), connect.NewRequest(&platformv1.GetApplicationRequest{WorkspaceId: workspaceID, ApplicationId: appID})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("撤销后的 OAuth 仍可访问: %v", err)
	}
}
