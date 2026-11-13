// Package session 适配成熟 Redis 会话存储，Cookie 仅包含签名的不透明随机标识。
package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	ginsessions "github.com/gin-contrib/sessions"
	ginredis "github.com/gin-contrib/sessions/redis"
	redigo "github.com/gomodule/redigo/redis"
	"github.com/gorilla/csrf"
	"github.com/gorilla/securecookie"
	gorillasessions "github.com/gorilla/sessions"
	goredis "github.com/redis/go-redis/v9"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/identity"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
)

const cookieName = "yummy_session"
const sessionKeyPrefix = "yummy:session:"
const idleTimeout = 12 * time.Hour
const absoluteTimeout = 7 * 24 * time.Hour

var ErrUnavailable = errors.New("会话服务暂不可用")
var ErrRateLimited = errors.New("登录尝试过于频繁，请稍后重试")

// BearerAuthenticator 只验证与本实例 API 受众精确匹配的 OAuth 凭证。
// PAT 使用独立的固定前缀识别，不因 OAuth 不可用而回退或借用 Cookie。
type BearerAuthenticator func(context.Context, string, string) (authorization.Principal, error)

type Config struct {
	RedisAddress   string
	PublicBaseURL  string
	CookieKey      string
	CSRFKey        string
	AllowLocalHTTP bool
	// BehindTLSProxy 声明公网入口由可信反向代理终止 HTTPS（docs/operations.md §4 部署形态）。
	// 仅豁免 gorilla/csrf 的"请求必须为TLS"检查（该检查在此形态下必然误报）；
	// 会话 Cookie 的 Secure 属性、Host 校验与 Origin 校验全部保持不变。
	BehindTLSProxy bool
	OAuth          BearerAuthenticator
}

type Manager struct {
	store          ginsessions.Store
	pool           *redigo.Pool
	redis          *goredis.Client
	identity       *identity.Service
	access         *authorization.Access
	oauth          BearerAuthenticator
	secure         bool
	behindTLSProxy bool
	baseURL        *url.URL
	csrfKey        []byte
	codecs         []securecookie.Codec
}

type exchange struct {
	request *http.Request
	writer  http.ResponseWriter
}
type exchangeKey struct{}

func New(config Config, service *identity.Service, access *authorization.Access) (*Manager, error) {
	base, err := url.Parse(config.PublicBaseURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("PUBLIC_BASE_URL 必须为明确的 HTTP(S) 站点根地址")
	}
	secure := base.Scheme == "https"
	if !secure {
		ip := net.ParseIP(base.Hostname())
		if base.Scheme != "http" || !config.AllowLocalHTTP || (base.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, errors.New("生产公开地址必须使用 HTTPS；仅显式开发模式允许本机 HTTP")
		}
	}
	cookieKey, err := base64.StdEncoding.DecodeString(config.CookieKey)
	if err != nil || len(cookieKey) != 32 {
		return nil, errors.New("会话密钥必须为 Base64 编码的32字节随机值")
	}
	csrfKey, err := base64.StdEncoding.DecodeString(config.CSRFKey)
	if err != nil || len(csrfKey) != 32 {
		return nil, errors.New("CSRF密钥必须为 Base64 编码的32字节随机值")
	}
	if string(cookieKey) == string(csrfKey) {
		return nil, errors.New("会话与CSRF密钥必须独立")
	}
	redisURL := config.RedisAddress
	if !strings.Contains(redisURL, "://") {
		redisURL = "redis://" + redisURL
	}
	options, err := goredis.ParseURL(redisURL)
	if err != nil {
		return nil, errors.New("Redis配置无效")
	}
	pool := &redigo.Pool{MaxIdle: 5, MaxActive: 20, Wait: true, IdleTimeout: time.Minute,
		Dial: func() (redigo.Conn, error) {
			return redigo.DialURL(redisURL, redigo.DialConnectTimeout(3*time.Second), redigo.DialReadTimeout(3*time.Second), redigo.DialWriteTimeout(3*time.Second))
		},
	}
	store, err := ginredis.NewStoreWithPool(pool, cookieKey)
	if err != nil {
		_ = pool.Close()
		return nil, ErrUnavailable
	}
	if err := ginredis.SetKeyPrefix(store, sessionKeyPrefix); err != nil {
		_ = pool.Close()
		return nil, err
	}
	redisStore, err := ginredis.GetRedisStore(store)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	store.Options(ginsessions.Options{Path: "/", MaxAge: int(idleTimeout.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	return &Manager{store: store, pool: pool, redis: goredis.NewClient(options), identity: service, access: access, oauth: config.OAuth, secure: secure, behindTLSProxy: config.BehindTLSProxy, baseURL: base, csrfKey: csrfKey, codecs: redisStore.Codecs}, nil
}

func (m *Manager) Close() error { return errors.Join(m.pool.Close(), m.redis.Close()) }
func CSRFToken(ctx context.Context) string {
	x, ok := ctx.Value(exchangeKey{}).(exchange)
	if !ok {
		return ""
	}
	return csrf.Token(x.request)
}

func (m *Manager) APIAudience() string { return strings.TrimSuffix(m.baseURL.String(), "/") + "/api" }

// Middleware 不信任 Host 或转发头；有 Authorization 的请求绝不借用 Cookie 身份。
func (m *Manager) Middleware(next http.Handler) http.Handler {
	protected := csrf.Protect(m.csrfKey, csrf.Secure(m.secure), csrf.HttpOnly(true), csrf.Path("/"), csrf.CookieName("yummy_csrf"),
		csrf.ErrorHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"permission_denied","message":"安全校验失败，请刷新页面重试"}`))
		})),
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), exchangeKey{}, exchange{r, w})
		if r.URL.Path == "/auth/csrf" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]string{"csrfToken": csrf.Token(r)})
			return
		}
		if r.Header.Get("Authorization") != "" {
			value := r.Header.Get("Authorization")
			if !strings.HasPrefix(value, "Bearer ") || strings.ContainsAny(value[7:], " \t\r\n") {
				writeAuthError(w, http.StatusUnauthorized, "凭证无效")
				return
			}
			token := strings.TrimPrefix(value, "Bearer ")
			var p authorization.Principal
			var err error
			if strings.HasPrefix(token, "ym_pat_") {
				p, err = m.identity.AuthenticatePAT(ctx, token, m.APIAudience())
			} else if m.oauth != nil {
				p, err = m.oauth(ctx, token, m.APIAudience())
			} else {
				err = authorization.ErrUnauthenticated
			}
			if err != nil {
				if errors.Is(err, authorization.ErrUnauthenticated) {
					writeAuthError(w, http.StatusUnauthorized, "凭证无效或已失效")
				} else {
					writeAuthError(w, http.StatusServiceUnavailable, "身份服务暂不可用")
				}
				return
			}
			ctx = authorization.WithPrincipal(ctx, p)
		} else {
			p, err := m.load(r, w)
			if err != nil && !errors.Is(err, authorization.ErrUnauthenticated) {
				writeAuthError(w, http.StatusServiceUnavailable, "会话服务暂不可用")
				return
			}
			if err == nil {
				ctx = authorization.WithPrincipal(ctx, p)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != m.baseURL.Host {
			writeAuthError(w, http.StatusBadRequest, "请求站点与公开地址不一致")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" && origin != m.baseURL.Scheme+"://"+m.baseURL.Host {
				writeAuthError(w, http.StatusForbidden, "请求来源不受信任")
				return
			}
		}
		if r.Header.Get("Authorization") != "" {
			r = csrf.UnsafeSkipCheck(r)
		}
		if !m.secure || m.behindTLSProxy {
			r = csrf.PlaintextHTTPRequest(r)
		}
		protected.ServeHTTP(w, r)
	})
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	code := "unauthenticated"
	if status == http.StatusForbidden {
		code = "permission_denied"
	}
	if status == http.StatusServiceUnavailable {
		code = "unavailable"
	}
	if status == http.StatusBadRequest {
		code = "invalid_argument"
	}
	_, _ = fmt.Fprintf(w, `{"code":%q,"message":%q}`, code, message)
}

func (m *Manager) load(r *http.Request, w http.ResponseWriter) (authorization.Principal, error) {
	if _, err := r.Cookie(cookieName); errors.Is(err, http.ErrNoCookie) {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	value, err := m.store.Get(r, cookieName)
	if err != nil {
		if invalidCookie(err) {
			m.clearCookie(w)
			return authorization.Principal{}, authorization.ErrUnauthenticated
		}
		return authorization.Principal{}, ErrUnavailable
	}
	if value.IsNew {
		m.clearCookie(w)
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	userID, _ := value.Values["user_id"].(string)
	version, _ := value.Values["auth_version"].(int64)
	created, _ := value.Values["created_at"].(int64)
	if userID == "" || created == 0 || !time.Now().Before(time.Unix(created, 0).Add(absoluteTimeout)) {
		m.clearCookie(w)
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	var current authorization.Principal
	err = m.access.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		current, err = m.access.Refresh(tx, authorization.Principal{UserID: userID, AuthVersion: version})
		return err
	})
	if err != nil {
		if errors.Is(err, authorization.ErrUnauthenticated) {
			m.clearCookie(w)
		}
		return authorization.Principal{}, err
	}
	if err := m.renew(r, w, value.ID, created); err != nil {
		return authorization.Principal{}, err
	}
	return current, nil
}

func invalidCookie(err error) bool {
	var cookieError securecookie.Error
	return errors.As(err, &cookieError) && cookieError.IsDecode()
}

func (m *Manager) cookieOptions(maxAge int) *gorillasessions.Options {
	return &gorillasessions.Options{Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode}
}

func (m *Manager) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, gorillasessions.NewCookie(cookieName, "", m.cookieOptions(-1)))
}

// renew 只延长仍存在的 Redis 记录，绝不重新写入身份。
// EXPIRE 对缺失键原子返回 false；在途请求不能在退出或轮换的 DEL 之后复活旧会话。
func (m *Manager) renew(r *http.Request, w http.ResponseWriter, id string, created int64) error {
	remaining := time.Until(time.Unix(created, 0).Add(absoluteTimeout))
	if id == "" || remaining < time.Second {
		m.clearCookie(w)
		return authorization.ErrUnauthenticated
	}
	ttl := min(idleTimeout, remaining.Truncate(time.Second))
	encoded, err := securecookie.EncodeMulti(cookieName, id, m.codecs...)
	if err != nil {
		return ErrUnavailable
	}
	alive, err := m.redis.Expire(r.Context(), sessionKeyPrefix+id, ttl).Result()
	if err != nil {
		return ErrUnavailable
	}
	if !alive {
		m.clearCookie(w)
		return authorization.ErrUnauthenticated
	}
	// 仅刷新同一个不透明 ID 的签名与 Cookie 期限，不调用底层 Save/SETEX。
	http.SetCookie(w, gorillasessions.NewCookie(cookieName, encoded, m.cookieOptions(int(ttl.Seconds()))))
	return nil
}

func (m *Manager) Establish(ctx context.Context, p authorization.Principal) error {
	x, ok := ctx.Value(exchangeKey{}).(exchange)
	if !ok {
		return errors.New("缺少会话请求上下文")
	}
	if x.request.Header.Get("Authorization") != "" {
		return authorization.ErrDenied
	}
	if _, err := x.request.Cookie(cookieName); err == nil {
		previous, err := m.store.Get(x.request, cookieName)
		if err != nil {
			if !invalidCookie(err) {
				return ErrUnavailable
			}
			m.clearCookie(x.writer)
		} else if previous.ID != "" {
			previous.Options.MaxAge = -1
			if err := previous.Save(x.request, x.writer); err != nil {
				return ErrUnavailable
			}
		}
	}
	fresh := gorillasessions.NewSession(m.store, cookieName)
	fresh.Options = m.cookieOptions(int(idleTimeout.Seconds()))
	now := time.Now().UTC().Unix()
	// 身份与创建时间在建立后保持不变；空闲期限仅由 Redis TTL 表达。
	fresh.Values = map[any]any{"user_id": p.UserID, "auth_version": p.AuthVersion, "created_at": now}
	if err := fresh.Save(x.request, x.writer); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (m *Manager) Logout(ctx context.Context) error {
	x, ok := ctx.Value(exchangeKey{}).(exchange)
	if !ok {
		return errors.New("缺少会话请求上下文")
	}
	if x.request.Header.Get("Authorization") != "" {
		return authorization.ErrDenied
	}
	value, err := m.store.Get(x.request, cookieName)
	if err != nil {
		if invalidCookie(err) {
			m.clearCookie(x.writer)
			return nil
		}
		return ErrUnavailable
	}
	if value.ID == "" {
		m.clearCookie(x.writer)
		return nil
	}
	value.Options.MaxAge = -1
	value.Values = make(map[any]any)
	if err := value.Save(x.request, x.writer); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (m *Manager) AllowLogin(ctx context.Context, username string) error {
	x, ok := ctx.Value(exchangeKey{}).(exchange)
	if !ok {
		return errors.New("缺少会话请求上下文")
	}
	host, _, err := net.SplitHostPort(x.request.RemoteAddr)
	if err != nil {
		host = x.request.RemoteAddr
	}
	// 不相信客户端提供的 X-Forwarded-For；代理部署需明确可信代理配置后再扩展。
	for _, key := range []string{"ip:" + host, "account:" + strings.ToLower(strings.TrimSpace(username))} {
		count, err := m.redis.Eval(ctx, `local n=redis.call('INCR',KEYS[1]);if n==1 then redis.call('EXPIRE',KEYS[1],900) end;return n`, []string{"yummy:login:" + security.Digest(key)}).Int()
		if err != nil {
			return ErrUnavailable
		}
		if count > 20 {
			return ErrRateLimited
		}
	}
	return nil
}
