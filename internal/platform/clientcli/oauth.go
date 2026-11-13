package clientcli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
)

const cliClientID = "yummy-cli"

type oauthMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

func discoverOAuth(ctx context.Context, base string) (oauthMetadata, error) {
	var m oauthMetadata
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return m, err
	}
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, errors.New("授权服务器发现失败")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, errors.New("授权服务器元数据无效")
	}
	if m.Issuer != base || m.AuthorizationEndpoint != base+"/oauth/authorize" || m.TokenEndpoint != base+"/oauth/token" {
		return m, errors.New("授权服务器 issuer 或端点不匹配实例")
	}
	found := false
	for _, v := range m.CodeChallengeMethods {
		if v == "S256" {
			found = true
		}
	}
	if !found {
		return m, errors.New("授权服务器不支持 PKCE S256")
	}
	return m, nil
}
func oauthConfig(base, redirect string, scopes []string) *oauth2.Config {
	return &oauth2.Config{ClientID: cliClientID, Endpoint: oauth2.Endpoint{AuthURL: base + "/oauth/authorize", TokenURL: base + "/oauth/token", AuthStyle: oauth2.AuthStyleInParams}, RedirectURL: redirect, Scopes: scopes}
}
func randomValue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func validOAuthToken(token *oauth2.Token) bool {
	return token != nil && token.AccessToken != "" && !strings.ContainsAny(token.AccessToken, " \t\r\n") && strings.EqualFold(token.TokenType, "Bearer") && !token.Expiry.IsZero()
}
func oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, newHTTPClient())
}

func (a *clientApp) oauthLoginCommand() *cobra.Command {
	var noBrowser, offline bool
	var scopes []string
	cmd := &cobra.Command{Use: "login", Short: "浏览器授权码 + PKCE 登录", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name, p, err := a.config()
		if err != nil {
			return err
		}
		if p.Server == "" {
			return configError(errors.New("请先指定实例 --server"))
		}
		if _, err = discoverOAuth(cmd.Context(), p.Server); err != nil {
			return fail("OAUTH_DISCOVERY", 3, false, "授权服务器发现失败或 issuer 不匹配")
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return fail("OAUTH_CALLBACK", 1, false, "无法打开本机回调端口")
		}
		defer listener.Close()
		redirect := "http://" + listener.Addr().String() + "/callback"
		state, err := randomValue()
		if err != nil {
			return err
		}
		verifier := oauth2.GenerateVerifier()
		if offline {
			scopes = append(scopes, "offline_access")
		}
		cfg := oauthConfig(p.Server, redirect, scopes)
		authorizeURL := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", p.Server+"/api"))
		type result struct {
			code string
			err  error
		}
		resultCh := make(chan result, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			if r.Method != "GET" {
				http.Error(w, "method not allowed", 405)
				return
			}
			q := r.URL.Query()
			if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || len(q["iss"]) != 1 || q.Get("iss") != p.Server {
				http.Error(w, "invalid authorization callback", 400)
				return
			}
			if len(q["error"]) > 0 && (len(q["error"]) != 1 || len(q["code"]) != 0) {
				http.Error(w, "invalid authorization callback", 400)
				return
			}
			if len(q["error"]) == 1 {
				select {
				case resultCh <- result{err: errors.New("授权被拒绝")}:
				default:
				}
				_, _ = io.WriteString(w, "授权未完成，请返回终端。\n")
				return
			}
			if len(q["code"]) != 1 || q.Get("code") == "" || len(q.Get("code")) > 4096 {
				http.Error(w, "invalid authorization callback", 400)
				return
			}
			select {
			case resultCh <- result{code: q.Get("code")}:
			default:
			}
			_, _ = io.WriteString(w, "授权完成，请返回终端。\n")
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
		defer srv.Close()
		go func() { _ = srv.Serve(listener) }()
		// 授权 URL 不含 access/refresh token；机器可读成功结果仍只写 stdout。
		_, _ = io.WriteString(cmd.ErrOrStderr(), "请在浏览器打开授权地址："+authorizeURL+"\n")
		if !noBrowser {
			_ = openBrowser(authorizeURL)
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		var r result
		select {
		case r = <-resultCh:
		case <-ctx.Done():
			return rpcError(ctx.Err())
		}
		if r.err != nil {
			return fail("OAUTH_DENIED", 3, false, "用户拒绝授权")
		}
		token, err := cfg.Exchange(oauthContext(ctx), r.code, oauth2.VerifierOption(verifier), oauth2.SetAuthURLParam("resource", p.Server+"/api"))
		if err != nil || !validOAuthToken(token) {
			return fail("OAUTH_EXCHANGE", 3, false, "授权码换取凭据失败")
		}
		if offline && token.RefreshToken == "" {
			return fail("OAUTH_EXCHANGE", 3, false, "未获得用户同意的离线令牌")
		}
		err = a.store.update(name, func(stored *profile) error {
			if stored.Server != "" && stored.Server != p.Server {
				return errors.New("profile 实例已变化，拒绝保存授权")
			}
			if stored.Server == "" {
				stored.Server = p.Server
			}
			stored.Token = token.AccessToken
			stored.RefreshToken = token.RefreshToken
			stored.TokenExpiry = token.Expiry
			return nil
		})
		if err != nil {
			return configError(err)
		}
		return output(cmd, map[string]any{"profile": name, "server": p.Server, "credential": "oauth", "expires_at": token.Expiry.UTC(), "offline": token.RefreshToken != ""})
	}}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "只输出授权地址，不启动浏览器")
	cmd.Flags().BoolVar(&offline, "offline", false, "请求离线访问，需用户明确同意")
	cmd.Flags().StringSliceVar(&scopes, "scope", []string{"application:read", "build:read", "logs:read", "runtime:read"}, "请求的权限，可覆盖默认只读权限")
	return cmd
}
func openBrowser(target string) error {
	var command string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return errors.New("不支持自动打开浏览器")
	}
	return exec.Command(command, target).Start()
}

// refreshToken 持有跨进程锁直到 refresh rotation 和本地原子替换均完成，避免并发重用刷新令牌。
func (a *clientApp) refreshToken(parent context.Context, name, server string) (profile, error) {
	ctx, cancel := context.WithTimeout(parent, 35*time.Second)
	defer cancel()
	p, err := a.store.change(name, func(stored *profile) error {
		if stored.Server != server {
			return errors.New("profile 实例已变化")
		}
		if stored.TokenExpiry.IsZero() || time.Now().Add(30*time.Second).Before(stored.TokenExpiry) {
			return nil
		}
		if stored.RefreshToken == "" {
			return errors.New("令牌已过期，请重新登录")
		}
		cfg := oauthConfig(server, "", nil)
		ctxWithHTTP := oauthContext(ctx)
		// resource 是 RFC 8707 必需参数；refresh 使用同一绑定资源。
		cfg.Endpoint.TokenURL = server + "/oauth/token"
		ctxWithHTTP = context.WithValue(ctxWithHTTP, oauth2.HTTPClient, &http.Client{Timeout: 35 * time.Second, Transport: &resourceTransport{base: http.DefaultTransport, resource: server + "/api"}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
		token, err := cfg.TokenSource(ctxWithHTTP, &oauth2.Token{RefreshToken: stored.RefreshToken, Expiry: time.Now().Add(-time.Hour)}).Token()
		if err != nil || !validOAuthToken(token) || token.RefreshToken == "" || token.RefreshToken == stored.RefreshToken {
			return errors.New("凭据刷新失败，请重新登录")
		}
		stored.Token = token.AccessToken
		stored.RefreshToken = token.RefreshToken
		stored.TokenExpiry = token.Expiry
		return nil
	})
	if err != nil {
		return profile{}, fail("OAUTH_REFRESH", 3, false, "凭据刷新失败，请重新登录")
	}
	return p, nil
}

// resourceTransport 将资源参数加入 oauth2 库产生的刷新表单；不修改业务 API 请求。
type resourceTransport struct {
	base     http.RoundTripper
	resource string
}

func (t *resourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != "POST" || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return nil, errors.New("无效的 OAuth token 请求")
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	values, err := url.ParseQuery(string(data))
	if err != nil {
		return nil, err
	}
	values.Set("resource", t.resource)
	encoded := values.Encode()
	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(strings.NewReader(encoded))
	clone.ContentLength = int64(len(encoded))
	return t.base.RoundTrip(clone)
}
