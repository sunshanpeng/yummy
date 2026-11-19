package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) ListMembers(ctx context.Context, p authorization.Principal, workspaceID, applicationID string, limit int, cursor string) (MemberPage, error) {
	result := MemberPage{Items: make([]MemberView, 0)}
	scope := "application-members:" + workspaceID + ":" + applicationID + ":" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		if _, _, err := s.Access.Application(tx, current, workspaceID, applicationID, authorization.MemberWrite); err != nil {
			return err
		}
		query := tx.Table("workspace_members wm").Select("wm.user_id, u.username, u.status AS user_status, CASE WHEN wm.role = 'admin' THEN 'workspace_admin' ELSE am.role END AS role, wm.role = 'admin' AS inherited").Joins("JOIN users u ON u.id = wm.user_id").Joins("LEFT JOIN application_members am ON am.workspace_id = wm.workspace_id AND am.user_id = wm.user_id AND am.application_id = ?", applicationID).Where("wm.workspace_id = ? AND (wm.role = 'admin' OR am.user_id IS NOT NULL)", workspaceID)
		if after != "" {
			query = query.Where("wm.user_id > ?", after)
		}
		if err := query.Order("wm.user_id ASC").Limit(limit + 1).Scan(&result.Items).Error; err != nil {
			return err
		}
		if len(result.Items) > limit {
			result.NextCursor = store.NextCursor(scope, result.Items[limit-1].UserID)
			result.Items = result.Items[:limit]
		}
		return nil
	})
	return result, err
}

func (s *Service) memberTarget(tx *gorm.DB, current authorization.Principal, workspaceID, applicationID, userID string, requireActive bool) (model.WorkspaceMember, error) {
	var member model.WorkspaceMember
	if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
		return member, err
	}
	// 与空间移除/变更角色共用空间锁，避免授权落到已离开的成员上。
	var workspace model.Workspace
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&workspace, "id = ? AND status = ?", workspaceID, model.ResourceActive).Error; err != nil {
		return member, err
	}
	if _, _, err := s.Access.Application(tx, current, workspaceID, applicationID, authorization.MemberWrite); err != nil {
		return member, err
	}
	if err := tx.First(&member, "workspace_id = ? AND user_id = ?", workspaceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return member, authorization.ErrNotFound
		}
		return member, err
	}
	if member.Role == model.WorkspaceAdmin {
		return member, fmt.Errorf("%w: 空间管理员继承应用访问权，不能显式授予或收回", authorization.ErrInvalid)
	}
	if requireActive {
		var user model.User
		if err := tx.First(&user, "id = ? AND status = ?", userID, model.UserActive).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return member, authorization.ErrNotFound
			}
			return member, err
		}
	}
	return member, nil
}

func (s *Service) SetMember(ctx context.Context, p authorization.Principal, workspaceID, applicationID, userID, role string) error {
	if role != model.ApplicationMaintainer && role != model.ApplicationDeveloper && role != model.ApplicationViewer {
		return authorization.ErrInvalid
	}
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if _, err := s.memberTarget(tx, current, workspaceID, applicationID, userID, true); err != nil {
			return err
		}
		var member model.ApplicationMember
		err := tx.First(&member, "workspace_id = ? AND application_id = ? AND user_id = ?", workspaceID, applicationID, userID).Error
		if err == nil {
			if member.Role == role {
				return nil
			}
			if err := tx.Model(&model.ApplicationMember{}).Where("workspace_id = ? AND application_id = ? AND user_id = ?", workspaceID, applicationID, userID).Update("role", role).Error; err != nil {
				return err
			}
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(&model.ApplicationMember{WorkspaceID: workspaceID, ApplicationID: applicationID, UserID: userID, Role: role}).Error; err != nil {
				return err
			}
		} else {
			return err
		}
		return audit.Record(tx, current.UserID, "application.member.set", "user", userID, &workspaceID, &applicationID)
	})
}

func (s *Service) RemoveMember(ctx context.Context, p authorization.Principal, workspaceID, applicationID, userID string) error {
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if _, err := s.memberTarget(tx, current, workspaceID, applicationID, userID, false); err != nil {
			return err
		}
		deleted := tx.Where("workspace_id = ? AND application_id = ? AND user_id = ?", workspaceID, applicationID, userID).Delete(&model.ApplicationMember{})
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected == 0 {
			return authorization.ErrNotFound
		}
		return audit.Record(tx, current.UserID, "application.member.remove", "user", userID, &workspaceID, &applicationID)
	})
}
