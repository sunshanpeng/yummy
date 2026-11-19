package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	xoauth2 "golang.org/x/oauth2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrOIDC = errors.New("外部登录失败或身份尚未显式绑定")
var providerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

type OIDCProviderConfig struct{ Name, Issuer, ClientID, ClientSecret, RedirectURI string }
type externalProvider struct {
	name, issuer string
	config       xoauth2.Config
	verifier     *oidc.IDTokenVerifier
	client       *http.Client
}
type OIDCStart struct {
	URL       string
	ExpiresAt time.Time
}

func (s *Service) configureOIDC(ctx context.Context, config OIDCProviderConfig) (*externalProvider, error) {
	if !providerNamePattern.MatchString(config.Name) || config.ClientID == "" || !s.trustedEndpoint(config.Issuer) || !s.trustedEndpoint(config.RedirectURI) {
		return nil, authorization.ErrInvalid
	}
	redirect, _ := url.Parse(config.RedirectURI)
	if !strings.HasPrefix(config.RedirectURI, s.base+"/") || redirect.RawQuery != "" || redirect.Fragment != "" {
		return nil, authorization.ErrInvalid
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return ErrOIDC }}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, config.Issuer)
	if err != nil {
		return nil, ErrOIDC
	}
	endpoint := provider.Endpoint()
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if provider.Claims(&discovery) != nil || !s.trustedEndpoint(discovery.JWKSURI) || !s.trustedEndpoint(endpoint.AuthURL) || !s.trustedEndpoint(endpoint.TokenURL) {
		return nil, ErrOIDC
	}
	return &externalProvider{name: config.Name, issuer: config.Issuer, config: xoauth2.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, RedirectURL: config.RedirectURI, Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID}}, verifier: provider.Verifier(&oidc.Config{ClientID: config.ClientID}), client: client}, nil
}
func (s *Service) trustedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || s.allowLoopback && u.Scheme == "http" && isLoopback(u.Hostname()))
}

// StartOIDC 的 browserBinding 必须由 HTTP 适配层安全Cookie产生；绝不来自任意用户参数。
// 此服务不建立网页登录会话，最终 Principal 交给现有 session 层建立和轮换 Cookie。
func (s *Service) StartOIDC(ctx context.Context, name, browserBinding string) (OIDCStart, error) {
	provider, exists := s.oidcProviders[name]
	if !exists || len(browserBinding) < 32 || len(browserBinding) > 1024 {
		return OIDCStart{}, ErrOIDC
	}
	state, err := security.NewToken("oidcs_")
	if err != nil {
		return OIDCStart{}, err
	}
	nonce, err := security.NewToken("oidcn_")
	if err != nil {
		return OIDCStart{}, err
	}
	verifier, err := security.NewToken("")
	if err != nil {
		return OIDCStart{}, err
	}
	id, err := security.NewID("oidcr")
	if err != nil {
		return OIDCStart{}, err
	}
	now := time.Now().UTC()
	record := oidcLoginRecord{ID: id, ProviderName: name, StateHash: security.Digest(state), BrowserBindingHash: security.Digest(browserBinding), NonceHash: security.Digest(nonce), PKCEVerifier: verifier, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return OIDCStart{}, err
	}
	hash := sha256.Sum256([]byte(verifier))
	target := provider.config.AuthCodeURL(state, oidc.Nonce(nonce), xoauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(hash[:])), xoauth2.SetAuthURLParam("code_challenge_method", "S256"))
	return OIDCStart{URL: target, ExpiresAt: record.ExpiresAt}, nil
}

func (s *Service) CompleteOIDC(ctx context.Context, name, state, code, browserBinding string) (authorization.Principal, error) {
	provider, exists := s.oidcProviders[name]
	if !exists || len(state) > 1024 || len(code) == 0 || len(code) > 4096 || len(browserBinding) < 32 || len(browserBinding) > 1024 {
		return authorization.Principal{}, ErrOIDC
	}
	// 先原子单次消费再访问 IdP；网络失败需重新登录，不允许同 code 重试/竞争替换身份。
	var stored oidcLoginRecord
	var pkceVerifier string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, "state_hash=? AND provider_name=?", security.Digest(state), name).Error; err != nil {
			return ErrOIDC
		}
		if stored.ConsumedAt != nil || !stored.ExpiresAt.After(time.Now()) || subtle.ConstantTimeCompare([]byte(stored.BrowserBindingHash), []byte(security.Digest(browserBinding))) != 1 {
			return ErrOIDC
		}
		pkceVerifier = stored.PKCEVerifier
		now := time.Now().UTC()
		return tx.Model(&stored).Updates(map[string]any{"consumed_at": now, "pkce_verifier": ""}).Error
	})
	if err != nil {
		return authorization.Principal{}, ErrOIDC
	}
	ctx = oidc.ClientContext(ctx, provider.client)
	token, err := provider.config.Exchange(ctx, code, xoauth2.SetAuthURLParam("code_verifier", pkceVerifier))
	if err != nil {
		return authorization.Principal{}, ErrOIDC
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || len(rawID) > 64<<10 {
		return authorization.Principal{}, ErrOIDC
	}
	verified, err := provider.verifier.Verify(ctx, rawID)
	if err != nil || verified.Subject == "" || subtle.ConstantTimeCompare([]byte(stored.NonceHash), []byte(security.Digest(verified.Nonce))) != 1 {
		return authorization.Principal{}, ErrOIDC
	}
	var binding oidcBindingRecord
	if err := s.db.WithContext(ctx).First(&binding, "issuer=? AND subject=?", provider.issuer, verified.Subject).Error; err != nil {
		return authorization.Principal{}, ErrOIDC
	}
	var user model.User
	if err := s.db.WithContext(ctx).First(&user, "id=? AND status=?", binding.UserID, model.UserActive).Error; err != nil {
		return authorization.Principal{}, ErrOIDC
	}
	supplied := authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion}
	var result authorization.Principal
	err = s.access.Do(ctx, supplied, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := audit.Record(tx, current.UserID, "account.oidc.login", "user", current.UserID, nil, nil); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, err
}

// BindExternalIdentity 只允许全局管理员把受配置的 issuer/sub 绑定到现有用户。
// 不读取 email、不创建用户、不添加空间成员，已有绑定不能被静默转移。
func (s *Service) BindExternalIdentity(ctx context.Context, p authorization.Principal, providerName, subject, userID string) error {
	provider, exists := s.oidcProviders[providerName]
	if !exists || subject == "" || len(subject) > 1024 {
		return authorization.ErrInvalid
	}
	return s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var user model.User
		if err := tx.First(&user, "id=? AND status=?", userID, model.UserActive).Error; err != nil {
			return authorization.ErrNotFound
		}
		value := oidcBindingRecord{Issuer: provider.issuer, Subject: subject, UserID: userID}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&value)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var previous oidcBindingRecord
			if err := tx.First(&previous, "issuer=? AND subject=?", provider.issuer, subject).Error; err != nil {
				return err
			}
			if previous.UserID != userID {
				return authorization.ErrConflict
			}
			return nil
		}
		return audit.Record(tx, current.UserID, "instance.oidc.bind", "user", userID, nil, nil)
	})
}
