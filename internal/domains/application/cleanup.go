package application

import (
	"context"
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	CleanupPending        = "PENDING"
	CleanupWaitingVolumes = "WAITING_VOLUMES"
	CleanupWaitingBuilds  = "WAITING_BUILDS"
	CleanupFailed         = "FAILED"
	CleanupDeleted        = "DELETED"
	cleanupLeaseSeconds   = 45
)

type cleanupJob struct {
	ID              string    `gorm:"column:id;primaryKey"`
	WorkspaceID     string    `gorm:"column:workspace_id"`
	ApplicationID   string    `gorm:"column:application_id"`
	NodeID          string    `gorm:"column:node_id"`
	ClusterNodeUID  string    `gorm:"column:cluster_node_uid"`
	RequestedBy     string    `gorm:"column:requested_by"`
	DeleteVolumes   bool      `gorm:"column:delete_volumes"`
	State           string    `gorm:"column:state"`
	ErrorCode       string    `gorm:"column:error_code"`
	LeaseRevision   int64     `gorm:"column:lease_revision"`
	ConnectionEpoch int64     `gorm:"column:connection_epoch"`
	LeaseUntil      time.Time `gorm:"column:lease_until;default:CURRENT_TIMESTAMP"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
	CreatedBy       string    `gorm:"column:created_by"`
	UpdatedBy       string    `gorm:"column:updated_by"`
}

func (cleanupJob) TableName() string { return "application_cleanup_jobs" }
func (cleanupJob) IDPrefix() string  { return "cln" }

type CleanupWorker struct {
	NodeID, ClusterUID string
	ConnectionEpoch    int64
}

type CleanupLease struct {
	ID       string
	Revision int64
}

type CleanupDispatch struct {
	Lease                                          CleanupLease
	WorkspaceID, ApplicationID, SpaceSlug, AppName string
	DeleteVolumes                                  bool
}

type CleanupResult struct {
	Done, HasVolumes bool
	ErrorCode        string
}

// HasPendingCleanup 供工作空间归档使用，防止归档后无法继续确认删卷或重试。
func (s *Service) HasPendingCleanup(tx *gorm.DB, workspaceID string) (bool, error) {
	var count int64
	err := tx.Model(&cleanupJob{}).Where("workspace_id = ? AND state <> ?", workspaceID, CleanupDeleted).Count(&count).Error
	return count > 0, err
}

// ClaimCleanup 只向历史运行位置或构建节点派发，节点重登记后仍须匹配首次核验的集群身份。
func (s *Service) ClaimCleanup(ctx context.Context, w CleanupWorker) (CleanupDispatch, error) {
	var result CleanupDispatch
	if w.NodeID == "" || w.ClusterUID == "" || w.ConnectionEpoch < 1 {
		return result, authorization.ErrInvalid
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job cleanupJob
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("node_id = ? AND state = ? AND lease_until <= CURRENT_TIMESTAMP AND (cluster_node_uid = '' OR cluster_node_uid = ?)", w.NodeID, CleanupPending, w.ClusterUID).
			Order("lease_until, id").Take(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var row model.Application
		if err := tx.Where("workspace_id = ? AND id = ? AND status = ?", job.WorkspaceID, job.ApplicationID, model.ResourceArchived).Take(&row).Error; err != nil {
			return err
		}
		var ws model.Workspace
		if err := tx.Select("slug").Take(&ws, "id = ?", job.WorkspaceID).Error; err != nil {
			return err
		}
		revision := job.LeaseRevision + 1
		if err := tx.Model(&job).Updates(map[string]any{"cluster_node_uid": w.ClusterUID, "lease_revision": revision, "connection_epoch": w.ConnectionEpoch, "lease_until": store.NowPlusSeconds(tx, cleanupLeaseSeconds)}).Error; err != nil {
			return err
		}
		result = CleanupDispatch{Lease: CleanupLease{ID: job.ID, Revision: revision}, WorkspaceID: row.WorkspaceID, ApplicationID: row.ID, SpaceSlug: ws.Slug, AppName: row.Name, DeleteVolumes: job.DeleteVolumes}
		return nil
	})
	return result, err
}

func cleanupLeaseQuery(tx *gorm.DB, w CleanupWorker, lease CleanupLease) *gorm.DB {
	return tx.Where("id = ? AND lease_revision = ? AND node_id = ? AND cluster_node_uid = ? AND connection_epoch = ? AND state = ? AND lease_until > CURRENT_TIMESTAMP", lease.ID, lease.Revision, w.NodeID, w.ClusterUID, w.ConnectionEpoch, CleanupPending)
}

func cleanupLeaseError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrConflict
	}
	return err
}

func (s *Service) CheckCleanup(ctx context.Context, w CleanupWorker, lease CleanupLease) error {
	if lease.ID == "" || lease.Revision < 1 || w.ConnectionEpoch < 1 {
		return authorization.ErrInvalid
	}
	var job cleanupJob
	return cleanupLeaseError(cleanupLeaseQuery(s.DB.WithContext(ctx), w, lease).Select("id").Take(&job).Error)
}

func (s *Service) CompleteCleanup(ctx context.Context, w CleanupWorker, lease CleanupLease, result CleanupResult) error {
	if lease.ID == "" || lease.Revision < 1 || w.ConnectionEpoch < 1 || (result.ErrorCode != "" && !validCleanupError(result.ErrorCode)) || (result.ErrorCode != "" && result.Done) {
		return authorization.ErrInvalid
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job cleanupJob
		if err := cleanupLeaseQuery(tx, w, lease).Take(&job).Error; err != nil {
			return cleanupLeaseError(err)
		}
		// 与管理员重试保持应用行、任务行的加锁顺序，并在持锁后再核对租约。
		var row model.Application
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "workspace_id = ? AND id = ? AND status = ?", job.WorkspaceID, job.ApplicationID, model.ResourceArchived).Error; err != nil {
			return cleanupLeaseError(err)
		}
		if err := cleanupLeaseQuery(tx.Clauses(clause.Locking{Strength: "UPDATE"}), w, lease).Take(&job).Error; err != nil {
			return cleanupLeaseError(err)
		}
		updates := map[string]any{"connection_epoch": 0, "lease_until": store.NowPlusSeconds(tx, 5)}
		switch {
		case result.ErrorCode != "":
			updates["state"], updates["error_code"] = CleanupFailed, result.ErrorCode
			if err := audit.Record(tx, job.RequestedBy, "application.cleanup.failed", "application_cleanup_job", job.ID, &job.WorkspaceID, &job.ApplicationID); err != nil {
				return err
			}
		case result.Done && result.HasVolumes:
			if job.DeleteVolumes {
				return authorization.ErrInvalid
			}
			updates["state"] = CleanupWaitingVolumes
		case result.Done:
			updates["state"] = CleanupDeleted
			if err := markDeleted(tx, row, job.RequestedBy); err != nil {
				return err
			}
		}
		return tx.Model(&job).Updates(updates).Error
	})
}

func validCleanupError(code string) bool {
	switch code {
	case "OWNERSHIP_CONFLICT", "PERMISSION_DENIED", "KUBERNETES_UNAVAILABLE":
		return true
	}
	return false
}
