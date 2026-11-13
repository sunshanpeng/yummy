package oauth

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/identity"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const CLIClientID = "yummy-cli"
const OfflineAccess = "offline_access"

var ErrClient = errors.New("OAuth 客户端元数据无效")

type Config struct {
	PublicBaseURL         string
	HMACSecret            []byte
	AllowInsecureLoopback bool
	OIDCProviders         []OIDCProviderConfig
}
type Service struct {
	db            *gorm.DB
	access        *authorization.Access
	identity      *identity.Service
	store         *storage
	provider      fosite.OAuth2Provider
	strategy      oauth2.CoreStrategy
	base          string
	allowLoopback bool
	oidcProviders map[string]*externalProvider
}

func NewService(db *gorm.DB, access *authorization.Access, identityService *identity.Service, config Config) (*Service, error) {
	parsed, err := url.Parse(config.PublicBaseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || len(config.HMACSecret) < 32 || db == nil || access == nil || identityService == nil {
		return nil, authorization.ErrInvalid
	}
	if parsed.Scheme != "https" && !(config.AllowInsecureLoopback && parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, authorization.ErrInvalid
	}
	base := strings.TrimRight(config.PublicBaseURL, "/")
	store := &storage{db: db}
	configuration := &fosite.Config{GlobalSecret: bytes.Clone(config.HMACSecret), AccessTokenLifespan: 15 * time.Minute, AuthorizeCodeLifespan: 5 * time.Minute, RefreshTokenLifespan: 30 * 24 * time.Hour, EnforcePKCE: true, EnforcePKCEForPublicClients: true, RefreshTokenScopes: []string{OfflineAccess}, MinParameterEntropy: 16, ScopeStrategy: fosite.ExactScopeStrategy, AudienceMatchingStrategy: fosite.ExactAudienceMatchingStrategy, SanitationWhiteList: []string{"code_challenge", "code_challenge_method", "redirect_uri", "resource"}, RedirectSecureChecker: func(_ context.Context, u *url.URL) bool {
		return u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname())
	}}
	strategy := compose.NewOAuth2HMACStrategy(configuration)
	provider := compose.Compose(configuration, store, strategy, compose.OAuth2AuthorizeExplicitFactory, compose.OAuth2RefreshTokenGrantFactory, compose.OAuth2TokenIntrospectionFactory, compose.OAuth2TokenRevocationFactory, compose.OAuth2PKCEFactory)
	service := &Service{db: db, access: access, identity: identityService, store: store, provider: provider, strategy: strategy, base: base, allowLoopback: config.AllowInsecureLoopback}
	// 第一方 CLI 是 public client，不嵌入 client secret；loopback 端口由 Fosite 按 RFC8252 匹配。
	client := clientRecord{ID: CLIClientID, Name: "Yummy CLI", Source: "first-party", RedirectURIs: []string{"http://127.0.0.1/callback", "http://[::1]/callback"}, Scopes: append(slices.Clone(authorization.TokenScopes), OfflineAccess), Resources: []string{base + "/api"}, Active: true}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&client).Error; err != nil {
		return nil, err
	}
	service.oidcProviders = make(map[string]*externalProvider)
	for _, providerConfig := range config.OIDCProviders {
		provider, err := service.configureOIDC(context.Background(), providerConfig)
		if err != nil {
			return nil, err
		}
		if _, exists := service.oidcProviders[providerConfig.Name]; exists {
			return nil, authorization.ErrInvalid
		}
		service.oidcProviders[providerConfig.Name] = provider
	}
	return service, nil
}
func isLoopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }
func (s *Service) resourceApplications(resource string) ([]string, error) {
	if resource == s.base+"/api" {
		return nil, nil
	}
	prefix := s.base + "/mcp/apps/"
	if !strings.HasPrefix(resource, prefix) {
		return nil, fosite.ErrInvalidRequest.WithHint("resource 必须是本实例 API 或应用 MCP 规范地址")
	}
	id := strings.TrimPrefix(resource, prefix)
	if id == "" || strings.ContainsAny(id, "/?#%\\") || len(id) > 128 {
		return nil, fosite.ErrInvalidRequest
	}
	return []string{id}, nil
}
func (s *Service) checkGrant(tx *gorm.DB, grant grantRecord) error {
	if grant.RevokedAt != nil || !grant.ExpiresAt.After(time.Now()) {
		return authorization.ErrUnauthenticated
	}
	var user model.User
	if err := tx.First(&user, "id = ?", grant.UserID).Error; err != nil {
		return authorization.ErrUnauthenticated
	}
	if user.Status != model.UserActive || user.AuthVersion != grant.AuthVersion {
		return authorization.ErrUnauthenticated
	}
	var client clientRecord
	if err := tx.First(&client, "id = ? AND active = TRUE", grant.ClientID).Error; err != nil {
		return authorization.ErrUnauthenticated
	}
	if !slices.Contains(client.Resources, grant.Resource) {
		return authorization.ErrUnauthenticated
	}
	for _, scope := range grant.Scopes {
		if !slices.Contains(client.Scopes, scope) {
			return authorization.ErrUnauthenticated
		}
	}
	// 当前角色在业务 Application 授权时再次求交；此处仅复核仍能访问授权应用。
	p := authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion, InstanceAdmin: user.InstanceAdmin}
	for _, id := range grant.ApplicationIDs {
		var app model.Application
		if err := tx.First(&app, "id = ?", id).Error; err != nil {
			return authorization.ErrUnauthenticated
		}
		if _, _, err := s.access.Application(tx, p, app.WorkspaceID, id, authorization.ApplicationRead); err != nil {
			return authorization.ErrUnauthenticated
		}
	}
	return nil
}

// VerifyPrincipal 由父级显式注册到 Access.SetOAuthVerifier。返回范围来自数据库而非 supplied。
func (s *Service) VerifyPrincipal(tx *gorm.DB, supplied authorization.Principal) (authorization.Principal, error) {
	if supplied.AuthKind != "oauth" || supplied.OAuthGrantID == "" || supplied.OAuthTokenSignature == "" {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	var grant grantRecord
	if err := tx.First(&grant, "id = ? AND user_id = ? AND resource = ?", supplied.OAuthGrantID, supplied.UserID, supplied.Audience).Error; err != nil {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	if err := s.checkGrant(tx, grant); err != nil {
		return authorization.Principal{}, err
	}
	ctx := context.WithValue(tx.Statement.Context, txKey{}, &transaction{db: tx})
	req, err := s.store.GetAccessTokenSession(ctx, supplied.OAuthTokenSignature, &session{})
	if err != nil || req.GetID() != grant.ID || !req.GetSession().GetExpiresAt(fosite.AccessToken).After(time.Now()) {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	return authorization.Principal{AuthKind: "oauth", UserID: grant.UserID, AuthVersion: grant.AuthVersion, Audience: grant.Resource, OAuthGrantID: grant.ID, OAuthTokenSignature: supplied.OAuthTokenSignature, Scopes: slices.Clone(grant.Scopes), ApplicationIDs: slices.Clone(grant.ApplicationIDs)}, nil
}
func (s *Service) Authenticate(ctx context.Context, bearer, expectedResource string) (authorization.Principal, error) {
	if len(bearer) > 4096 || bearer == "" {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	_, request, err := s.provider.IntrospectToken(ctx, bearer, fosite.AccessToken, &session{})
	if err != nil {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	sess, ok := request.GetSession().(*session)
	if !ok || sess.Resource != expectedResource {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	supplied := authorization.Principal{AuthKind: "oauth", UserID: sess.Subject, AuthVersion: sess.AuthVersion, OAuthGrantID: sess.GrantID, OAuthTokenSignature: s.strategy.AccessTokenSignature(ctx, bearer), Audience: expectedResource}
	var result authorization.Principal
	err = s.access.Do(ctx, supplied, false, func(_ *gorm.DB, p authorization.Principal) error { result = p; return nil })
	return result, err
}
