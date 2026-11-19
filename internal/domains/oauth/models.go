// Package oauth 把 Fosite 协议处理与持久化存储嵌入现有控制面，不新增身份进程。
package oauth

import (
	"net/url"
	"time"

	"github.com/ory/fosite"
)

type clientRecord struct {
	ID           string `gorm:"column:id"`
	Name         string
	Source       string
	RedirectURIs []string `gorm:"serializer:json;type:jsonb"`
	Scopes       []string `gorm:"serializer:json;type:jsonb"`
	Resources    []string `gorm:"serializer:json;type:jsonb"`
	Active       bool
	CreatedAt    time.Time
}

func (clientRecord) TableName() string { return "oauth_clients" }
func (c clientRecord) fositeClient() fosite.Client {
	return &fosite.DefaultClient{ID: c.ID, RedirectURIs: c.RedirectURIs, Scopes: c.Scopes, Audience: c.Resources, Public: true, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}}
}

type consentRecord struct {
	ID         string     `gorm:"column:id"`
	Parameters url.Values `gorm:"serializer:json;type:jsonb"`
	UserID     *string
	CSRFHash   string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

func (consentRecord) TableName() string { return "oauth_requests" }

type grantRecord struct {
	ID             string `gorm:"column:id"`
	UserID         string
	ClientID       string
	Resource       string
	ApplicationIDs []string `gorm:"serializer:json;type:jsonb"`
	Scopes         []string `gorm:"serializer:json;type:jsonb"`
	AuthVersion    int64
	ExpiresAt      time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
}

func (grantRecord) TableName() string { return "oauth_grants" }

type protocolRecord struct {
	ID        string `gorm:"column:id;primaryKey"`
	Kind      string `gorm:"column:kind"`
	Signature string `gorm:"column:signature"`
	RequestID string
	Payload   []byte `gorm:"type:jsonb"`
	Active    bool
	CreatedAt time.Time
}

func (protocolRecord) IDPrefix() string  { return "ses" }
func (protocolRecord) TableName() string { return "oauth_sessions" }

type assertionRecord struct {
	ID        string `gorm:"column:id;primaryKey"`
	JTIHash   string `gorm:"column:j_t_i_hash"`
	ExpiresAt time.Time
}

func (assertionRecord) IDPrefix() string  { return "asn" }
func (assertionRecord) TableName() string { return "oauth_client_assertions" }

type oidcBindingRecord struct {
	ID        string `gorm:"column:id;primaryKey"`
	Issuer    string `gorm:"column:issuer"`
	Subject   string `gorm:"column:subject"`
	UserID    string
	CreatedAt time.Time
}

func (oidcBindingRecord) IDPrefix() string  { return "oib" }
func (oidcBindingRecord) TableName() string { return "oidc_identity_bindings" }

type oidcLoginRecord struct {
	ID                 string `gorm:"column:id"`
	ProviderName       string
	StateHash          string
	BrowserBindingHash string
	NonceHash          string
	PKCEVerifier       string
	ExpiresAt          time.Time
	ConsumedAt         *time.Time
	CreatedAt          time.Time
}

func (oidcLoginRecord) TableName() string { return "oidc_login_requests" }

type session struct {
	fosite.DefaultSession
	GrantID           string    `json:"grant_id"`
	Resource          string    `json:"resource"`
	AuthVersion       int64     `json:"auth_version"`
	ExpiresAtAbsolute time.Time `json:"expires_at_absolute"`
}

func (s *session) Clone() fosite.Session {
	copy := *s
	copy.ExpiresAt = make(map[fosite.TokenType]time.Time, len(s.ExpiresAt))
	for key, value := range s.ExpiresAt {
		copy.ExpiresAt[key] = value
	}
	return &copy
}

// 刷新期限固定在授权时，不因轮换向后无限滑动。
func (s *session) SetExpiresAt(kind fosite.TokenType, value time.Time) {
	if kind == fosite.RefreshToken && !s.ExpiresAtAbsolute.IsZero() && value.After(s.ExpiresAtAbsolute) {
		value = s.ExpiresAtAbsolute
	}
	s.DefaultSession.SetExpiresAt(kind, value)
}
