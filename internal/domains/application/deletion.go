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

func (s *Service) ListArchived(ctx context.Context, p authorization.Principal, ws string, limit int, cursor string) (Page, error) {
	result := Page{Items: []View{}}
	scope := "archived-applications:" + ws + ":" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, ws); err != nil {
			return err
		}
		var rows []model.Application
		query := tx.Where("workspace_id = ? AND status = ?", ws, model.ResourceArchived)
		if after != "" {
			query = query.Where("id > ?", after)
		}
		if err := query.Order("id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > limit {
			result.NextCursor = store.NextCursor(scope, rows[limit-1].ID)
			rows = rows[:limit]
		}
		for _, row := range rows {
			ready, err := s.deletionReady(tx, ws, row.ID)
			if err != nil {
				return err
			}
			value := s.view(row, "", current)
			value.DeletionReady = ready
			result.Items = append(result.Items, value)
		}
		return nil
	})
	return result, err
}

func (s *Service) deletionReady(tx *gorm.DB, ws, id string) (bool, error) {
	built, err := s.Builds.HasBuildHistory(tx, ws, id)
	if built || err != nil {
		return false, err
	}
	placed, err := s.Runtimes.HasRuntimeHistory(tx, ws, id)
	return !placed && err == nil, err
}

// Delete 释放没有执行历史的归档应用名称。保留原 ID，历史记录与新应用保持隔离。
func (s *Service) Delete(ctx context.Context, p authorization.Principal, ws, id string, expectedRevision int64) error {
	if id == "" || expectedRevision < 1 {
		return authorization.ErrInvalid
	}
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, ws); err != nil {
			return err
		}
		var row model.Application
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "workspace_id = ? AND id = ?", ws, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if row.Status == model.ApplicationDeleted && row.Revision-1 == expectedRevision {
			return nil
		}
		if row.Status != model.ResourceArchived || row.Revision != expectedRevision {
			return authorization.ErrConflict
		}
		ready, err := s.deletionReady(tx, ws, id)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("%w: 应用有构建或部署历史，其集群资源尚待清理", authorization.ErrConflict)
		}
		if err := tx.Model(&row).Updates(map[string]any{"status": model.ApplicationDeleted, "revision": row.Revision + 1}).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "application.delete", "application", id, &ws, &id)
	})
}
