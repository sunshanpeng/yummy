package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) ListMembers(ctx context.Context, p authorization.Principal, workspaceID string, limit int, cursor string) (MemberPage, error) {
	result := MemberPage{Items: make([]MemberView, 0)}
	scope := "workspace-members:" + workspaceID + ":" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		query := tx.Table("workspace_members wm").Select("wm.user_id, u.username, u.status AS user_status, wm.role").Joins("JOIN users u ON u.id = wm.user_id").Where("wm.workspace_id = ?", workspaceID)
		if after != "" {
			query = query.Where("wm.user_id > ?", after)
		}
		var rows []struct {
			UserID     string
			Username   string
			UserStatus string
			Role       string
		}
		if err := query.Order("wm.user_id ASC").Limit(limit + 1).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			result.Items = append(result.Items, MemberView{UserID: row.UserID, Username: row.Username, UserStatus: row.UserStatus, Role: row.Role})
		}
		if len(result.Items) > limit {
			result.NextCursor = store.NextCursor(scope, result.Items[limit-1].UserID)
			result.Items = result.Items[:limit]
		}
		for index := range result.Items {
			member := &result.Items[index]
			member.Applications = make([]ApplicationGrant, 0)
			member.InheritsAllApplications = member.Role == model.WorkspaceAdmin
			if member.InheritsAllApplications {
				continue
			}
			if err := tx.Table("application_members am").Select("am.application_id, a.name AS application_name, am.role").Joins("JOIN applications a ON a.id = am.application_id AND a.workspace_id = am.workspace_id").Where("am.workspace_id = ? AND am.user_id = ? AND a.status = ?", workspaceID, member.UserID, model.ResourceActive).Order("am.application_id ASC").Limit(101).Scan(&member.Applications).Error; err != nil {
				return err
			}
			if len(member.Applications) > 100 {
				member.ApplicationsTruncated = true
				member.Applications = member.Applications[:100]
			}
		}
		return nil
	})
	return result, err
}

func lockWorkspace(tx *gorm.DB, id string) error {
	var value model.Workspace
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "id = ? AND status = ?", id, model.ResourceActive).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return authorization.ErrNotFound
		}
		return err
	}
	return nil
}

func protectLastAdmin(tx *gorm.DB, workspaceID string, member model.WorkspaceMember) error {
	if member.Role != model.WorkspaceAdmin {
		return nil
	}
	var count int64
	if err := tx.Table("workspace_members wm").Joins("JOIN users u ON u.id = wm.user_id").Where("wm.workspace_id = ? AND wm.role = ? AND u.status = ?", workspaceID, model.WorkspaceAdmin, model.UserActive).Count(&count).Error; err != nil {
		return err
	}
	var user model.User
	if err := tx.First(&user, "id = ?", member.UserID).Error; err != nil {
		return err
	}
	if user.Status == model.UserActive && count <= 1 {
		return fmt.Errorf("%w: 不能移除或降级最后一位有效空间管理员", authorization.ErrConflict)
	}
	return nil
}

func (s *Service) ChangeMemberRole(ctx context.Context, p authorization.Principal, workspaceID, userID, role string) error {
	if role != model.WorkspaceAdmin && role != model.WorkspaceMemberRole {
		return authorization.ErrInvalid
	}
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		if err := lockWorkspace(tx, workspaceID); err != nil {
			return err
		}
		var member model.WorkspaceMember
		if err := tx.First(&member, "workspace_id = ? AND user_id = ?", workspaceID, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if member.Role == role {
			return nil
		}
		if role != model.WorkspaceAdmin {
			if err := protectLastAdmin(tx, workspaceID, member); err != nil {
				return err
			}
		}
		if err := tx.Model(&model.WorkspaceMember{}).Where("workspace_id = ? AND user_id = ?", workspaceID, userID).Update("role", role).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "workspace.member.role", "user", userID, &workspaceID, nil)
	})
}

func (s *Service) RemoveMember(ctx context.Context, p authorization.Principal, workspaceID, userID string) error {
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		if err := lockWorkspace(tx, workspaceID); err != nil {
			return err
		}
		var member model.WorkspaceMember
		if err := tx.First(&member, "workspace_id = ? AND user_id = ?", workspaceID, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if err := protectLastAdmin(tx, workspaceID, member); err != nil {
			return err
		}
		var grants []model.ApplicationMember
		if err := tx.Where("workspace_id = ? AND user_id = ?", workspaceID, userID).Find(&grants).Error; err != nil {
			return err
		}
		for _, grant := range grants {
			if err := audit.Record(tx, current.UserID, "application.member.remove", "user", userID, &workspaceID, &grant.ApplicationID); err != nil {
				return err
			}
		}
		if err := tx.Where("workspace_id = ? AND user_id = ?", workspaceID, userID).Delete(&model.ApplicationMember{}).Error; err != nil {
			return err
		}
		if err := tx.Where("workspace_id = ? AND user_id = ?", workspaceID, userID).Delete(&model.WorkspaceMember{}).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "workspace.member.remove", "user", userID, &workspaceID, nil)
	})
}
