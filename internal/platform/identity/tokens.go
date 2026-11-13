package identity

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
)

func (s *Service) CreateToken(ctx context.Context, supplied authorization.Principal, input CreateTokenInput) (NewToken, error) {
	if supplied.IsBearer() {
		return NewToken{}, authorization.ErrDenied
	}
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) == 0 || len(input.Name) > 100 || len(input.ApplicationIDs) == 0 || len(input.ApplicationIDs) > 100 || len(input.Scopes) == 0 {
		return NewToken{}, authorization.ErrInvalid
	}
	input.Scopes = slices.Compact(slices.Sorted(slices.Values(input.Scopes)))
	input.ApplicationIDs = slices.Compact(slices.Sorted(slices.Values(input.ApplicationIDs)))
	for _, scope := range input.Scopes {
		if !slices.Contains(authorization.TokenScopes, scope) {
			return NewToken{}, fmt.Errorf("%w: 不支持的凭证权限", authorization.ErrInvalid)
		}
	}
	now := time.Now().UTC()
	if input.ExpiresAt.IsZero() {
		input.ExpiresAt = now.Add(30 * 24 * time.Hour)
	}
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(90*24*time.Hour)) {
		return NewToken{}, fmt.Errorf("%w: 凭证有效期最长90天", authorization.ErrInvalid)
	}
	plain, err := security.NewToken("ym_pat_")
	if err != nil {
		return NewToken{}, err
	}
	id, err := security.NewID("pat")
	if err != nil {
		return NewToken{}, err
	}
	var token model.AccessToken
	err = s.Access.Do(ctx, supplied, true, func(tx *gorm.DB, p authorization.Principal) error {
		links := make([]model.AccessTokenApplication, 0, len(input.ApplicationIDs))
		for _, appID := range input.ApplicationIDs {
			var app model.Application
			if err := tx.First(&app, "id = ?", appID).Error; err != nil {
				return authorization.ErrNotFound
			}
			for _, scope := range input.Scopes {
				if _, _, err := s.Access.Application(tx, p, app.WorkspaceID, app.ID, authorization.Action(scope)); err != nil {
					return authorization.ErrDenied
				}
			}
			links = append(links, model.AccessTokenApplication{TokenID: id, WorkspaceID: app.WorkspaceID, ApplicationID: app.ID})
		}
		token = model.AccessToken{ID: id, UserID: p.UserID, Name: input.Name, TokenHash: security.Digest(plain), Scopes: input.Scopes, AuthVersion: p.AuthVersion, ExpiresAt: input.ExpiresAt}
		if err := tx.Create(&token).Error; err != nil {
			return err
		}
		if err := tx.Create(&links).Error; err != nil {
			return err
		}
		return audit.Record(tx, p.UserID, "account.token.create", "access_token", id, nil, nil)
	})
	if err != nil {
		return NewToken{}, err
	}
	return NewToken{Plaintext: plain, Token: tokenView(token, input.ApplicationIDs)}, nil
}

func tokenView(token model.AccessToken, applicationIDs []string) TokenView {
	return TokenView{ID: token.ID, Name: token.Name, Scopes: token.Scopes, ApplicationIDs: applicationIDs, ExpiresAt: token.ExpiresAt, CreatedAt: token.CreatedAt, RevokedAt: token.RevokedAt, LastUsedAt: token.LastUsedAt}
}

func (s *Service) ListTokens(ctx context.Context, supplied authorization.Principal) ([]TokenView, error) {
	result := make([]TokenView, 0)
	err := s.Access.Do(ctx, supplied, false, func(tx *gorm.DB, p authorization.Principal) error {
		if p.IsBearer() {
			return authorization.ErrDenied
		}
		var tokens []model.AccessToken
		if err := tx.Where("user_id = ?", p.UserID).Order("created_at DESC").Limit(1000).Find(&tokens).Error; err != nil {
			return err
		}
		for _, token := range tokens {
			var links []model.AccessTokenApplication
			if err := tx.Where("token_id = ?", token.ID).Order("application_id").Find(&links).Error; err != nil {
				return err
			}
			ids := make([]string, 0, len(links))
			for _, link := range links {
				ids = append(ids, link.ApplicationID)
			}
			result = append(result, tokenView(token, ids))
		}
		return nil
	})
	return result, err
}

func (s *Service) RevokeToken(ctx context.Context, supplied authorization.Principal, tokenID string) error {
	return s.Access.Do(ctx, supplied, true, func(tx *gorm.DB, p authorization.Principal) error {
		if p.IsBearer() {
			return authorization.ErrDenied
		}
		// 即使是实例管理员，也不能撤销或查看其他用户拥有的凭证。
		var token model.AccessToken
		if err := tx.First(&token, "id = ? AND user_id = ?", tokenID, p.UserID).Error; err != nil {
			return authorization.ErrNotFound
		}
		if token.RevokedAt != nil {
			return nil
		}
		if err := tx.Model(&token).Update("revoked_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		return audit.Record(tx, p.UserID, "account.token.revoke", "access_token", token.ID, nil, nil)
	})
}
