package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
)

func TestCIMDMetadataValidationWithoutCustomResourceExtension(t *testing.T) {
	id := "https://client.example/oauth-client.json"
	metadata := map[string]any{"client_id": id, "client_name": "第三方客户端", "redirect_uris": []string{"https://client.example/callback"}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}}
	raw, _ := json.Marshal(metadata)
	parsed, err := parseCIMD(id, raw)
	if err != nil || len(parsed.Scopes) == 0 || len(parsed.Resources) != 0 {
		t.Fatalf("标准CIMD被自定义resource阻塞:%+v %v", parsed, err)
	}
	for _, name := range []string{"id", "redirect", "auth", "grant", "response", "scope", "size"} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			_ = json.Unmarshal(raw, &value)
			switch name {
			case "id":
				value["client_id"] = "https://attacker.example/meta"
			case "redirect":
				value["redirect_uris"] = []string{"http://attacker.example/callback"}
			case "auth":
				value["token_endpoint_auth_method"] = "client_secret_post"
			case "grant":
				value["grant_types"] = []string{"password"}
			case "response":
				value["response_types"] = []string{"token"}
			case "scope":
				value["scope"] = "instance:admin"
			case "size":
				value["padding"] = strings.Repeat("x", 65<<10)
			}
			modified, _ := json.Marshal(value)
			if _, err := parseCIMD(id, modified); !errors.Is(err, ErrClient) {
				t.Fatalf("无效metadata通过:%v", err)
			}
		})
	}
}
func TestMCPResourceGrantCannotCrossAppOrAPI(t *testing.T) {
	e := setup(t)
	resource := baseURL + "/mcp/apps/app"
	if err := e.service.RegisterClient(context.Background(), e.root, ClientInput{ID: "mcp-client", Name: "受控MCP", RedirectURIs: []string{callbackURL}, Scopes: []string{"application:read", "build:read", "offline_access"}, Resources: []string{resource}}); err != nil {
		t.Fatal(err)
	}
	// 复用同一浏览器流程，客户端及资源都固定在服务端请求。
	query := authorizeQuery(resource, "application:read build:read offline_access")
	query.Set("client_id", "mcp-client")
	first := httptest.NewRecorder()
	e.service.Authorize(first, request(e.p, "GET", baseURL+"/oauth/authorize?"+query.Encode(), nil))
	if first.Code != 303 {
		t.Fatalf("authorize=%d %s", first.Code, first.Body.String())
	}
	viewResponse := httptest.NewRecorder()
	viewURL := strings.Replace(first.Header().Get("Location"), "/authorize?", "/oauth/consent?", 1)
	e.service.Consent(viewResponse, request(e.p, "GET", baseURL+viewURL, nil))
	var view ConsentView
	if err := json.Unmarshal(viewResponse.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"request_id": {view.RequestID}, "csrf_token": {view.CSRFToken}, "decision": {"allow"}, "offline_access": {"true"}}
	// 企图在同意页扩应用必须拒绝，且不得消费合法请求。
	form.Set("application_id", "other")
	denied := httptest.NewRecorder()
	e.service.Consent(denied, request(e.p, "POST", baseURL+"/oauth/consent", form))
	if denied.Code == 302 && strings.Contains(denied.Header().Get("Location"), "code=") {
		t.Fatal("扩MCP应用成功")
	}
	form.Set("application_id", "app")
	approved := httptest.NewRecorder()
	e.service.Consent(approved, request(e.p, "POST", baseURL+"/oauth/consent", form))
	callback, _ := url.Parse(approved.Header().Get("Location"))
	code := callback.Query().Get("code")
	if code == "" {
		t.Fatalf("合法MCP同意失败:%d %s", approved.Code, approved.Body.String())
	}
	exchangeForm := exchange(code, resource)
	exchangeForm.Set("client_id", "mcp-client")
	status, tokens := e.token(t, exchangeForm)
	if status != 200 {
		t.Fatalf("MCP token=%d %+v", status, tokens)
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, resource); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{baseURL + "/api", baseURL + "/mcp/apps/other"} {
		if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, wrong); !errors.Is(err, authorization.ErrUnauthenticated) {
			t.Fatalf("跨资源成功:%s %v", wrong, err)
		}
	}
}
func TestConsentCSRFOwnerAndTamperedParameters(t *testing.T) {
	e := setup(t)
	query := authorizeQuery(baseURL+"/api", "application:read build:read")
	first := httptest.NewRecorder()
	e.service.Authorize(first, request(e.p, "GET", baseURL+"/oauth/authorize?"+query.Encode(), nil))
	response := httptest.NewRecorder()
	viewURL := strings.Replace(first.Header().Get("Location"), "/authorize?", "/oauth/consent?", 1)
	e.service.Consent(response, request(e.p, "GET", baseURL+viewURL, nil))
	var view ConsentView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"request_id": {view.RequestID}, "csrf_token": {"wrong"}, "decision": {"allow"}, "application_id": {"app"}}
	denied := httptest.NewRecorder()
	e.service.Consent(denied, request(e.p, "POST", baseURL+"/oauth/consent", form))
	if denied.Code != 403 {
		t.Fatalf("错误CSRF通过:%d", denied.Code)
	}
	form.Set("csrf_token", view.CSRFToken)
	denied = httptest.NewRecorder()
	e.service.Consent(denied, request(e.root, "POST", baseURL+"/oauth/consent", form))
	if denied.Code != 403 {
		t.Fatalf("其他owner同意:%d", denied.Code)
	}
	form.Set("client_id", "attacker")
	form.Set("resource", baseURL+"/mcp/apps/other")
	form.Set("scope", "config:write")
	form.Set("redirect_uri", "https://attacker.example/callback")
	approved := httptest.NewRecorder()
	e.service.Consent(approved, request(e.p, "POST", baseURL+"/oauth/consent", form))
	callback, _ := url.Parse(approved.Header().Get("Location"))
	if callback.Host != "127.0.0.1:45678" || callback.Query().Get("code") == "" {
		t.Fatalf("客户端参数覆盖服务端请求:%s", callback)
	}
	_, tokens := e.token(t, exchange(callback.Query().Get("code"), baseURL+"/api"))
	p, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range p.Scopes {
		if scope == "config:write" {
			t.Fatal("CSRF表单扩scope")
		}
	}
}
func TestRevokeRequiresWholeTokenAndClientDisableImmediate(t *testing.T) {
	e := setup(t)
	code := e.code(t, e.p, baseURL+"/api", "application:read offline_access", true)
	_, tokens := e.token(t, exchange(code, baseURL+"/api"))
	// token签名后缀不秘密，伪造随机前段不能触发撤销。
	parts := strings.Split(tokens.AccessToken, ".")
	parts[0] = parts[0] + "forged"
	w := httptest.NewRecorder()
	e.service.Revoke(w, request(authorization.Principal{}, "POST", baseURL+"/oauth/revoke", url.Values{"client_id": {CLIClientID}, "token": {strings.Join(parts, ".")}}))
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); err != nil {
		t.Fatalf("签名后缀可吊销合法token:%v", err)
	}
	w = httptest.NewRecorder()
	e.service.Revoke(w, request(authorization.Principal{}, "POST", baseURL+"/oauth/revoke", url.Values{"client_id": {CLIClientID}, "token": {tokens.AccessToken}}))
	if w.Code != 200 {
		t.Fatalf("合法撤销=%d %s", w.Code, w.Body.String())
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("合法撤销无效:%v", err)
	}
	code = e.code(t, e.p, baseURL+"/api", "application:read offline_access", true)
	_, tokens = e.token(t, exchange(code, baseURL+"/api"))
	if err := e.service.SetClientActive(context.Background(), e.root, CLIClientID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("停client无效:%v", err)
	}
	if err := e.service.SetClientActive(context.Background(), e.root, CLIClientID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("重启client复活旧token:%v", err)
	}
}
func TestAuthVersionRevokesAndRefreshAbsoluteLifetime(t *testing.T) {
	e := setup(t)
	code := e.code(t, e.p, baseURL+"/api", "application:read offline_access", true)
	_, tokens := e.token(t, exchange(code, baseURL+"/api"))
	if err := e.db.Model(&model.User{}).Where("id=?", e.p.UserID).Update("auth_version", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("authVersion无效:%v", err)
	}
	if status, _ := e.token(t, refresh(tokens.RefreshToken, baseURL+"/api")); status == 200 {
		t.Fatal("失效账号仍refresh")
	}
	sess := &session{ExpiresAtAbsolute: time.Now().Add(time.Hour)}
	sess.SetExpiresAt("refresh_token", time.Now().Add(30*24*time.Hour))
	if sess.GetExpiresAt("refresh_token").After(sess.ExpiresAtAbsolute) {
		t.Fatal("refresh滑动超过授权最长期")
	}
}
