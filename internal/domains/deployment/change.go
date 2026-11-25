package deployment

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) Stop(ctx context.Context, p authorization.Principal, ws, app string, in ChangeInput) (Result, error) {
	return s.change(ctx, p, ws, app, in, Stop)
}

// StoppedForArchive 判断应用能否安全归档：从未部署（无运行状态行）或期望状态
// 和实际观察都已停止且无在途操作。停止失败也会清除在途标记，只有副本归零证据才能支持归档。
// 调用方须持有应用的写授权，
// 方法自身只读运行事实，供应用域归档边界经显式接口依赖。
func (s *Service) StoppedForArchive(tx *gorm.DB, ws, app string) (bool, error) {
	var runtime struct {
		DesiredState      string
		ObservedState     string
		ActiveOperationID *string
	}
	err := tx.Table("application_runtime_states").Select("desired_state, observed_state, active_operation_id").Where("workspace_id = ? AND application_id = ?", ws, app).Take(&runtime).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return runtime.DesiredState == DesiredStop && runtime.ObservedState == Stopped && runtime.ActiveOperationID == nil, nil
}

// HasRuntimeHistory 供删除应用检查是否可能存在集群资源，包含失败的发布。
func (s *Service) HasRuntimeHistory(tx *gorm.DB, ws, app string) (bool, error) {
	var placement Placement
	err := tx.Select("id").Where("workspace_id = ? AND application_id = ?", ws, app).Take(&placement).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}
func (s *Service) Start(ctx context.Context, p authorization.Principal, ws, app string, in ChangeInput) (Result, error) {
	return s.change(ctx, p, ws, app, in, Start)
}
func (s *Service) Restart(ctx context.Context, p authorization.Principal, ws, app string, in ChangeInput) (Result, error) {
	return s.change(ctx, p, ws, app, in, Restart)
}
func (s *Service) change(ctx context.Context, p authorization.Principal, ws, app string, in ChangeInput, action string) (Result, error) {
	if err := validateChange(in.Expected, in.IdempotencyKey); err != nil {
		return Result{}, err
	}
	auth := authorization.RuntimeWrite
	if action == Restart {
		auth = authorization.DeploymentWrite
	}
	hash := fingerprint([]any{action, in})
	var result Result
	var selected releaseRecord
	var duplicate bool
	prepare := func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, auth); err != nil {
			return err
		}
		var err error
		result, duplicate, err = existing(tx, ws, app, current.UserID, in.IdempotencyKey, hash)
		if err != nil || duplicate {
			return err
		}
		state, err := readState(tx, ws, app)
		if err != nil {
			return err
		}
		if err = s.checkExpected(tx, ws, app, state, in.Expected); err != nil {
			return err
		}
		if action != Stop && state.ActiveOperationID != nil {
			return authorization.ErrConflict
		}
		id := text(state.ServingReleaseID)
		if action == Stop {
			id = text(state.DesiredReleaseID)
		}
		if id == "" {
			id = text(state.DesiredReleaseID)
		}
		if id == "" {
			return invalid("应用尚无可运行的发布")
		}
		selected, err = readRelease(tx, ws, app, id)
		if err != nil {
			return err
		}
		if action != Stop {
			if _, err = s.builds.VerifiedArtifact(tx, ws, app, selected.ArtifactID); err != nil {
				return err
			}
			_, err = s.materialize(tx, ws, app, selected.RuntimeConfigRevision)
		}
		return err
	}
	if err := s.access.Do(ctx, p, false, prepare); err != nil {
		return Result{}, err
	}
	if duplicate {
		return result, nil
	}
	verified := selected.ID
	if action != Stop {
		if s.verifier == nil {
			return Result{}, ErrVerification
		}
		if err := s.verifier.Verify(ctx, releaseView(selected).Artifact); err != nil {
			return Result{}, ErrVerification
		}
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := lockApplication(tx, ws, app); err != nil {
			return err
		}
		if err := prepare(tx, current); err != nil || duplicate {
			return err
		}
		if selected.ID != verified {
			return authorization.ErrConflict
		}
		state, err := readState(tx, ws, app)
		if err != nil {
			return err
		}
		if state.ActiveOperationID != nil {
			if action != Stop {
				return authorization.ErrConflict
			}
			now := time.Now().UTC()
			if err = tx.Model(&operationRecord{}).Where("id = ? AND active = TRUE", *state.ActiveOperationID).Updates(map[string]any{"active": false, "state": Superseded, "finished_at": now}).Error; err != nil {
				return err
			}
		}
		op, err := newOperation(tx, &state, selected, current.UserID, action, in.IdempotencyKey, hash, nil)
		if err != nil {
			return err
		}
		if err = audit.Record(tx, current.UserID, "application.runtime."+strings.ToLower(action), "deployment_operation", op.ID, &ws, &app); err != nil {
			return err
		}
		result = Result{Release: releaseView(selected), Operation: operationView(op)}
		return nil
	})
	return result, err
}

func (s *Service) Get(ctx context.Context, p authorization.Principal, ws, app, operationID string) (Result, error) {
	var result Result
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.DeploymentRead); err != nil {
			return err
		}
		var op operationRecord
		if err := tx.First(&op, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, operationID).Error; err != nil {
			return missing(err)
		}
		release, err := readRelease(tx, ws, app, op.ReleaseID)
		result = Result{Release: releaseView(release), Operation: operationView(op)}
		return err
	})
	return result, err
}
func (s *Service) List(ctx context.Context, p authorization.Principal, ws, app string, limit int, cursor string) (Page, error) {
	result := Page{Items: []Result{}}
	scope := "deployments:" + ws + ":" + app + ":" + p.UserID + ":" + p.TokenID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.DeploymentRead); err != nil {
			return err
		}
		query := tx.Where("workspace_id = ? AND application_id = ?", ws, app)
		if after != "" {
			query = query.Where("id > ?", after)
		}
		var ops []operationRecord
		if err := query.Order("id ASC").Limit(limit + 1).Find(&ops).Error; err != nil {
			return err
		}
		if len(ops) > limit {
			result.NextCursor = store.NextCursor(scope, ops[limit-1].ID)
			ops = ops[:limit]
		}
		for _, op := range ops {
			release, err := readRelease(tx, ws, app, op.ReleaseID)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, Result{Release: releaseView(release), Operation: operationView(op)})
		}
		return nil
	})
	return result, err
}
func (s *Service) GetStatus(ctx context.Context, p authorization.Principal, ws, app string) (Status, error) {
	var result Status
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.RuntimeRead); err != nil {
			return err
		}
		v, err := readState(tx, ws, app)
		if err != nil {
			return err
		}
		observed := v.ObservedState
		if v.LastObservedAt != nil && time.Since(*v.LastObservedAt) > 90*time.Second && observed != Stopped {
			observed = Unknown
		}
		result = Status{WorkspaceID: ws, ApplicationID: app, Generation: v.Generation, DesiredState: v.DesiredState, DesiredReleaseID: text(v.DesiredReleaseID), ServingReleaseID: text(v.ServingReleaseID), ServingGeneration: v.ServingGeneration, ObservedGeneration: v.ObservedGeneration, ObservedState: observed, ActiveOperationID: text(v.ActiveOperationID), LastObservedAt: v.LastObservedAt}
		return nil
	})
	return result, err
}

// CheckGeneration 供 worker 的每个 Kubernetes 写操作前调用；不使用长连接授权缓存。
func (s *Service) CheckGeneration(ctx context.Context, worker WorkerIdentity, ws, app, operationID, releaseID string, generation int64, nodeUID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := authenticateWorker(tx, worker); err != nil {
			return err
		}
		state, err := readState(tx, ws, app)
		if err != nil {
			return err
		}
		if state.Generation != generation || text(state.DesiredReleaseID) != releaseID {
			return ErrStaleObservation
		}
		var op operationRecord
		if err := tx.First(&op, "id = ? AND workspace_id = ? AND application_id = ? AND release_id = ? AND generation = ?", operationID, ws, app, releaseID, generation).Error; err != nil {
			return missing(err)
		}
		if text(state.ActiveOperationID) != operationID && op.State != Succeeded {
			return ErrStaleObservation
		}
		release, err := readRelease(tx, ws, app, releaseID)
		if err != nil {
			return err
		}
		if release.PlacementSnapshot.NodeID != worker.NodeID || release.PlacementSnapshot.WorkerID != worker.WorkerID {
			return authorization.ErrDenied
		}
		var node nodeRecord
		if err = tx.First(&node, "id = ?", worker.NodeID).Error; err != nil {
			return err
		}
		if nodeUID == "" || node.ClusterNodeUID != nodeUID || nodeUID != release.PlacementSnapshot.ClusterNodeUID {
			return authorization.ErrDenied
		}
		return nil
	})
}

func authenticateWorker(tx *gorm.DB, w WorkerIdentity) error {
	if w.NodeID == "" || w.WorkerID == "" || w.CertificateSerial == "" || len(w.CertificateSHA256) != 64 {
		return authorization.ErrUnauthenticated
	}
	// 与证书撤销、轮换及节点停用保持 node→certificate 的锁顺序，避免死锁。
	var node nodeRecord
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&node, "id = ? AND worker_id = ? AND status IN ('ACTIVE','DRAINING')", w.NodeID, w.WorkerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return authorization.ErrUnauthenticated
		}
		return err
	}
	var cert certificateRecord
	err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("serial = ? AND node_id = ? AND worker_id = ? AND certificate_sha256 = ? AND revoked_at IS NULL AND not_before <= NOW() AND not_after > NOW()", w.CertificateSerial, w.NodeID, w.WorkerID, w.CertificateSHA256).Take(&cert).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrUnauthenticated
	}
	return err
}

// AuthenticateWorkerTx 在调用方事务中锁住节点和证书，供发送敏感材料前重新验证身份。
// 调用者仍须持有应用操作锁并校验操作与 release；不能将此函数用作外部认证入口。
func AuthenticateWorkerTx(tx *gorm.DB, worker WorkerIdentity) error {
	if tx == nil {
		return authorization.ErrInvalid
	}
	return authenticateWorker(tx, worker)
}
