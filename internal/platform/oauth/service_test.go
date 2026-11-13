package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/identity"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"gorm.io/gorm"
)

const baseURL = "https://yummy.example"
const callbackURL = "http://127.0.0.1:45678/callback"
const verifier = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHijklmn"

type environment struct {
	db              *gorm.DB
	service         *Service
	access          *authorization.Access
	identity        *identity.Service
	p, viewer, root authorization.Principal
}

func setup(t *testing.T) *environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, value := range []any{
		&model.User{ID: "dev", Username: "dev", PasswordHash: "hash"}, &model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash"}, &model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true},
		&model.Workspace{ID: "space", Name: "空间", Slug: "space"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "dev", Role: "member"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: "member"},
		&model.Application{ID: "app", WorkspaceID: "space", Name: "app"}, &model.Application{ID: "other", WorkspaceID: "space", Name: "other"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app", UserID: "dev", Role: "developer"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app", UserID: "viewer", Role: "viewer"},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.NewService(db, access)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, access, ids, Config{PublicBaseURL: baseURL, HMACSecret: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	access.SetOAuthVerifier(service.VerifyPrincipal)
	return &environment{db: db, service: service, access: access, identity: ids, p: authorization.Principal{UserID: "dev", AuthVersion: 1}, viewer: authorization.Principal{UserID: "viewer", AuthVersion: 1}, root: authorization.Principal{UserID: "root", AuthVersion: 1}}
}
func authorizeQuery(resource, scopes string) url.Values {
	hash := sha256.Sum256([]byte(verifier))
	return url.Values{"response_type": {"code"}, "client_id": {CLIClientID}, "redirect_uri": {callbackURL}, "scope": {scopes}, "state": {"high-entropy-user-client-state-12345"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "resource": {resource}}
}
func request(p authorization.Principal, method, target string, form url.Values) *http.Request {
	var req *http.Request
	if form == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", baseURL)
	if method == http.MethodPost {
		req.Header.Set("X-Yummy-Response-Mode", "manual")
	}
	if p.UserID != "" {
		req = req.WithContext(authorization.WithPrincipal(req.Context(), p))
	}
	return req
}
func (e *environment) code(t *testing.T, p authorization.Principal, resource, scopes string, offline bool) string {
	t.Helper()
	first := httptest.NewRecorder()
	e.service.Authorize(first, request(p, "GET", baseURL+"/oauth/authorize?"+authorizeQuery(resource, scopes).Encode(), nil))
	if first.Code != http.StatusSeeOther {
		t.Fatalf("authorize=%d %s", first.Code, first.Body.String())
	}
	viewResponse := httptest.NewRecorder()
	viewURL := strings.Replace(first.Header().Get("Location"), "/authorize?", "/oauth/consent?", 1)
	e.service.Consent(viewResponse, request(p, "GET", baseURL+viewURL, nil))
	if viewResponse.Code != 200 {
		t.Fatalf("consent view=%d %s", viewResponse.Code, viewResponse.Body.String())
	}
	var view ConsentView
	if err := json.Unmarshal(viewResponse.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"request_id": {view.RequestID}, "csrf_token": {view.CSRFToken}, "decision": {"allow"}, "application_id": {"app"}}
	if offline {
		form.Set("offline_access", "true")
	}
	consent := httptest.NewRecorder()
	e.service.Consent(consent, request(p, "POST", baseURL+"/oauth/consent", form))
	if consent.Code != 302 && consent.Code != 303 {
		t.Fatalf("consent=%d %s", consent.Code, consent.Body.String())
	}
	redirect, err := url.Parse(consent.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Query().Get("iss") != baseURL || redirect.Query().Get("state") != "high-entropy-user-client-state-12345" || redirect.Query().Get("code") == "" {
		t.Fatalf("callback=%s", redirect)
	}
	return redirect.Query().Get("code")
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
}

func (e *environment) token(t *testing.T, form url.Values) (int, tokenResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	e.service.Token(w, request(authorization.Principal{}, "POST", baseURL+"/oauth/token", form))
	var out tokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("token JSON=%s %v", w.Body.String(), err)
	}
	return w.Code, out
}
func exchange(code, resource string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {CLIClientID}, "redirect_uri": {callbackURL}, "code": {code}, "code_verifier": {verifier}, "resource": {resource}}
}
func refresh(token, resource string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {CLIClientID}, "refresh_token": {token}, "resource": {resource}}
}

func TestPKCEAuthorizationPersistenceAndAudience(t *testing.T) {
	e := setup(t)
	code := e.code(t, e.p, baseURL+"/api", "application:read build:read source:upload offline_access", true)
	status, tokens := e.token(t, exchange(code, baseURL+"/api"))
	if status != 200 || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatalf("token=%d %+v", status, tokens)
	}
	p, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api")
	if err != nil || p.UserID != "dev" || p.AuthKind != "oauth" || len(p.ApplicationIDs) != 1 || p.ApplicationIDs[0] != "app" {
		t.Fatalf("principal=%+v %v", p, err)
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/mcp/apps/app"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("API token可用于MCP: %v", err)
	}
	restarted, err := NewService(e.db, e.access, e.identity, Config{PublicBaseURL: baseURL, HMACSecret: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	e.access.SetOAuthVerifier(restarted.VerifyPrincipal)
	if _, err := restarted.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); err != nil {
		t.Fatalf("重启失效: %v", err)
	}
	var records []protocolRecord
	if err := e.db.Find(&records).Error; err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if bytes.Contains(record.Payload, []byte(tokens.AccessToken)) || bytes.Contains(record.Payload, []byte(tokens.RefreshToken)) || bytes.Contains(record.Payload, []byte(verifier)) {
			t.Fatal("持久化完整凭证或verifier")
		}
	}
	status, replayed := e.token(t, exchange(code, baseURL+"/api"))
	if status == 200 || replayed.Error == "" {
		t.Fatal("授权码重放成功")
	}
	if _, err := restarted.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("code重放未撤销族: %v", err)
	}
}
func TestPKCEAndResourceNegativeCases(t *testing.T) {
	e := setup(t)
	for _, kind := range []string{"缺失verifier", "错误verifier", "resource", "redirect", "scope"} {
		t.Run(kind, func(t *testing.T) {
			code := e.code(t, e.p, baseURL+"/api", "application:read build:read", false)
			form := exchange(code, baseURL+"/api")
			switch kind {
			case "缺失verifier":
				form.Del("code_verifier")
			case "错误verifier":
				form.Set("code_verifier", strings.Repeat("z", 60))
			case "resource":
				form.Set("resource", baseURL+"/mcp/apps/app")
			case "redirect":
				form.Set("redirect_uri", "http://127.0.0.1:45678/other")
			case "scope":
				form.Set("scope", "config:write")
			}
			status, response := e.token(t, form)
			if status == 200 || response.Error == "" {
				t.Fatalf("负向成功: %d %+v", status, response)
			}
			status, response = e.token(t, exchange(code, baseURL+"/api"))
			if status != 200 || response.AccessToken == "" {
				t.Fatalf("失败污染合法code: %d %+v", status, response)
			}
			if response.RefreshToken != "" {
				t.Fatal("未请求offline也发refresh")
			}
		})
	}
}
func TestRefreshReuseConcurrentFamilyAndNoExpansion(t *testing.T) {
	e := setup(t)
	code := e.code(t, e.p, baseURL+"/api", "application:read build:read offline_access", true)
	status, first := e.token(t, exchange(code, baseURL+"/api"))
	if status != 200 {
		t.Fatalf("initial %+v", first)
	}
	expanded := refresh(first.RefreshToken, baseURL+"/api")
	expanded.Set("scope", "build:write")
	if status, _ := e.token(t, expanded); status == 200 {
		t.Fatal("refresh扩大scope")
	}
	if status, _ := e.token(t, refresh(first.RefreshToken, baseURL+"/mcp/apps/app")); status == 200 {
		t.Fatal("refresh扩大resource")
	}
	status, second := e.token(t, refresh(first.RefreshToken, baseURL+"/api"))
	if status != 200 || second.RefreshToken == first.RefreshToken || second.RefreshToken == "" {
		t.Fatalf("rotate=%d %+v", status, second)
	}
	if status, _ := e.token(t, refresh(first.RefreshToken, baseURL+"/api")); status == 200 {
		t.Fatal("refresh重用成功")
	}
	if _, err := e.service.Authenticate(context.Background(), second.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("重用未全族吊销: %v", err)
	}
	// 新授权的同一刷新并发只能一次成功，重用检测使新值也失效。
	code = e.code(t, e.p, baseURL+"/api", "application:read offline_access", true)
	_, initial := e.token(t, exchange(code, baseURL+"/api"))
	start := make(chan struct{})
	out := make(chan int, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			status, _ := e.token(t, refresh(initial.RefreshToken, baseURL+"/api"))
			out <- status
		}()
	}
	close(start)
	wait.Wait()
	close(out)
	good := 0
	for status := range out {
		if status == 200 {
			good++
		}
	}
	if good != 1 {
		t.Fatalf("刷新并发成功次数%d", good)
	}
}
func TestOwnershipRevocationAndCurrentRole(t *testing.T) {
	e := setup(t)
	code := e.code(t, e.p, baseURL+"/api", "application:read build:write", false)
	_, tokens := e.token(t, exchange(code, baseURL+"/api"))
	p, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.Model(&model.ApplicationMember{}).Where("application_id='app' AND user_id='dev'").Update("role", "viewer").Error; err != nil {
		t.Fatal(err)
	}
	err = e.access.Do(context.Background(), p, false, func(tx *gorm.DB, current authorization.Principal) error {
		_, _, err := e.access.Application(tx, current, "space", "app", authorization.BuildWrite)
		return err
	})
	if !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("降级后仍构建:%v", err)
	}
	list, err := e.service.ListConnections(context.Background(), e.p)
	if err != nil || len(list) != 1 {
		t.Fatalf("connections %+v %v", list, err)
	}
	if err := e.service.RevokeConnection(context.Background(), e.root, list[0].ID); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("实例admin可撤别人个人授权:%v", err)
	}
	if err := e.service.RevokeConnection(context.Background(), e.p, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Authenticate(context.Background(), tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("撤销未即时:%v", err)
	}
}
func TestAuthorizeDoesNotTrustHostOrInvalidCallback(t *testing.T) {
	e := setup(t)
	for _, kind := range []string{"缺PKCE", "plain", "无state", "回调"} {
		query := authorizeQuery(baseURL+"/api", "application:read")
		switch kind {
		case "缺PKCE":
			query.Del("code_challenge")
		case "plain":
			query.Set("code_challenge_method", "plain")
		case "无state":
			query.Del("state")
		case "回调":
			query.Set("redirect_uri", "https://attacker.example/callback")
		}
		w := httptest.NewRecorder()
		e.service.Authorize(w, request(e.p, "GET", baseURL+"/oauth/authorize?"+query.Encode(), nil))
		if strings.Contains(w.Header().Get("Location"), "/authorize?") {
			t.Fatalf("接受非法授权%s", kind)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "https://attacker.example/.well-known/oauth-authorization-server", nil)
	e.service.Metadata(w, r)
	if strings.Contains(w.Body.String(), "attacker.example") || !strings.Contains(w.Body.String(), baseURL) {
		t.Fatalf("Host污染元数据:%s", w.Body.String())
	}
	anonymous := httptest.NewRecorder()
	e.service.Authorize(anonymous, request(authorization.Principal{}, "GET", baseURL+"/oauth/authorize?"+authorizeQuery(baseURL+"/api", "application:read").Encode(), nil))
	if !strings.HasPrefix(anonymous.Header().Get("Location"), "/login?return_request=") {
		t.Fatalf("未保存登录请求:%s", anonymous.Header().Get("Location"))
	}
}
func TestCIMDNetworkRestrictions(t *testing.T) {
	for _, raw := range []string{"http://client.example/meta", "https://127.0.0.1/client.json", "https://[::1]/client.json", "https://169.254.169.254/latest", "https://10.0.0.1/meta", "https://user:pass@client.example/meta"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := fetchCIMD(ctx, raw)
		cancel()
		if err == nil {
			t.Fatalf("允许危险CIMD:%s", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.2.3.4", "169.254.169.254", "100.64.0.1", "192.0.2.1", "::1", "fc00::1", "2001:db8::1"} {
		if publicIP(net.ParseIP(raw)) {
			t.Fatalf("非公网IP通过:%s", raw)
		}
	}
}
