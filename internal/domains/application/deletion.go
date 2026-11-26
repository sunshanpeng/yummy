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
			value := s.view(row, "", current)
			var job cleanupJob
			err := tx.Where("workspace_id = ? AND application_id = ?", ws, row.ID).Take(&job).Error
			switch {
			case err == nil:
				value.DeletionState = job.State
				value.DeletionError = job.ErrorCode
				value.DeletingVolumes = job.DeleteVolumes
			case errors.Is(err, gorm.ErrRecordNotFound):
				_, _, targetErr := s.cleanupTarget(tx, ws, row.ID)
				if errors.Is(targetErr, authorization.ErrConflict) {
					value.DeletionState = CleanupWaitingBuilds
				} else if targetErr != nil {
					return targetErr
				} else {
					value.DeletionReady = true
				}
			default:
				return err
			}
			result.Items = append(result.Items, value)
		}
		return nil
	})
	return result, err
}

func (s *Service) cleanupTarget(tx *gorm.DB, ws, id string) (string, string, error) {
	buildNode, err := s.Builds.CleanupBuildNode(tx, ws, id)
	if err != nil {
		return "", "", err
	}
	node, uid, err := s.Runtimes.CleanupPlacement(tx, ws, id)
	if node != "" || err != nil {
		return node, uid, err
	}
	return buildNode, "", nil
}

// Delete 直接删除无需资源清理的应用，或受理保留持久卷的清理任务。
func (s *Service) Delete(ctx context.Context, p authorization.Principal, ws, id string, expectedRevision int64) (string, error) {
	return s.requestDeletion(ctx, p, ws, id, expectedRevision, false, "")
}

func (s *Service) DeleteVolumes(ctx context.Context, p authorization.Principal, ws, id string, expectedRevision int64, confirmation string) (string, error) {
	return s.requestDeletion(ctx, p, ws, id, expectedRevision, true, confirmation)
}

func (s *Service) requestDeletion(ctx context.Context, p authorization.Principal, ws, id string, expectedRevision int64, volumes bool, confirmation string) (string, error) {
	if id == "" || expectedRevision < 1 {
		return "", authorization.ErrInvalid
	}
	var state string
	err := s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, ws); err != nil {
			return err
		}
		// 与空间归档在同一行上串行，防止开始清理后失去人工确认入口。
		var space model.Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Take(&space, "id = ? AND status = ?", ws, model.ResourceActive).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		var row model.Application
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "workspace_id = ? AND id = ?", ws, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if volumes && confirmation != row.Name {
			return fmt.Errorf("%w: 请输入准确的应用名称以确认删除持久卷", authorization.ErrInvalid)
		}
		if row.Status == model.ApplicationDeleted && row.Revision-1 == expectedRevision {
			state = CleanupDeleted
			return nil
		}
		if row.Status != model.ResourceArchived || row.Revision != expectedRevision {
			return authorization.ErrConflict
		}
		var job cleanupJob
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workspace_id = ? AND application_id = ?", ws, id).Take(&job).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if job.ID != "" {
			state = job.State
			if volumes {
				if job.State == CleanupPending && job.DeleteVolumes {
					return nil
				}
				if job.State != CleanupWaitingVolumes && !(job.State == CleanupFailed && job.DeleteVolumes) {
					return authorization.ErrConflict
				}
			} else if job.State != CleanupFailed || job.DeleteVolumes {
				return nil
			}
			state = CleanupPending
			if err := tx.Model(&job).Updates(map[string]any{"state": CleanupPending, "delete_volumes": volumes, "requested_by": current.UserID, "error_code": "", "connection_epoch": 0, "lease_until": gorm.Expr("CURRENT_TIMESTAMP")}).Error; err != nil {
				return err
			}
			return recordCleanupRequest(tx, job, current.UserID, volumes)
		}
		if volumes {
			return authorization.ErrConflict
		}
		node, uid, err := s.cleanupTarget(tx, ws, id)
		if err != nil {
			return err
		}
		if node == "" {
			state = CleanupDeleted
			return markDeleted(tx, row, current.UserID)
		}
		job = cleanupJob{WorkspaceID: ws, ApplicationID: id, NodeID: node, ClusterNodeUID: uid, RequestedBy: current.UserID, State: CleanupPending}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		state = CleanupPending
		return recordCleanupRequest(tx, job, current.UserID, false)
	})
	return state, err
}

func recordCleanupRequest(tx *gorm.DB, job cleanupJob, actor string, volumes bool) error {
	action := "application.cleanup.request"
	if volumes {
		action = "application.cleanup.volumes.request"
	}
	return audit.Record(tx, actor, action, "application_cleanup_job", job.ID, &job.WorkspaceID, &job.ApplicationID)
}

func markDeleted(tx *gorm.DB, row model.Application, actor string) error {
	if err := tx.Model(&row).Updates(map[string]any{"status": model.ApplicationDeleted, "revision": row.Revision + 1}).Error; err != nil {
		return err
	}
	return audit.Record(tx, actor, "application.delete", "application", row.ID, &row.WorkspaceID, &row.ID)
}
