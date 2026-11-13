package transport

import (
	"context"
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
	"github.com/redis/go-redis/v9"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/identity"
	"github.com/sunshanpeng/yummy/internal/platform/session"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"go.uber.org/zap"
)

func TestIdentityHTTPRealRedisCSRFAndSessionLifecycle(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("未设置TEST_REDIS_ADDR")
	}
	db := testkit.OpenDatabase(t)
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewService(db, access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InitializeAdmin(context.Background(), "admin", "http-secure-password"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	base := "http://" + server.Listener.Addr().String()
	cookieKey := base64.StdEncoding.EncodeToString([]byte("12345678901234567890123456789012"))
	csrfKey := base64.StdEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwxzy123456"))
	manager, err := session.New(session.Config{RedisAddress: address, PublicBaseURL: base, CookieKey: cookieKey, CSRFKey: csrfKey, AllowLocalHTTP: true}, service, access)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	path, handler := NewIdentityHandler(service, manager, zap.NewNop())
	mux := http.NewServeMux()
	mux.Handle("/api"+path, http.StripPrefix("/api", handler))
	server.Config.Handler = manager.Middleware(mux)
	server.Start()
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	api := platformv1connect.NewIdentityServiceClient(client, base+"/api")
	ctx := context.Background()
	bootstrap, err := client.Get(base + "/auth/csrf")
	if err != nil {
		t.Fatal(err)
	}
	var csrfResult struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(bootstrap.Body).Decode(&csrfResult); err != nil {
		t.Fatal(err)
	}
	_ = bootstrap.Body.Close()
	initialRequest := connect.NewRequest(&platformv1.GetSessionRequest{})
	initialRequest.Header().Set("X-CSRF-Token", csrfResult.CSRFToken)
	current, err := api.GetSession(ctx, initialRequest)
	if err != nil {
		t.Fatal(err)
	}
	if current.Msg.User != nil || current.Msg.CsrfToken == "" {
		t.Fatal("匿名会话应仅返回CSRF")
	}
	if _, err := api.Login(ctx, connect.NewRequest(&platformv1.LoginRequest{Username: "admin", Password: "http-secure-password"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("无CSRF登录未拒绝: %v", err)
	}
	login := connect.NewRequest(&platformv1.LoginRequest{Username: "admin", Password: "http-secure-password"})
	login.Header().Set("X-CSRF-Token", current.Msg.CsrfToken)
	logged, err := api.Login(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	if logged.Msg.User == nil {
		t.Fatal("登录未返回用户")
	}
	currentReq := connect.NewRequest(&platformv1.GetSessionRequest{})
	currentReq.Header().Set("X-CSRF-Token", current.Msg.CsrfToken)
	current, err = api.GetSession(ctx, currentReq)
	if err != nil {
		t.Fatal(err)
	}
	if current.Msg.User == nil {
		t.Fatal("Cookie会话未持久化")
	}
	u, _ := url.Parse(base)
	var oldCookie *http.Cookie
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "yummy_session" {
			oldCookie = cookie
		}
	}
	if oldCookie == nil {
		t.Fatal("缺少会话Cookie")
	}
	// Cookie不能保存用户名或身份JSON；服务端Redis记录才承载身份。
	if strings.Contains(oldCookie.Value, "admin") || strings.Contains(oldCookie.Value, logged.Msg.User.Id) {
		t.Fatal("Cookie泄露身份内容")
	}
	rawURL := address
	if !strings.Contains(rawURL, "://") {
		rawURL = "redis://" + rawURL
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(options)
	t.Cleanup(func() { _ = rdb.Close() })
	keys, err := rdb.Keys(ctx, "yummy:session:*").Result()
	if err != nil || len(keys) == 0 {
		t.Fatalf("Redis没有服务端会话: %v", err)
	}
	found := false
	for _, key := range keys {
		payload, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), logged.Msg.User.Id) {
			found = true
		}
	}
	if !found {
		t.Fatal("服务端会话未保存用户身份")
	}

	// 对外JSON含未知托管字段时必须明确拒绝，不能静默丢弃。
	request, err := http.NewRequest(http.MethodPost, base+"/api/platform.v1.IdentityService/Login", strings.NewReader(`{"username":"admin","password":"http-secure-password","nodeId":"override"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("X-CSRF-Token", current.Msg.CsrfToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("未知字段未拒绝: %d %s", response.StatusCode, body)
	}

	cross, err := http.NewRequest(http.MethodPost, base+"/api/platform.v1.IdentityService/Logout", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	cross.Header.Set("Content-Type", "application/json")
	cross.Header.Set("Origin", "https://evil.test")
	cross.Header.Set("X-CSRF-Token", current.Msg.CsrfToken)
	response, err = client.Do(cross)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("跨站写未拒绝")
	}

	// Authorization存在即不借用Cookie，即使Cookie身份有效。
	invalid := connect.NewRequest(&platformv1.GetSessionRequest{})
	invalid.Header().Set("Authorization", "Bearer invalid")
	if _, err := api.GetSession(ctx, invalid); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("无效Bearer借用Cookie: %v", err)
	}

	logout := connect.NewRequest(&platformv1.LogoutRequest{})
	logout.Header().Set("X-CSRF-Token", current.Msg.CsrfToken)
	if _, err := api.Logout(ctx, logout); err != nil {
		t.Fatal(err)
	}
	currentReq = connect.NewRequest(&platformv1.GetSessionRequest{})
	currentReq.Header().Set("X-CSRF-Token", current.Msg.CsrfToken)
	current, err = api.GetSession(ctx, currentReq)
	if err != nil {
		t.Fatal(err)
	}
	if current.Msg.User != nil {
		t.Fatal("退出后仍有身份")
	}
	// 重放退出前Cookie也必须无身份。
	plainClient := &http.Client{}
	replay, _ := http.NewRequest(http.MethodPost, base+"/api/platform.v1.IdentityService/GetSession", strings.NewReader(`{}`))
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("X-CSRF-Token", current.Msg.CsrfToken)
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "yummy_csrf" {
			replay.AddCookie(cookie)
		}
	}
	replay.AddCookie(oldCookie)
	response, err = plainClient.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["user"] != nil {
		t.Fatalf("退出旧cookie可重放: %v", result)
	}
}
