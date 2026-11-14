package build

import (
	"context"
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func verifyWorker(tx *gorm.DB, w WorkerIdentity) error {
	if w.NodeID == "" || w.WorkerID == "" || w.CertificateSerial == "" {
		return authorization.ErrUnauthenticated
	}
	var count int64
	err := tx.Table("worker_certificates c").Joins("JOIN execution_nodes n ON n.id = c.node_id AND n.worker_id = c.worker_id").Where("c.serial = ? AND c.node_id = ? AND c.worker_id = ? AND c.revoked_at IS NULL AND c.not_before <= NOW() AND c.not_after > NOW() AND n.status IN ('ACTIVE','DRAINING')", w.CertificateSerial, w.NodeID, w.WorkerID).Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return authorization.ErrUnauthenticated
	}
	return nil
}
func saveBuild(tx *gorm.DB, b *buildRecord) error { b.Version++; return tx.Save(b).Error }
func finish(tx *gorm.DB, b *buildRecord, state, code string) error {
	now := time.Now().UTC()
	b.State = state
	b.ErrorCode = code
	b.FinishedAt = &now
	return saveBuild(tx, b)
}
func requestStop(tx *gorm.DB, b *buildRecord, reason string) error {
	b.DesiredState = "CANCEL"
	b.StopReason = reason
	if b.State == Queued {
		state := Canceled
		if reason == "TIMEOUT" {
			state = TimedOut
		}
		return finish(tx, b, state, "")
	}
	if !terminal(b.State) {
		b.State = Canceling
	}
	// 请求停止后立即可重投；用数据库时钟与领取侧 NOW() 保持同一时间基准。
	if err := tx.Model(&attemptRecord{}).Where("build_id = ?", b.ID).Update("next_dispatch_at", gorm.Expr("NOW()")).Error; err != nil {
		return err
	}
	if err := tx.Model(&artifactRecord{}).Where("build_id = ? AND state = 'VERIFYING'", b.ID).Update("state", "RESIDUAL").Error; err != nil {
		return err
	}
	var a attemptRecord
	if err := tx.Where("build_id = ?", b.ID).Take(&a).Error; err != nil {
		return err
	}
	// worker 成功后尚在仓库验证阶段，也已经有停止证明；不必等不存在的活动 Job。
	if !a.Occupied && !terminal(b.State) {
		state := Canceled
		if reason == "TIMEOUT" {
			state = TimedOut
		}
		return finish(tx, b, state, "")
	}
	return saveBuild(tx, b)
}
func (s *Service) eligible(tx *gorm.DB, b buildRecord) (bool, error) {
	var user model.User
	if err := tx.First(&user, "id = ?", b.ActorID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if user.Status != model.UserActive {
		return false, nil
	}
	p := authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion, InstanceAdmin: user.InstanceAdmin}
	for _, action := range []authorization.Action{authorization.BuildWrite, authorization.SourceRead} {
		if _, _, err := s.access.Application(tx, p, b.WorkspaceID, b.ApplicationID, action); err != nil {
			if errors.Is(err, authorization.ErrNotFound) || errors.Is(err, authorization.ErrDenied) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// Claim 只持数据库锁，不投递网络。node 与 workspace 的容量均计算仍 occupied 的 attempt。
func (s *Service) Claim(ctx context.Context, w WorkerIdentity) (Dispatch, error) {
	var out Dispatch
	claimed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := verifyWorker(tx, w); err != nil {
			return err
		}
		var candidates []buildRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("node_id = ? AND state = ?", w.NodeID, Queued).Order("created_at, id").Limit(1).Find(&candidates).Error; err != nil {
			return err
		}
		for i := range candidates {
			b := &candidates[i]
			eligible, err := s.eligible(tx, *b)
			if err != nil {
				return err
			}
			if !eligible {
				if err := requestStop(tx, b, "ACTOR_INELIGIBLE"); err != nil {
					return err
				}
				if err := audit.Record(tx, b.ActorID, "build.actor_ineligible", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
					return err
				}
				continue
			}
			if !b.DeadlineAt.After(time.Now()) {
				if err := requestStop(tx, b, "TIMEOUT"); err != nil {
					return err
				}
				if err := audit.Record(tx, b.ActorID, "build.queue_timeout", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
					return err
				}
				continue
			}
			var src sourceRecord
			if err := tx.Where("id = ? AND workspace_id = ? AND application_id = ?", b.SourceSnapshotID, b.WorkspaceID, b.ApplicationID).Take(&src).Error; err != nil {
				return err
			}
			if src.State != "READY" || !src.ExpiresAt.After(time.Now()) {
				if err := finish(tx, b, Failed, "SOURCE_EXPIRED"); err != nil {
					return err
				}
				if err := audit.Record(tx, b.ActorID, "build.source_expired", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
					return err
				}
				continue
			}
			var allocation allocationRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("workspace_id = ? AND instance_id = ?", b.WorkspaceID, b.InstanceID).Take(&allocation).Error; err != nil {
				return err
			}
			var node nodeRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&node, "id = ?", b.NodeID).Error; err != nil {
				return err
			}
			if !nodeReady(node, time.Now()) || node.WorkerID != w.WorkerID {
				b.QueueReason = "NODE_NOT_READY"
				if err := saveBuild(tx, b); err != nil {
					return err
				}
				continue
			}
			var allowed int64
			if err := tx.Table("workspace_allocation_nodes").Where("workspace_id = ? AND node_id = ? AND instance_id = ?", b.WorkspaceID, b.NodeID, b.InstanceID).Count(&allowed).Error; err != nil {
				return err
			}
			if allowed != 1 {
				b.QueueReason = "ALLOCATION_CHANGED"
				if err := saveBuild(tx, b); err != nil {
					return err
				}
				continue
			}
			var nodeCount, spaceCount int64
			if err := tx.Model(&attemptRecord{}).Where("node_id = ? AND occupied", b.NodeID).Count(&nodeCount).Error; err != nil {
				return err
			}
			if err := tx.Model(&attemptRecord{}).Where("workspace_id = ? AND occupied", b.WorkspaceID).Count(&spaceCount).Error; err != nil {
				return err
			}
			if nodeCount >= int64(node.Capacity) || spaceCount >= int64(allocation.BuildConcurrency) {
				b.QueueReason = "CAPACITY_BUSY"
				if err := saveBuild(tx, b); err != nil {
					return err
				}
				continue
			}
			id, err := security.NewID("att")
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			a := attemptRecord{ID: id, WorkspaceID: b.WorkspaceID, ApplicationID: b.ApplicationID, BuildID: b.ID, NodeID: b.NodeID, WorkerID: w.WorkerID, JobName: "build-" + hash([]byte(id))[:40], Occupied: true, NextDispatchAt: now.Add(5 * time.Second), DispatchCount: 1, CreatedAt: now}
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
			// 领取租约用数据库时钟表达，避免宿主机与数据库时钟偏差使任务不可领取。
			if err := tx.Model(&a).Update("next_dispatch_at", gorm.Expr("NOW() + INTERVAL '5 seconds'")).Error; err != nil {
				return err
			}
			b.State = Starting
			b.Stage = "PREPARING_SOURCE"
			b.QueueReason = ""
			b.StartedAt = &now
			if err := saveBuild(tx, b); err != nil {
				return err
			}
			if err := audit.Record(tx, b.ActorID, "build.claim", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
				return err
			}
			slug, slugErr := workspaceSlug(tx, b.WorkspaceID)
			if slugErr != nil {
				return slugErr
			}
			out = dispatch(*b, a, slug)
			claimed = true
			return nil
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if !claimed {
		return out, ErrNoCapacity
	}
	return out, nil
}

// workspaceSlug 读取空间标识（统一命名规范），供按空间派生命名空间。
func workspaceSlug(tx *gorm.DB, workspaceID string) (string, error) {
	var ws model.Workspace
	if err := tx.First(&ws, "id = ?", workspaceID).Error; err != nil {
		return "", err
	}
	return ws.Slug, nil
}

func dispatch(b buildRecord, a attemptRecord, spaceSlug string) Dispatch {
	command := "RECONCILE"
	if b.DesiredState == "CANCEL" {
		command = "CANCEL"
	} else if a.JobUID == "" && b.State == Starting {
		command = "RUN"
	}
	return Dispatch{BuildID: b.ID, AttemptID: a.ID, WorkspaceID: b.WorkspaceID, ApplicationID: b.ApplicationID, SpaceSlug: spaceSlug, NodeID: a.NodeID, WorkerID: a.WorkerID, JobName: a.JobName, JobUID: a.JobUID, Command: command, SourceSnapshotID: b.SourceSnapshotID, SourceSHA256: b.SourceSHA256, Config: b.ConfigSnapshot, RegistryID: b.RegistryID, CredentialVersion: b.CredentialVersion, Repository: b.Repository, Tag: b.Tag, Platform: b.Platform, Execution: b.ExecutionSnapshot, DeadlineAt: b.DeadlineAt}
}

// PendingDispatch 从 PG 恢复同一个 attempt；发送失败不会创建新 Job 身份。
// 非 RUN 命令也只能查询/停止原 Job，已绑定 UID 的执行丢失不能补跑。
func (s *Service) PendingDispatch(ctx context.Context, w WorkerIdentity, limit int) ([]Dispatch, error) {
	out := []Dispatch{}
	if limit < 1 || limit > 100 {
		return out, authorization.ErrInvalid
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := verifyWorker(tx, w); err != nil {
			return err
		}
		var records []buildRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("node_id = ? AND state NOT IN ('SUCCEEDED','FAILED','CANCELED','TIMED_OUT') AND EXISTS (SELECT 1 FROM build_attempts a WHERE a.build_id = builds.id AND a.occupied AND a.next_dispatch_at <= NOW())", w.NodeID).Order("created_at, id").Limit(limit).Find(&records).Error; err != nil {
			return err
		}
		for _, b := range records {
			var a attemptRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("build_id = ? AND worker_id = ?", b.ID, w.WorkerID).Take(&a).Error; err != nil {
				return err
			}
			a.DispatchCount++
			a.NextDispatchAt = time.Now().Add(5 * time.Second)
			if err := tx.Save(&a).Error; err != nil {
				return err
			}
			slug, slugErr := workspaceSlug(tx, b.WorkspaceID)
			if slugErr != nil {
				return slugErr
			}
			out = append(out, dispatch(b, a, slug))
		}
		return nil
	})
	return out, err
}
func lockAttempt(tx *gorm.DB, w WorkerIdentity, id string) (buildRecord, attemptRecord, error) {
	var b buildRecord
	var a attemptRecord
	if err := verifyWorker(tx, w); err != nil {
		return b, a, err
	}
	if err := tx.Where("id = ? AND node_id = ? AND worker_id = ?", id, w.NodeID, w.WorkerID).Take(&a).Error; err != nil {
		return b, a, missing(err)
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&b, "id = ?", a.BuildID).Error; err != nil {
		return b, a, err
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&a).Error; err != nil {
		return b, a, err
	}
	return b, a, nil
}

// Reconcile 扫描断线与超时，LOST 保留占用，绝不自动改派或假定进程已停止。
func (s *Service) Reconcile(ctx context.Context, now time.Time) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var records []buildRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("state NOT IN ('SUCCEEDED','FAILED','CANCELED','TIMED_OUT','LOST')").Order("created_at, id").Limit(100).Find(&records).Error; err != nil {
			return err
		}
		for i := range records {
			b := &records[i]
			eligible, err := s.eligible(tx, *b)
			if err != nil {
				return err
			}
			if !eligible && b.DesiredState != "CANCEL" {
				if err := requestStop(tx, b, "ACTOR_INELIGIBLE"); err != nil {
					return err
				}
				if err := audit.Record(tx, b.ActorID, "build.actor_ineligible", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
					return err
				}
			}
			if !terminal(b.State) && !b.DeadlineAt.After(now) && b.StopReason != "TIMEOUT" {
				if err := requestStop(tx, b, "TIMEOUT"); err != nil {
					return err
				}
				if err := audit.Record(tx, b.ActorID, "build.timeout_requested", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
					return err
				}
			}
			if terminal(b.State) || b.State == Queued {
				continue
			}
			var a attemptRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("build_id = ?", b.ID).Take(&a).Error; err != nil {
				return err
			}
			if !a.Occupied {
				continue
			}
			var n nodeRecord
			if err := tx.First(&n, "id = ?", b.NodeID).Error; err != nil {
				return err
			}
			seen := a.CreatedAt
			if a.LastEventAt != nil {
				seen = *a.LastEventAt
			}
			stale := n.LastSeen == nil || now.Sub(*n.LastSeen) > 90*time.Second || now.Sub(seen) > 90*time.Second
			if stale {
				oldState := b.State
				if a.ReconcilingAt == nil {
					v := now
					a.ReconcilingAt = &v
					b.State = Reconciling
				}
				if now.Sub(*a.ReconcilingAt) >= 5*time.Minute {
					b.DesiredState = "CANCEL"
					b.StopReason = "LOST"
					if err := finish(tx, b, Lost, "EXECUTION_UNKNOWN"); err != nil {
						return err
					}
				} else if err := saveBuild(tx, b); err != nil {
					return err
				}
				if b.State != oldState {
					if err := audit.Record(tx, b.ActorID, "build.reconcile."+b.State, "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
						return err
					}
				}
				a.NextDispatchAt = now
				if err := tx.Save(&a).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// StopActor 供账号停用后的收敛调用；只停止构建，不影响已经发布的应用。
func (s *Service) StopActor(ctx context.Context, actor string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var records []buildRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("actor_id = ? AND state NOT IN ('SUCCEEDED','FAILED','CANCELED','TIMED_OUT')", actor).Find(&records).Error; err != nil {
			return err
		}
		for i := range records {
			b := &records[i]
			if err := requestStop(tx, b, "ACTOR_DISABLED"); err != nil {
				return err
			}
			if err := audit.Record(tx, actor, "build.actor_stop", "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunLoop 由控制面显式调用，ctx 是模块生命周期而非某次 HTTP/流请求。
// workers 来自已认证连接层；此循环不启动任何服务，不保留任务的内存事实。
func (s *Service) RunLoop(ctx context.Context, workers func() []WorkerIdentity, d Dispatcher, v ArtifactVerifier, onError func(error)) error {
	report := func(err error) {
		if err != nil && onError != nil {
			onError(err)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			report(s.Reconcile(ctx, now))
			if d != nil && workers != nil {
				for _, w := range workers() {
					items, err := s.PendingDispatch(ctx, w, 32)
					report(err)
					if err == nil {
						for _, item := range items {
							report(d.Dispatch(ctx, item))
						}
					}
					item, err := s.Claim(ctx, w)
					if err == nil {
						report(d.Dispatch(ctx, item))
					} else if !errors.Is(err, ErrNoCapacity) {
						report(err)
					}
				}
			}
			if v != nil {
				report(s.VerifyPending(ctx, v, 16))
			}
		}
	}
}
