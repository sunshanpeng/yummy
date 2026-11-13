package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func invitationView(value model.Invitation) InvitationView {
	return InvitationView{ID: value.ID, WorkspaceID: value.WorkspaceID, Username: value.Username, Role: value.Role, ExpiresAt: value.ExpiresAt, ConsumedAt: value.ConsumedAt, RevokedAt: value.RevokedAt}
}

func (s *Service) CreateInvitation(ctx context.Context, supplied authorization.Principal, workspaceID, username, role string) (NewInvitation, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return NewInvitation{}, err
	}
	if role != model.WorkspaceAdmin && role != model.WorkspaceMemberRole {
		return NewInvitation{}, authorization.ErrInvalid
	}
	plain, err := security.NewToken("ym_inv_")
	if err != nil {
		return NewInvitation{}, err
	}
	id, err := security.NewID("inv")
	if err != nil {
		return NewInvitation{}, err
	}
	value := model.Invitation{ID: id, WorkspaceID: workspaceID, Username: username, Role: role, TokenHash: security.Digest(plain), CreatedBy: supplied.UserID, ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	err = s.Access.Do(ctx, supplied, true, func(tx *gorm.DB, p authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, p, workspaceID); err != nil {
			return err
		}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		return audit.Record(tx, p.UserID, "workspace.invitation.create", "invitation", id, &workspaceID, nil)
	})
	if err != nil {
		return NewInvitation{}, err
	}
	return NewInvitation{Invitation: invitationView(value), Plaintext: plain}, nil
}

// AcceptInvitation 对已有账号要求提供该账号密码，绝不凭邀请令牌接管已有用户。
func (s *Service) AcceptInvitation(ctx context.Context, plaintext, password string) (UserView, error) {
	if len(plaintext) > 128 {
		return UserView{}, authorization.ErrNotFound
	}
	var accepted model.User
	err := s.Access.PublicMutation(ctx, func(tx *gorm.DB) error {
		var invitation model.Invitation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&invitation, "token_hash = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()", security.Digest(plaintext)).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		var workspace model.Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&workspace, "id = ? AND status = ?", invitation.WorkspaceID, model.ResourceActive).Error; err != nil {
			return authorization.ErrNotFound
		}
		// username唯一约束是最终保护；同名新用户并发创建只会有一个成功事务。
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&accepted, "username = ?", invitation.Username).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			hash, err := security.HashPassword(password)
			if err != nil {
				return err
			}
			id, err := security.NewID("usr")
			if err != nil {
				return err
			}
			accepted = model.User{ID: id, Username: invitation.Username, PasswordHash: hash, Status: model.UserActive, AuthVersion: 1}
			if err := tx.Create(&accepted).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if accepted.Status != model.UserActive || !security.VerifyPassword(accepted.PasswordHash, password) {
			return ErrCredentials
		}
		var count int64
		if err := tx.Model(&model.WorkspaceMember{}).Where("workspace_id = ? AND user_id = ?", workspace.ID, accepted.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%w: 已是工作空间成员", authorization.ErrConflict)
		}
		if err := tx.Create(&model.WorkspaceMember{WorkspaceID: workspace.ID, UserID: accepted.ID, Role: invitation.Role}).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		result := tx.Model(&model.Invitation{}).Where("id = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()", invitation.ID).Updates(map[string]any{"consumed_at": now, "consumed_by": accepted.ID})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		return audit.Record(tx, accepted.ID, "workspace.invitation.accept", "invitation", invitation.ID, &workspace.ID, nil)
	})
	return view(accepted), err
}

func (s *Service) ListInvitations(ctx context.Context, p authorization.Principal, workspaceID string) ([]InvitationView, error) {
	result := make([]InvitationView, 0)
	err := s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		var values []model.Invitation
		if err := tx.Where("workspace_id = ?", workspaceID).Order("created_at DESC").Limit(1000).Find(&values).Error; err != nil {
			return err
		}
		for _, value := range values {
			result = append(result, invitationView(value))
		}
		return nil
	})
	return result, err
}

func (s *Service) RevokeInvitation(ctx context.Context, p authorization.Principal, workspaceID, invitationID string) error {
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		var value model.Invitation
		if err := tx.First(&value, "id = ? AND workspace_id = ?", invitationID, workspaceID).Error; err != nil {
			return authorization.ErrNotFound
		}
		if value.RevokedAt != nil {
			return nil
		}
		if value.ConsumedAt != nil {
			return authorization.ErrConflict
		}
		if err := tx.Model(&value).Update("revoked_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "workspace.invitation.revoke", "invitation", value.ID, &workspaceID, nil)
	})
}
