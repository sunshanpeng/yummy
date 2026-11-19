package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
)

type fakeIdP struct {
	server                 *httptest.Server
	key                    *rsa.PrivateKey
	mu                     sync.Mutex
	nonce, challenge, mode string
	exchanges              int
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/authorize", "token_endpoint": idp.server.URL + "/token", "jwks_uri": idp.server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &idp.key.PublicKey, KeyID: "kid", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			http.Error(w, "bad", 400)
			return
		}
		idp.mu.Lock()
		nonce, challenge, mode := idp.nonce, idp.challenge, idp.mode
		idp.exchanges++
		idp.mu.Unlock()
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "provider-code" || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
			http.Error(w, "invalid grant", 400)
			return
		}
		signerKey := idp.key
		if mode == "signature" {
			var err error
			signerKey, err = rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				http.Error(w, "key error", 500)
				return
			}
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signerKey}, (&jose.SignerOptions{}).WithHeader("kid", "kid"))
		if err != nil {
			http.Error(w, "signer", 500)
			return
		}
		issuer, audience, subject := idp.server.URL, "oidc-client", "subject-a"
		expiry := time.Now().Add(time.Hour)
		switch mode {
		case "issuer":
			issuer = "https://wrong.example"
		case "audience":
			audience = "wrong-client"
		case "nonce":
			nonce = "wrong-nonce"
		case "expired":
			expiry = time.Now().Add(-time.Hour)
		case "unbound":
			subject = "unbound-subject"
		}
		token, err := jwt.Signed(signer).Claims(map[string]any{"iss": issuer, "aud": audience, "sub": subject, "exp": expiry.Unix(), "iat": time.Now().Unix(), "nonce": nonce, "email": "dev@example.com", "email_verified": true}).Serialize()
		if err != nil {
			http.Error(w, "jwt", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "external-idp-access-never-platform-token", "token_type": "Bearer", "id_token": token, "expires_in": 3600})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}
func (e *environment) oidcService(t *testing.T, idp *fakeIdP) *Service {
	t.Helper()
	service, err := NewService(e.db, e.access, e.identity, Config{PublicBaseURL: baseURL, HMACSecret: []byte(strings.Repeat("x", 32)), AllowInsecureLoopback: true, OIDCProviders: []OIDCProviderConfig{{Name: "team", Issuer: idp.server.URL, ClientID: "oidc-client", ClientSecret: "provider-secret", RedirectURI: baseURL + "/oidc/callback"}}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func startFake(t *testing.T, service *Service, idp *fakeIdP, mode string) (string, string) {
	t.Helper()
	binding := strings.Repeat("browser-binding-random-", 3)
	start, err := service.StartOIDC(context.Background(), "team", binding)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := target.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("nonce") == "" || query.Get("state") == "" {
		t.Fatalf("OIDC缺少安全参数:%s", target)
	}
	idp.mu.Lock()
	idp.nonce = query.Get("nonce")
	idp.challenge = query.Get("code_challenge")
	idp.mode = mode
	idp.mu.Unlock()
	return query.Get("state"), binding
}
func TestOIDCIssuerSubjectBindingAndReplay(t *testing.T) {
	e := setup(t)
	idp := newFakeIdP(t)
	service := e.oidcService(t, idp)
	if err := service.BindExternalIdentity(context.Background(), e.p, "team", "subject-a", "dev"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通人可绑定身份:%v", err)
	}
	if err := service.BindExternalIdentity(context.Background(), e.root, "team", "subject-a", "dev"); err != nil {
		t.Fatal(err)
	}
	if err := service.BindExternalIdentity(context.Background(), e.root, "team", "subject-a", "viewer"); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("绑定可转移:%v", err)
	}
	state, binding := startFake(t, service, idp, "")
	p, err := service.CompleteOIDC(context.Background(), "team", state, "provider-code", binding)
	if err != nil || p.UserID != "dev" || p.IsBearer() {
		t.Fatalf("OIDC返回身份:%+v %v", p, err)
	}
	if _, err := service.CompleteOIDC(context.Background(), "team", state, "provider-code", binding); !errors.Is(err, ErrOIDC) {
		t.Fatalf("OIDC state可重放:%v", err)
	}
	var stored oidcLoginRecord
	if err := e.db.First(&stored, "state_hash = ?", digestState(state)).Error; err != nil {
		t.Fatal(err)
	}
	if stored.PKCEVerifier != "" || stored.ConsumedAt == nil {
		t.Fatal("消费未清PKCE")
	}
}
func digestState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}
func TestOIDCNegativeSignatureClaimsAndBrowserBinding(t *testing.T) {
	e := setup(t)
	idp := newFakeIdP(t)
	service := e.oidcService(t, idp)
	if err := service.BindExternalIdentity(context.Background(), e.root, "team", "subject-a", "dev"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"issuer", "audience", "nonce", "signature", "expired", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			state, binding := startFake(t, service, idp, mode)
			if _, err := service.CompleteOIDC(context.Background(), "team", state, "provider-code", binding); !errors.Is(err, ErrOIDC) {
				t.Fatalf("无效OIDC通过:%v", err)
			}
		})
	}
	state, binding := startFake(t, service, idp, "")
	if _, err := service.CompleteOIDC(context.Background(), "team", state, "provider-code", strings.Repeat("other-browser", 4)); !errors.Is(err, ErrOIDC) {
		t.Fatalf("未绑定浏览器通过:%v", err)
	}
	if _, err := service.CompleteOIDC(context.Background(), "team", state, "provider-code", binding); err != nil {
		t.Fatalf("错binding污染合法state:%v", err)
	}
	// 同email的未绑定subject绝不能自动合并到现有dev，用户数量保持不变。
	var count int64
	if err := e.db.Table("users").Count(&count).Error; err != nil || count != 3 {
		t.Fatalf("外部自动建用户:%d %v", count, err)
	}
}
func TestOIDCConcurrentSingleConsumption(t *testing.T) {
	e := setup(t)
	idp := newFakeIdP(t)
	service := e.oidcService(t, idp)
	if err := service.BindExternalIdentity(context.Background(), e.root, "team", "subject-a", "dev"); err != nil {
		t.Fatal(err)
	}
	state, binding := startFake(t, service, idp, "")
	start := make(chan struct{})
	out := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := service.CompleteOIDC(context.Background(), "team", state, "provider-code", binding)
			out <- err
		}()
	}
	close(start)
	wait.Wait()
	close(out)
	good := 0
	for err := range out {
		if err == nil {
			good++
		} else if !errors.Is(err, ErrOIDC) {
			t.Fatal(err)
		}
	}
	idp.mu.Lock()
	calls := idp.exchanges
	idp.mu.Unlock()
	if good != 1 || calls != 1 {
		t.Fatalf("state并发成功=%d provider exchange=%d", good, calls)
	}
}
