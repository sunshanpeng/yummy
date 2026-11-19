package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ginsessions "github.com/gin-contrib/sessions"
	ginredis "github.com/gin-contrib/sessions/redis"
	"github.com/gorilla/securecookie"
	gorillasessions "github.com/gorilla/sessions"
	goredis "github.com/redis/go-redis/v9"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/identity"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
)

type sessionFixture struct {
	manager *Manager
	server  *httptest.Server
	user    authorization.Principal
	client  *http.Client
	csrf    string
	cookies []*http.Cookie
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	address := os.Getenv("TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
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
	user := model.User{ID: "session-user", Username: "session-user", PasswordHash: "unused"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	manager, err := New(Config{
		RedisAddress: address, PublicBaseURL: "http://" + server.Listener.Addr().String(), AllowLocalHTTP: true,
		CookieKey: base64.StdEncoding.EncodeToString([]byte("12345678901234567890123456789012")),
		CSRFKey:   base64.StdEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwxzy123456")),
	}, service, access)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	f := &sessionFixture{manager: manager, server: server, user: authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion}}
	server.Config.Handler = manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch r.URL.Path {
		case "/login":
			err = manager.Establish(r.Context(), f.user)
		case "/logout":
			err = manager.Logout(r.Context())
		default:
			_, err = authorization.FromContext(r.Context())
		}
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, authorization.ErrUnauthenticated) {
				status = http.StatusUnauthorized
			}
			if errors.Is(err, ErrUnavailable) {
				status = http.StatusServiceUnavailable
			}
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.Start()
	f.client = server.Client()
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = manager.Close() })
	bootstrap := f.request(t, http.MethodGet, "/auth/csrf", nil)
	var body struct {
		Token string `json:"csrfToken"`
	}
	if err := json.NewDecoder(bootstrap.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	_ = bootstrap.Body.Close()
	f.csrf = body.Token
	f.cookies = bootstrap.Cookies()
	login := f.request(t, http.MethodPost, "/login", f.cookies)
	if login.StatusCode != http.StatusOK {
		t.Fatalf("测试登录失败: %d", login.StatusCode)
	}
	f.cookies = append(f.cookies, sessionCookie(t, login.Cookies()))
	_ = login.Body.Close()
	// 清理使用独立连接，故障用例关闭 Manager 的连接后也不会泄留会话。
	cleanupRedis := goredis.NewClient(manager.redis.Options())
	// 只清理本测试创建的随机会话，不影响共享 Redis 上的其他测试。
	t.Cleanup(func() {
		defer cleanupRedis.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, cookie := range f.cookies {
			if cookie.Name == cookieName && cookie.Value != "" {
				id := f.sessionID(t, cookie)
				if err := cleanupRedis.Del(ctx, sessionKeyPrefix+id).Err(); err != nil {
					t.Errorf("清理测试会话失败: %v", err)
				}
			}
		}
	})
	return f
}

func (f *sessionFixture) request(t *testing.T, method, path string, cookies []*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	if f.csrf != "" {
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func sessionCookie(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for index := len(cookies) - 1; index >= 0; index-- {
		if cookies[index].Name == cookieName {
			return cookies[index]
		}
	}
	t.Fatal("响应缺少会话 Cookie")
	return nil
}

func (f *sessionFixture) sessionID(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	store := f.manager.store
	if gate, ok := store.(*gatedSessionStore); ok {
		store = gate.Store
	}
	redisStore, err := ginredis.GetRedisStore(store)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := securecookie.DecodeMulti(cookieName, cookie.Value, &id, redisStore.Codecs...); err != nil {
		t.Fatal(err)
	}
	return id
}

// gatedSessionStore 暂停已从真实 Redis 读到旧身份的请求，精确重现 GET→DEL→续期次序。
// 不修改生产代码，也不依赖随机调度或 sleep 触发竞态。
type gatedSessionStore struct {
	ginsessions.Store
	loaded  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedSessionStore) Get(r *http.Request, name string) (*gorillasessions.Session, error) {
	value, err := s.Store.Get(r, name)
	if r.URL.Path == "/blocked" {
		s.once.Do(func() {
			close(s.loaded)
			select {
			case <-s.release:
			case <-r.Context().Done():
			}
		})
	}
	return value, err
}

func TestConcurrentLogoutAndRotationCannotResurrectSession(t *testing.T) {
	for _, action := range []string{"/logout", "/login"} {
		t.Run(strings.TrimPrefix(action, "/"), func(t *testing.T) {
			f := newSessionFixture(t)
			oldCookie := sessionCookie(t, f.cookies)
			oldID := f.sessionID(t, oldCookie)
			gate := &gatedSessionStore{Store: f.manager.store, loaded: make(chan struct{}), release: make(chan struct{})}
			f.manager.store = gate
			var release sync.Once
			unblock := func() { release.Do(func() { close(gate.release) }) }
			defer unblock()
			result := make(chan *http.Response, 1)
			failures := make(chan error, 1)
			req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/blocked", nil)
			for _, cookie := range f.cookies {
				req.AddCookie(cookie)
			}
			go func() {
				response, err := f.client.Do(req)
				if err != nil {
					failures <- err
					return
				}
				result <- response
			}()
			select {
			case <-gate.loaded:
			case err := <-failures:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("等待会话读取超时")
			}
			mutation := f.request(t, http.MethodPost, action, f.cookies)
			if mutation.StatusCode != http.StatusOK {
				t.Fatalf("会话变更失败: %d", mutation.StatusCode)
			}
			_ = mutation.Body.Close()
			if action == "/login" {
				fresh := sessionCookie(t, mutation.Cookies())
				if f.sessionID(t, fresh) == oldID {
					t.Fatal("登录未轮换会话标识")
				}
				f.cookies = append(f.cookies[:len(f.cookies)-1], fresh)
			}
			unblock()
			select {
			case response := <-result:
				_ = response.Body.Close()
				if response.StatusCode != http.StatusUnauthorized {
					t.Errorf("已删除会话的在途续期仍返回身份: %d", response.StatusCode)
				}
			case err := <-failures:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("等待会话续期超时")
			}
			exists, err := f.manager.redis.Exists(context.Background(), "yummy:session:"+oldID).Result()
			if err != nil || exists != 0 {
				t.Fatalf("已删除旧会话被并发续期复活: exists=%d err=%v", exists, err)
			}
			replay := f.request(t, http.MethodGet, "/me", []*http.Cookie{oldCookie})
			_ = replay.Body.Close()
			if replay.StatusCode != http.StatusUnauthorized {
				t.Fatalf("退出或轮换前 Cookie 可重放: %d", replay.StatusCode)
			}
			if action == "/login" {
				current := f.request(t, http.MethodGet, "/me", f.cookies)
				_ = current.Body.Close()
				if current.StatusCode != http.StatusOK {
					t.Fatalf("轮换后的新会话不可用: %d", current.StatusCode)
				}
			}
		})
	}
}

func TestMalformedCookieClearsWithoutBlockingLoginOrLogout(t *testing.T) {
	f := newSessionFixture(t)
	bad := []*http.Cookie{{Name: cookieName, Value: "invalid-signature"}}
	me := f.request(t, http.MethodGet, "/me", bad)
	_ = me.Body.Close()
	if me.StatusCode != http.StatusUnauthorized {
		t.Errorf("坏 Cookie 误报服务故障: %d", me.StatusCode)
	}
	if cookie := sessionCookie(t, me.Cookies()); cookie.MaxAge != -1 || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("坏 Cookie 未安全清除: %#v", cookie)
	}
	for _, action := range []string{"/logout", "/login"} {
		cookies := append([]*http.Cookie{}, bad...)
		for _, cookie := range f.cookies {
			if cookie.Name != cookieName {
				cookies = append(cookies, cookie)
			}
		}
		response := f.request(t, http.MethodPost, action, cookies)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("坏 Cookie 阻断%s: %d", action, response.StatusCode)
		}
		if action == "/login" {
			fresh := sessionCookie(t, response.Cookies())
			f.cookies = append(f.cookies, fresh)
			check := f.request(t, http.MethodGet, "/me", []*http.Cookie{fresh})
			_ = check.Body.Close()
			if check.StatusCode != http.StatusOK {
				t.Fatalf("清除坏 Cookie 后登录未成功: %d", check.StatusCode)
			}
		}
	}
}

func TestSessionRefreshOnlyRenewsExistingKeyAndKeepsIdentityImmutable(t *testing.T) {
	f := newSessionFixture(t)
	id := f.sessionID(t, sessionCookie(t, f.cookies))
	key := "yummy:session:" + id
	ctx := context.Background()
	payload, err := f.manager.redis.Get(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.redis.Expire(ctx, key, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	response := f.request(t, http.MethodGet, "/me", f.cookies)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	cookie := sessionCookie(t, response.Cookies())
	if f.sessionID(t, cookie) != id || cookie.MaxAge != int(idleTimeout.Seconds()) || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("滚动 Cookie 标识或安全属性改变: %#v", cookie)
	}
	after, err := f.manager.redis.Get(ctx, key).Result()
	if err != nil || after != payload {
		t.Fatalf("续期不应重写身份数据: changed=%v err=%v", after != payload, err)
	}
	ttl, err := f.manager.redis.TTL(ctx, key).Result()
	if err != nil || ttl < idleTimeout-time.Minute || ttl > idleTimeout {
		t.Fatalf("空闲 TTL 未滚动续期: %v %v", ttl, err)
	}
}

func TestSessionAbsoluteLifetimeCannotBeExtendedByRefresh(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Minute, -time.Minute} {
		t.Run(remaining.String(), func(t *testing.T) {
			f := newSessionFixture(t)
			u, _ := url.Parse(f.server.URL)
			r := &http.Request{URL: u, Header: make(http.Header)}
			for _, cookie := range f.cookies {
				r.AddCookie(cookie)
			}
			value, err := f.manager.store.Get(r, cookieName)
			if err != nil {
				t.Fatal(err)
			}
			value.Values["created_at"] = time.Now().Add(-absoluteTimeout + remaining).Unix()
			value.Values["last_seen"] = time.Now().Unix()
			if err := value.Save(r, httptest.NewRecorder()); err != nil {
				t.Fatal(err)
			}
			response := f.request(t, http.MethodGet, "/me", f.cookies)
			_ = response.Body.Close()
			if remaining < 0 {
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("超过绝对期限仍有身份: %d", response.StatusCode)
				}
				return
			}
			if response.StatusCode != http.StatusOK {
				t.Fatal(response.StatusCode)
			}
			cookie := sessionCookie(t, response.Cookies())
			if cookie.MaxAge > int(remaining.Seconds()) || cookie.MaxAge < int(remaining.Seconds())-5 {
				t.Fatalf("续期 Cookie 越过绝对期限: %d", cookie.MaxAge)
			}
			ttl, err := f.manager.redis.TTL(context.Background(), "yummy:session:"+value.ID).Result()
			if err != nil || ttl > remaining || ttl < remaining-5*time.Second {
				t.Fatalf("续期 Redis TTL 越过绝对期限: %v %v", ttl, err)
			}
		})
	}
}

func TestSessionRedisFailuresCloseAuthentication(t *testing.T) {
	for _, operation := range []string{"get", "expire"} {
		t.Run(operation, func(t *testing.T) {
			f := newSessionFixture(t)
			if operation == "get" {
				_ = f.manager.pool.Close()
			} else {
				_ = f.manager.redis.Close()
			}
			response := f.request(t, http.MethodGet, "/me", f.cookies)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("Redis %s 故障未失败关闭: %d", operation, response.StatusCode)
			}
			bad := f.request(t, http.MethodGet, "/me", []*http.Cookie{{Name: cookieName, Value: "invalid"}})
			_ = bad.Body.Close()
			if bad.StatusCode != http.StatusUnauthorized {
				t.Fatalf("坏签名不应访问 Redis 或误报故障: %d", bad.StatusCode)
			}
		})
	}
}

func TestBearerDatabaseFailureIsUnavailableNotInvalidCredentials(t *testing.T) {
	f := newSessionFixture(t)
	if err := f.manager.access.DB.Create(&model.Workspace{ID: "pat-space", Name: "凭证空间", Slug: "pat-space"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.manager.access.DB.Create(&model.WorkspaceMember{WorkspaceID: "pat-space", UserID: f.user.UserID, Role: model.WorkspaceAdmin}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.manager.access.DB.Create(&model.Application{ID: "pat-app", WorkspaceID: "pat-space", Name: "pat-app"}).Error; err != nil {
		t.Fatal(err)
	}
	token, err := f.manager.identity.CreateToken(context.Background(), f.user, identity.CreateTokenInput{Name: "test", Scopes: []string{"application:read"}, ApplicationIDs: []string{"pat-app"}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(token string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		// 即使浏览器 Cookie 有效，Bearer 分支也不能回退到 Cookie 身份。
		for _, cookie := range f.cookies {
			req.AddCookie(cookie)
		}
		response, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	if status, _ := call("ym_pat_invalid"); status != http.StatusUnauthorized {
		t.Fatalf("无效 PAT 未返回401: %d", status)
	}
	if status, _ := call(token.Plaintext); status != http.StatusOK {
		t.Fatalf("有效 PAT 未认证: %d", status)
	}
	db, err := f.manager.access.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	status, body := call(token.Plaintext)
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"unavailable"`) {
		t.Fatalf("数据库故障冒充凭证失效: %d %s", status, body)
	}
	if strings.Contains(string(body), "database") || strings.Contains(string(body), "closed") || strings.Contains(string(body), token.Plaintext) {
		t.Fatalf("身份错误泄露内部原因或凭证: %s", body)
	}
}

func TestHostAndCSRFSecurityBoundaries(t *testing.T) {
	f := newSessionFixture(t)
	for _, test := range []struct {
		name, host, origin, forwarded string
		csrf                          bool
		want                          int
	}{
		{name: "缺少CSRF", want: http.StatusForbidden},
		{name: "跨站来源", origin: "https://evil.example", csrf: true, want: http.StatusForbidden},
		{name: "伪造Host", host: "evil.example", csrf: true, want: http.StatusBadRequest},
		{name: "转发头不覆盖Host", host: "evil.example", forwarded: strings.TrimPrefix(f.server.URL, "http://"), csrf: true, want: http.StatusBadRequest},
		{name: "转发头不改变公开站点", forwarded: "evil.example", csrf: true, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/login", nil)
			for _, cookie := range f.cookies {
				req.AddCookie(cookie)
			}
			if test.csrf {
				req.Header.Set("X-CSRF-Token", f.csrf)
			}
			req.Host = test.host
			req.Header.Set("Origin", test.origin)
			req.Header.Set("X-Forwarded-Host", test.forwarded)
			response, err := f.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("边界检查返回%d，预期%d", response.StatusCode, test.want)
			}
			if response.StatusCode == http.StatusOK {
				f.cookies = append(f.cookies[:len(f.cookies)-1], sessionCookie(t, response.Cookies()))
			}
		})
	}
}

// newTLSProxyFixture 构造 https 公开地址 + 明文 httptest 后端的会话管理器，
// 复现 docs/operations.md §4 可信反向代理终止 TLS 的部署形态。
func newTLSProxyFixture(t *testing.T, behindTLSProxy bool) (*Manager, *httptest.Server, authorization.Principal, string, []*http.Cookie) {
	t.Helper()
	address := os.Getenv("TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
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
	user := model.User{ID: "tls-proxy-user", Username: "tls-proxy-user", PasswordHash: "unused"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{
		RedisAddress: address, PublicBaseURL: "https://122.51.59.201.nip.io", BehindTLSProxy: behindTLSProxy,
		CookieKey: base64.StdEncoding.EncodeToString([]byte("12345678901234567890123456789012")),
		CSRFKey:   base64.StdEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwxzy123456")),
	}, service, access)
	if err != nil {
		t.Fatal(err)
	}
	principal := authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion}
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if err := manager.Establish(r.Context(), principal); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if _, err := authorization.FromContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); _ = manager.Close() })
	// 请求 Host 必须与 https 公开地址一致（模拟反代保留 Host 转发）。
	client := server.Client()
	do := func(method, path string, cookies []*http.Cookie, csrfToken string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "122.51.59.201.nip.io"
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		if csrfToken != "" {
			req.Header.Set("X-CSRF-Token", csrfToken)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	bootstrap := do(http.MethodGet, "/auth/csrf", nil, "")
	var body struct {
		Token string `json:"csrfToken"`
	}
	if err := json.NewDecoder(bootstrap.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return manager, server, principal, body.Token, bootstrap.Cookies()
}

// 反向代理终止 TLS 是 §4 文档化生产形态：CSRF 的"必须TLS"检查会必然误报，
// BehindTLSProxy 只豁免该检查；关闭该标志时保持失败关闭，Cookie Secure 属性不受影响。
func TestBehindTLSProxyRestoresCookieCSRFBehindTerminatingProxy(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		want    int
	}{
		{name: "未声明反代时保持失败关闭", enabled: false, want: http.StatusForbidden},
		{name: "声明反代后Cookie会话写请求可用", enabled: true, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, server, _, csrfToken, cookies := newTLSProxyFixture(t, tc.enabled)
			// /auth/csrf 引导响应设置的是 yummy_csrf；https 公开地址下必须保持 Secure。
			secureSeen := false
			for _, cookie := range cookies {
				if cookie.Name == "yummy_csrf" && cookie.Secure {
					secureSeen = true
				}
			}
			if !secureSeen {
				t.Fatalf("https 公开地址下 CSRF Cookie 必须保持 Secure 属性: %v", cookies)
			}
			login, err := http.NewRequest(http.MethodPost, server.URL+"/login", nil)
			if err != nil {
				t.Fatal(err)
			}
			login.Host = "122.51.59.201.nip.io"
			for _, cookie := range cookies {
				login.AddCookie(cookie)
			}
			login.Header.Set("X-CSRF-Token", csrfToken)
			response, err := http.DefaultTransport.RoundTrip(login)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != tc.want {
				t.Fatalf("POST /login 状态 = %d, 期望 %d", response.StatusCode, tc.want)
			}
		})
	}
}

// 浏览器对 POST 携带 Origin: <https 主机>；反向代理明文转发到本进程后
// gorilla/csrf 的 sameOrigin 会因 scheme 不一致误判跨站。BehindTLSProxy（与
// behind-tls-proxy 安装形态一致）下必须放行"自己的 https 来源"，跨站仍拒绝。
func TestBrowserOriginOverBehindTLSProxyPassesCSRF(t *testing.T) {
	_, server, _, csrfToken, cookies := newTLSProxyFixture(t, true)
	originHeader := "https://122.51.59.201.nip.io"
	send := func(withOrigin bool) int {
		login, err := http.NewRequest(http.MethodPost, server.URL+"/login", nil)
		if err != nil {
			t.Fatal(err)
		}
		login.Host = "122.51.59.201.nip.io"
		for _, cookie := range cookies {
			login.AddCookie(cookie)
		}
		login.Header.Set("X-CSRF-Token", csrfToken)
		if withOrigin {
			login.Header.Set("Origin", originHeader)
		}
		response, err := http.DefaultTransport.RoundTrip(login)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode
	}
	if code := send(false); code != http.StatusOK {
		t.Fatalf("无 Origin 头的 POST 应通过: %d", code)
	}
	if code := send(true); code != http.StatusOK {
		t.Fatalf("浏览器 Origin（与公开地址一致）的 POST 应通过: %d", code)
	}
}
