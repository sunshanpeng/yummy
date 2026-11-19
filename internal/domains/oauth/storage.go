package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/ory/fosite"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
)

type txKey struct{}
type transaction struct {
	db               *gorm.DB
	owned            bool
	securityMutation bool
}
type storage struct{ db *gorm.DB }

func (s *storage) database(ctx context.Context) *gorm.DB {
	if tx, ok := ctx.Value(txKey{}).(*transaction); ok {
		return tx.db.WithContext(ctx)
	}
	return s.db.WithContext(ctx)
}
func (s *storage) BeginTX(ctx context.Context) (context.Context, error) {
	if _, ok := ctx.Value(txKey{}).(*transaction); ok {
		return ctx, nil
	}
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return ctx, tx.Error
	}
	return context.WithValue(ctx, txKey{}, &transaction{db: tx, owned: true}), nil
}
func (s *storage) Commit(ctx context.Context) error {
	if tx, ok := ctx.Value(txKey{}).(*transaction); ok && tx.owned {
		return tx.db.Commit().Error
	}
	return nil
}
func (s *storage) Rollback(ctx context.Context) error {
	if tx, ok := ctx.Value(txKey{}).(*transaction); ok && tx.owned {
		return tx.db.Rollback().Error
	}
	return nil
}

func (s *storage) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	var client clientRecord
	err := s.database(ctx).First(&client, "id = ? AND active = TRUE", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return client.fositeClient(), nil
}
func (s *storage) ClientAssertionJWTValid(ctx context.Context, jti string) error {
	var count int64
	if err := s.database(ctx).Model(&assertionRecord{}).Where("jti_hash = ? AND expires_at > ?", security.Digest(jti), time.Now()).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return fosite.ErrJTIKnown
	}
	return nil
}
func (s *storage) SetClientAssertionJWT(ctx context.Context, jti string, exp time.Time) error {
	return s.database(ctx).Create(&assertionRecord{JTIHash: security.Digest(jti), ExpiresAt: exp}).Error
}

type storedRequest struct {
	ID                string     `json:"id"`
	RequestedAt       time.Time  `json:"requested_at"`
	ClientID          string     `json:"client_id"`
	RequestedScopes   []string   `json:"requested_scopes"`
	GrantedScopes     []string   `json:"granted_scopes"`
	RequestedAudience []string   `json:"requested_audience"`
	GrantedAudience   []string   `json:"granted_audience"`
	Form              url.Values `json:"form"`
	Session           *session   `json:"session"`
}

func encodeRequest(req fosite.Requester) ([]byte, error) {
	sess, ok := req.GetSession().(*session)
	if !ok {
		return nil, fosite.ErrServerError
	}
	// 仅保留协议验证所需的非秘密表单，不持久化 code/verifier/token/client_secret。
	form := url.Values{}
	for _, key := range []string{"redirect_uri", "code_challenge", "code_challenge_method", "resource"} {
		if value := req.GetRequestForm()[key]; len(value) > 0 {
			form[key] = value
		}
	}
	return json.Marshal(storedRequest{ID: req.GetID(), RequestedAt: req.GetRequestedAt(), ClientID: req.GetClient().GetID(), RequestedScopes: req.GetRequestedScopes(), GrantedScopes: req.GetGrantedScopes(), RequestedAudience: req.GetRequestedAudience(), GrantedAudience: req.GetGrantedAudience(), Form: form, Session: sess})
}
func (s *storage) decode(ctx context.Context, raw []byte) (fosite.Requester, error) {
	var value storedRequest
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	client, err := s.GetClient(ctx, value.ClientID)
	if err != nil {
		return nil, err
	}
	if value.Session == nil {
		return nil, fosite.ErrNotFound
	}
	return &fosite.Request{ID: value.ID, RequestedAt: value.RequestedAt, Client: client, RequestedScope: value.RequestedScopes, GrantedScope: value.GrantedScopes, RequestedAudience: value.RequestedAudience, GrantedAudience: value.GrantedAudience, Form: value.Form, Session: value.Session}, nil
}
func (s *storage) create(ctx context.Context, kind, signature string, req fosite.Requester) error {
	payload, err := encodeRequest(req)
	if err != nil {
		return err
	}
	return s.database(ctx).Create(&protocolRecord{Kind: kind, Signature: signature, RequestID: req.GetID(), Payload: payload, Active: true}).Error
}
func (s *storage) get(ctx context.Context, kind, signature string) (fosite.Requester, error) {
	var record protocolRecord
	if err := s.database(ctx).First(&record, "kind = ? AND signature = ?", kind, signature).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fosite.ErrNotFound
		}
		return nil, err
	}
	req, err := s.decode(ctx, record.Payload)
	if err != nil {
		return nil, err
	}
	if !record.Active {
		if kind == "code" {
			return req, fosite.ErrInvalidatedAuthorizeCode
		}
		return req, fosite.ErrInactiveToken
	}
	return req, nil
}
func (s *storage) inactive(ctx context.Context, kind, signature string) error {
	result := s.database(ctx).Model(&protocolRecord{}).Where("kind = ? AND signature = ? AND active = TRUE", kind, signature).Update("active", false)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fosite.ErrInactiveToken
	}
	return nil
}
func (s *storage) CreateAuthorizeCodeSession(ctx context.Context, sig string, req fosite.Requester) error {
	return s.create(ctx, "code", sig, req)
}
func (s *storage) GetAuthorizeCodeSession(ctx context.Context, sig string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(ctx, "code", sig)
}
func (s *storage) InvalidateAuthorizeCodeSession(ctx context.Context, sig string) error {
	return s.inactive(ctx, "code", sig)
}
func (s *storage) CreateAccessTokenSession(ctx context.Context, sig string, req fosite.Requester) error {
	return s.create(ctx, "access", sig, req)
}
func (s *storage) GetAccessTokenSession(ctx context.Context, sig string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(ctx, "access", sig)
}
func (s *storage) DeleteAccessTokenSession(ctx context.Context, sig string) error {
	return s.inactive(ctx, "access", sig)
}
func (s *storage) CreateRefreshTokenSession(ctx context.Context, sig, _ string, req fosite.Requester) error {
	return s.create(ctx, "refresh", sig, req)
}
func (s *storage) GetRefreshTokenSession(ctx context.Context, sig string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(ctx, "refresh", sig)
}
func (s *storage) DeleteRefreshTokenSession(ctx context.Context, sig string) error {
	result := s.database(ctx).Model(&protocolRecord{}).Where("kind = 'refresh' AND signature = ?", sig).Update("active", false)
	return result.Error
}
func (s *storage) RotateRefreshToken(ctx context.Context, _ string, sig string) error {
	return s.inactive(ctx, "refresh", sig)
}
func (s *storage) CreatePKCERequestSession(ctx context.Context, sig string, req fosite.Requester) error {
	return s.create(ctx, "pkce", sig, req)
}
func (s *storage) GetPKCERequestSession(ctx context.Context, sig string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(ctx, "pkce", sig)
}
func (s *storage) DeletePKCERequestSession(ctx context.Context, sig string) error {
	return s.inactive(ctx, "pkce", sig)
}
func (s *storage) RevokeAccessToken(ctx context.Context, id string) error  { return s.revoke(ctx, id) }
func (s *storage) RevokeRefreshToken(ctx context.Context, id string) error { return s.revoke(ctx, id) }
func (s *storage) revoke(ctx context.Context, id string) error {
	if tx, ok := ctx.Value(txKey{}).(*transaction); ok {
		tx.securityMutation = true
	}
	db := s.database(ctx)
	now := time.Now().UTC()
	if err := db.Model(&grantRecord{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", now).Error; err != nil {
		return err
	}
	return db.Model(&protocolRecord{}).Where("request_id = ?", id).Update("active", false).Error
}
