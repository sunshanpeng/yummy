package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"gorm.io/gorm"
)

type Connection struct {
	ID             string    `json:"id"`
	ClientID       string    `json:"clientId"`
	Resource       string    `json:"resource"`
	ApplicationIDs []string  `json:"applicationIds"`
	Scopes         []string  `json:"scopes"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Revoked        bool      `json:"revoked"`
}

func (s *Service) ListConnections(ctx context.Context, p authorization.Principal) ([]Connection, error) {
	result := []Connection{}
	if p.IsBearer() {
		return nil, authorization.ErrDenied
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		var grants []grantRecord
		if err := tx.Where("user_id = ?", current.UserID).Order("created_at DESC,id").Limit(100).Find(&grants).Error; err != nil {
			return err
		}
		for _, value := range grants {
			result = append(result, Connection{ID: value.ID, ClientID: value.ClientID, Resource: value.Resource, ApplicationIDs: value.ApplicationIDs, Scopes: value.Scopes, CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt, Revoked: value.RevokedAt != nil})
		}
		return nil
	})
	return result, err
}
func (s *Service) RevokeConnection(ctx context.Context, p authorization.Principal, id string) error {
	if p.IsBearer() {
		return authorization.ErrDenied
	}
	return s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		var grant grantRecord
		if err := tx.First(&grant, "id = ? AND user_id = ?", id, current.UserID).Error; err != nil {
			return authorization.ErrNotFound
		}
		now := time.Now().UTC()
		if err := tx.Model(&grant).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&protocolRecord{}).Where("request_id = ?", id).Update("active", false).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "account.oauth.revoke", "oauth_grant", id, nil, nil)
	})
}
func (s *Service) SetClientActive(ctx context.Context, p authorization.Principal, id string, active bool) error {
	return s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var client clientRecord
		if err := tx.First(&client, "id=?", id).Error; err != nil {
			return authorization.ErrNotFound
		}
		if err := tx.Model(&client).Update("active", active).Error; err != nil {
			return err
		}
		if !active {
			now := time.Now().UTC()
			if err := tx.Model(&grantRecord{}).Where("client_id=? AND revoked_at IS NULL", id).Update("revoked_at", now).Error; err != nil {
				return err
			}
			if err := tx.Model(&protocolRecord{}).Where("request_id IN (SELECT id FROM oauth_grants WHERE client_id=?)", id).Update("active", false).Error; err != nil {
				return err
			}
		}
		return audit.Record(tx, current.UserID, "instance.oauth.client.status", "oauth_client", id, nil, nil)
	})
}

func (s *Service) Metadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"issuer": s.base, "authorization_endpoint": s.base + "/oauth/authorize", "token_endpoint": s.base + "/oauth/token", "revocation_endpoint": s.base + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": append(append([]string{}, authorization.TokenScopes...), OfflineAccess), "authorization_response_iss_parameter_supported": true, "client_id_metadata_document_supported": true})
}
func (s *Service) ProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("applicationID")
	if id == "" {
		id = r.PathValue("applicationId")
	}
	resource := s.base + "/mcp/apps/" + id
	if _, err := s.resourceApplications(resource); err != nil {
		http.Error(w, "resource invalid", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource, "authorization_servers": []string{s.base}, "scopes_supported": []string{"build:read", "logs:read", "build:write", "deployment:read", "deployment:write", "runtime:read", "runtime:write", "config:read", "config:write"}, "bearer_methods_supported": []string{"header"}})
}
func (s *Service) WWWAuthenticate(applicationID string) string {
	return fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp/apps/%s"`, s.base, applicationID)
}

// CleanupExpiredRequests 批量删除已过期的授权请求记录。授权端点允许匿名发起
// （登录后回到同意流），记录若不清理会无界增长；授权消费本身按主键+FOR UPDATE
// +ConsumedAt 保护，删除过期行不影响任何在途流。供控制面后台清理循环调用。
func (s *Service) CleanupExpiredRequests(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, authorization.ErrInvalid
	}
	var removed int64
	for _, record := range []any{&consentRecord{}, &oidcLoginRecord{}} {
		db := s.db.WithContext(ctx)
		expired := db.Model(record).Select("id").Where("expires_at < NOW()").Order("expires_at, id").Limit(limit)
		result := db.Where("id IN (?)", expired).Delete(record)
		removed += result.RowsAffected
		if result.Error != nil {
			return removed, result.Error
		}
	}
	return removed, nil
}
