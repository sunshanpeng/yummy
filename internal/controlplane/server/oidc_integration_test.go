package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
)

func TestExternalOIDCLoginEstablishesSession(t *testing.T) {
	redis := os.Getenv("TEST_REDIS_ADDR")
	if redis == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
	}
	db := testkit.OpenDatabase(t)
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := jose.JSONWebKey{Key: private, KeyID: "test-key", Algorithm: string(jose.ES256), Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", key.KeyID))
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewUnstartedServer(nil)
	issuer := "http://" + provider.Listener.Addr().String()
	var authQuery atomic.Pointer[url.Values]
	provider.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
				"jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"ES256"},
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{key.Public()}})
		case "/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("code") != "test-code" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			query := authQuery.Load()
			hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if query == nil || query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id, signErr := jwt.Signed(signer).Claims(map[string]any{"iss": issuer, "aud": "team-client", "sub": "member-123", "nonce": query.Get("nonce"), "exp": time.Now().Add(time.Hour).Unix()}).Serialize()
			if signErr != nil {
				t.Errorf("签名 IdP 回包：%v", signErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id_token": id, "access_token": "test-idp-access", "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	})
	provider.Start()
	t.Cleanup(provider.Close)

	web := httptest.NewUnstartedServer(nil)
	base := "http://" + web.Listener.Addr().String()
	keyString := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	ctx := context.Background()
	product, err := openWithDatabase(ctx, db, Config{
		RedisAddress: redis, PublicBaseURL: base, SessionKey: keyString(91), CSRFKey: keyString(92), MasterKey: keyString(93), OAuthKey: keyString(94),
		OIDCIssuer: issuer, OIDCClientID: "team-client", OIDCClientSecret: "test-idp-secret",
		SourceRoot: t.TempDir(), AllowLocalHTTP: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = product.Close() })
	admin, err := product.Identity.InitializeAdmin(ctx, "oidcadmin", "oidc-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := product.OAuth.BindExternalIdentity(ctx, authorization.Principal{UserID: admin.ID, AuthVersion: admin.AuthVersion}, "team", "member-123", admin.ID); err != nil {
		t.Fatal(err)
	}
	// 使用本次测试独有的对端地址，避免共享 Redis 中其他测试的计数干扰。
	remoteAddr := "oidc-test-" + rand.Text() + ":12345"
	web.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = remoteAddr
		product.Handler.ServeHTTP(w, r)
	})
	web.Start()
	t.Cleanup(web.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(base + "/auth/csrf")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap map[string]string
	if err := json.NewDecoder(response.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	start := func(csrf string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, base+"/auth/oidc/start", strings.NewReader(`{"returnRequest":"req_aaaaaaaaaaaaaaaaaaaaaa"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	denied := start("")
	_ = denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("OIDC 开始端点接受无 CSRF 请求：%d", denied.StatusCode)
	}
	response = start(bootstrap["csrfToken"])
	var started map[string]string
	decodeErr := json.NewDecoder(response.Body).Decode(&started)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil {
		t.Fatalf("未启动登录流程：%d %v", response.StatusCode, decodeErr)
	}
	target, err := url.Parse(started["url"])
	if err != nil {
		t.Fatal(err)
	}
	query := target.Query()
	authQuery.Store(&query)
	callback := base + "/auth/oidc/callback?" + (url.Values{"state": {query.Get("state")}, "code": {"test-code"}}).Encode()
	unbound, err := (&http.Client{CheckRedirect: client.CheckRedirect}).Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	_ = unbound.Body.Close()
	if unbound.StatusCode != http.StatusSeeOther || !strings.HasPrefix(unbound.Header.Get("Location"), "/login?") {
		t.Fatal("缺少浏览器绑定的回调未按失败处理")
	}
	response, err = client.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/authorize?request_id=req_aaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("回调未继续授权：%d %q", response.StatusCode, response.Header.Get("Location"))
	}
	api := platformv1connect.NewIdentityServiceClient(client, base+"/api", connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("X-CSRF-Token", bootstrap["csrfToken"])
			return next(ctx, req)
		}
	})))
	got, err := api.GetSession(ctx, connect.NewRequest(&platformv1.GetSessionRequest{}))
	if err != nil || got.Msg.GetUser().GetId() != admin.ID {
		t.Fatalf("外部登录未建立本地 Cookie 会话：%v", err)
	}
	for attempt := 2; attempt <= 21; attempt++ {
		response = start(bootstrap["csrfToken"])
		_ = response.Body.Close()
		want := http.StatusOK
		if attempt == 21 {
			want = http.StatusTooManyRequests
		}
		if response.StatusCode != want {
			t.Fatalf("第 %d 次团队登录响应为 %d，预期 %d", attempt, response.StatusCode, want)
		}
	}
	if _, err := api.Login(ctx, connect.NewRequest(&platformv1.LoginRequest{Username: "oidcadmin", Password: "oidc-admin-password"})); err != nil {
		t.Fatalf("团队登录达到限流后，本地密码登录不可用：%v", err)
	}
}
