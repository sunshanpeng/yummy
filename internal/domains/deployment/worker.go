package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ClaimPending 使用短数据库租约重投同一操作；同一 operation ID 的重启标记永不变化。
// STOP 的优先级高于发布，旧命令仍必须在每次写 Kubernetes 前调用 CheckGeneration。
func (s *Service) ClaimPending(ctx context.Context, worker WorkerIdentity) (Dispatch, error) {
	var result Dispatch
	var candidate operationRecord
	var verifiedRelease releaseRecord
	resumed := false
	// 镜像/历史凭据检查在事务外；停止不依赖仓库或旧秘密仍然可读。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := authenticateWorker(tx, worker); err != nil {
			return err
		}
		err := tx.Table("deployment_operations AS o").Select("o.*").Joins("JOIN application_placements p ON p.workspace_id = o.workspace_id AND p.application_id = o.application_id").Where("o.active = TRUE AND o.next_dispatch_at <= NOW() AND p.node_id = ? AND p.worker_id = ?", worker.NodeID, worker.WorkerID).Order("CASE WHEN o.action = 'STOP' THEN 0 ELSE 1 END, o.next_dispatch_at, o.id").Take(&candidate).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 无可领取操作时尝试合成观察恢复：worker 重启丢失内存中的运行采集器，
			// 已完成的部署操作不会重投，恢复只能由控制面按停更事实主动下发。
			// 恢复命令不动集群资源，不需要镜像校验与第二段事务。
			resume, resumeErr := s.claimResume(tx, worker)
			if resumeErr != nil {
				return resumeErr
			}
			result = resume
			resumed = true
			return nil
		}
		if err != nil {
			return err
		}
		verifiedRelease, err = readRelease(tx, candidate.WorkspaceID, candidate.ApplicationID, candidate.ReleaseID)
		if err != nil {
			return err
		}
		// 标记候选：在访问仓库核验之前先把到期时间推后 dispatchMarkSeconds，
		// 核验失败（或进程崩溃）的候选天然不再占队首，同 worker 其余到期任务
		// 继续领取；条件更新保证只有仍到期且活跃的候选被标记，并发竞争输家
		// 在此让路。
		marked := tx.Model(&operationRecord{}).Where("id = ? AND active = TRUE AND next_dispatch_at <= NOW()", candidate.ID).Update("next_dispatch_at", store.NowPlusSeconds(tx, dispatchMarkSeconds))
		if marked.Error != nil {
			return marked.Error
		}
		if marked.RowsAffected == 0 {
			return ErrNoPending
		}
		return nil
	}); err != nil {
		return result, err
	}
	if resumed {
		return result, nil
	}
	if candidate.Action != Stop {
		if s.verifier == nil {
			return result, ErrVerification
		}
		if err := s.verifier.Verify(ctx, releaseView(verifiedRelease).Artifact); err != nil {
			// 候选已在第一段事务标记：核验失败不再额外处理，到期后由下一轮
			// 重新标记并重试。
			return result, ErrVerification
		}
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := authenticateWorker(tx, worker); err != nil {
			return err
		}
		if err := lockApplication(tx, candidate.WorkspaceID, candidate.ApplicationID); err != nil {
			return err
		}
		var op operationRecord
		// 复核改为 dispatch_count CAS：候选已在第一段事务被标记出队，比较到期
		// 时间会永远失配；并发领取者（如网关重连重叠）只有一个能把计数从候选
		// 快照值推进，第二个领取者在此失败。其余守卫（generation/active op/
		// 节点/产物）照旧复核。
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND active = TRUE AND dispatch_count = ?", candidate.ID, candidate.DispatchCount).Take(&op).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNoPending
		}
		if err != nil {
			return err
		}
		state, err := readState(tx, op.WorkspaceID, op.ApplicationID)
		if err != nil {
			return err
		}
		if state.Generation != op.Generation || text(state.ActiveOperationID) != op.ID {
			return ErrNoPending
		}
		release, err := readRelease(tx, op.WorkspaceID, op.ApplicationID, op.ReleaseID)
		if err != nil {
			return err
		}
		var node nodeRecord
		if err = tx.First(&node, "id = ?", worker.NodeID).Error; err != nil {
			return err
		}
		if node.ClusterNodeUID != release.PlacementSnapshot.ClusterNodeUID {
			return authorization.ErrDenied
		}
		var material runtimeconfig.Materialized
		if op.Action != Stop {
			artifact, err := s.builds.VerifiedArtifact(tx, op.WorkspaceID, op.ApplicationID, release.ArtifactID)
			if err != nil {
				return err
			}
			if verifiedArtifactSnapshot(artifact) != releaseView(verifiedRelease).Artifact {
				return authorization.ErrConflict
			}
			material, err = s.materialize(tx, op.WorkspaceID, op.ApplicationID, release.RuntimeConfigRevision)
			if err != nil {
				return err
			}
		} else {
			material.View = runtimeconfig.View{WorkspaceID: op.WorkspaceID, ApplicationID: op.ApplicationID, Revision: release.RuntimeConfigRevision, Type: release.PlacementSnapshot.RuntimeType, Confirmed: true}
		}
		// 默认域名属于冻结发布，不修改已经保存的配置版本。
		material.View.Domains = append([]string{}, release.Domains...)
		var spaceSlug, appName string
		if err := tx.Table("workspaces").Select("slug").Where("id = ?", op.WorkspaceID).Take(&spaceSlug).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Application{}).Select("name").Where("workspace_id = ? AND id = ?", op.WorkspaceID, op.ApplicationID).Take(&appName).Error; err != nil {
			return err
		}
		result = Dispatch{WorkspaceID: op.WorkspaceID, ApplicationID: op.ApplicationID, SpaceSlug: spaceSlug, AppName: appName, OperationID: op.ID, ReleaseID: release.ID, Action: op.Action, Generation: op.Generation, NextEventSeq: op.EventSeq + 1, Placement: release.PlacementSnapshot, Artifact: releaseView(release).Artifact, Config: material, Domains: release.Domains}
		// 入口 TLS 是实例级运行设施的当下事实，不冻结进发布：证书轮换不应要求
		// 重新发布。领取时按 release.Domains 查 application_domain_claims 当前绑定，
		// 全部域名绑定同一非空 Secret 才下发 HTTPS；存在未绑定或绑定不一致时
		// 保持 TLS=nil（失败关闭），入口探测将因缺少 TLS 证据无法收敛，绝不猜证书。
		tls, err := convergeTLS(tx, release.Domains)
		if err != nil {
			return err
		}
		result.TLS = tls
		if op.Action == Restart {
			result.RestartMarker = op.ID
		}
		return tx.Model(&op).Updates(map[string]any{"dispatch_count": gorm.Expr("dispatch_count + 1"), "next_dispatch_at": store.NowPlusSeconds(tx, 15)}).Error
	})
	return result, err
}

const (
	// resumeStaleAfter：观察停更多久才合成恢复命令，取观察间隔的数倍，容忍
	// 网络抖动与单次观察失败；resumeBackoffWindow：同节点同发布的合成退避，
	// 恢复完成前（LastObservedAt 刷新）的周期轮询不重复下发。
	resumeStaleAfterSeconds = 120
	resumeBackoffWindow     = time.Minute
	// dispatchMarkSeconds 是领取候选的核验标记窗口：候选在访问仓库前先出队
	// 该窗口，核验成功立即领取（租约覆盖标记），失败或崩溃后按窗口重试。
	dispatchMarkSeconds = 60
)

// claimResume 在无可领取操作时为观察停更的 serving release 合成观察恢复命令
// （Action=RECONCILE）：worker 重启丢失内存中的运行采集器（运行日志/指标），
// 而已完成部署操作不会重投，恢复只能由控制面按停更事实主动下发。worker 收到
// 后不动集群资源，只重建观察循环与采集器，观察经 monitoring 路径续传
// （ApplyObservation：op SUCCEEDED、无在飞操作、DesiredState=RUN）。
// 只排除停止意图（DesiredState != RUN）：停止后的应用不再观察，绝不为它合成。
func (s *Service) claimResume(tx *gorm.DB, worker WorkerIdentity) (Dispatch, error) {
	var result Dispatch
	var candidate struct {
		WorkspaceID, ApplicationID, ServingReleaseID string
	}
	err := tx.Table("application_placements AS p").
		Select("p.workspace_id, p.application_id, st.serving_release_id").
		Joins("JOIN application_runtime_states st ON st.workspace_id = p.workspace_id AND st.application_id = p.application_id").
		Where("p.node_id = ? AND p.worker_id = ?", worker.NodeID, worker.WorkerID).
		Where("st.serving_release_id IS NOT NULL AND st.serving_release_id <> ''").
		Where("st.desired_state = ?", Run).
		Where("st.active_operation_id IS NULL OR st.active_operation_id = ''").
		Where("st.last_observed_at IS NULL OR st.last_observed_at < NOW() - (? * INTERVAL '1 second')", resumeStaleAfterSeconds).
		Order("st.last_observed_at ASC NULLS FIRST").
		Limit(1).
		Take(&candidate).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, ErrNoPending
		}
		return result, err
	}
	key := worker.NodeID + "|" + candidate.ServingReleaseID
	s.resumeMu.Lock()
	if next, ok := s.resumeBackoff[key]; ok && time.Now().Before(next) {
		s.resumeMu.Unlock()
		return result, ErrNoPending
	}
	if s.resumeBackoff == nil {
		s.resumeBackoff = map[string]time.Time{}
	}
	s.resumeBackoff[key] = time.Now().Add(resumeBackoffWindow)
	s.resumeMu.Unlock()
	release, err := readRelease(tx, candidate.WorkspaceID, candidate.ApplicationID, candidate.ServingReleaseID)
	if err != nil {
		return result, err
	}
	placement := release.PlacementSnapshot
	if placement.NodeID != worker.NodeID || placement.WorkerID != worker.WorkerID {
		return result, ErrNoPending
	}
	var node nodeRecord
	if err := tx.First(&node, "id = ?", worker.NodeID).Error; err != nil {
		return result, err
	}
	if node.ClusterNodeUID != placement.ClusterNodeUID {
		return result, ErrNoPending
	}
	// serving 操作即恢复后观察续传的目标操作；绝不为停止意图（STOP serving）
	// 合成——DesiredState 过滤已挡住，这里再按操作事实兜底。
	var op operationRecord
	if err := tx.Where("workspace_id = ? AND application_id = ? AND release_id = ? AND state = ?", candidate.WorkspaceID, candidate.ApplicationID, candidate.ServingReleaseID, Succeeded).Order("created_at DESC").Take(&op).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, ErrNoPending
		}
		return result, err
	}
	material, err := s.materialize(tx, candidate.WorkspaceID, candidate.ApplicationID, release.RuntimeConfigRevision)
	if err != nil {
		return result, err
	}
	var spaceSlug, appName string
	if err := tx.Table("workspaces").Select("slug").Where("id = ?", candidate.WorkspaceID).Take(&spaceSlug).Error; err != nil {
		return result, err
	}
	if err := tx.Model(&model.Application{}).Select("name").Where("workspace_id = ? AND id = ?", candidate.WorkspaceID, candidate.ApplicationID).Take(&appName).Error; err != nil {
		return result, err
	}
	result = Dispatch{WorkspaceID: candidate.WorkspaceID, ApplicationID: candidate.ApplicationID, SpaceSlug: spaceSlug, AppName: appName, OperationID: op.ID, ReleaseID: release.ID, Action: Reconcile, Generation: op.Generation, NextEventSeq: op.EventSeq + 1, Placement: release.PlacementSnapshot, Artifact: releaseView(release).Artifact, Config: material, Domains: release.Domains}
	return result, nil
}

func (s *Service) ApplyObservation(ctx context.Context, worker WorkerIdentity, event Observation) (Ack, error) {
	if event.Generation < 1 || event.Seq < 1 || event.CollectedAt.IsZero() || event.CollectedAt.After(time.Now().Add(30*time.Second)) || time.Since(event.CollectedAt) > 5*time.Minute || len(event.DeploymentUID) > 256 || len(event.PodUID) > 256 || len(event.ClusterNodeUID) > 256 || event.Replicas < 0 || event.ReadyReplicas < 0 || event.Replicas > 1000 || event.ReadyReplicas > 1000 {
		return Ack{}, authorization.ErrInvalid
	}
	switch event.Kind {
	case Applying, WaitingReady, "OBSERVED", "STOPPED", "LOST", "FAILED":
	default:
		return Ack{}, authorization.ErrInvalid
	}
	if event.ErrorCode != "" && !safeCode.MatchString(event.ErrorCode) {
		return Ack{}, authorization.ErrInvalid
	}
	hash := fingerprint(event)
	var ack Ack
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := authenticateWorker(tx, worker); err != nil {
			return err
		}
		if err := lockApplication(tx, event.WorkspaceID, event.ApplicationID); err != nil {
			return err
		}
		state, err := readState(tx, event.WorkspaceID, event.ApplicationID)
		if err != nil {
			return err
		}
		var op operationRecord
		if err = tx.First(&op, "workspace_id = ? AND application_id = ? AND id = ?", event.WorkspaceID, event.ApplicationID, event.OperationID).Error; err != nil {
			return missing(err)
		}
		release, err := readRelease(tx, event.WorkspaceID, event.ApplicationID, op.ReleaseID)
		if err != nil {
			return err
		}
		placement := release.PlacementSnapshot
		if placement.NodeID != worker.NodeID || placement.WorkerID != worker.WorkerID || placement.ClusterNodeUID != event.ClusterNodeUID || op.ReleaseID != event.ReleaseID {
			return authorization.ErrDenied
		}
		var node nodeRecord
		if err = tx.First(&node, "id = ?", worker.NodeID).Error; err != nil {
			return err
		}
		if node.ClusterNodeUID != placement.ClusterNodeUID {
			return authorization.ErrDenied
		}
		// 先拒绝旧代际，再处理重复；停止之后旧成功重放也不能被当作有效观察。
		if state.Generation != event.Generation || op.Generation != event.Generation || text(state.DesiredReleaseID) != event.ReleaseID || op.State == Superseded {
			return ErrStaleObservation
		}
		var old observationRecord
		err = tx.First(&old, "operation_id = ? AND seq = ?", op.ID, event.Seq).Error
		if err == nil {
			if old.PayloadHash != hash {
				return ErrSequence
			}
			ack = Ack{Seq: event.Seq, Duplicate: true, State: op.State}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		monitoring := op.State == Succeeded && state.ActiveOperationID == nil
		if !monitoring && (!op.Active || text(state.ActiveOperationID) != op.ID) {
			return ErrStaleObservation
		}
		if event.Seq != op.EventSeq+1 {
			return ErrSequence
		}
		if op.LastObservedAt != nil && event.CollectedAt.Before(*op.LastObservedAt) {
			return ErrStaleObservation
		}
		if event.DeploymentUID != "" {
			if op.DeploymentUID != "" && op.DeploymentUID != event.DeploymentUID {
				return authorization.ErrConflict
			}
			op.DeploymentUID = event.DeploymentUID
		}
		op.EventSeq = event.Seq
		op.LastObservedAt = &event.CollectedAt
		state.LastObservedAt = &event.CollectedAt
		state.ObservedGeneration = event.Generation
		payload := observationRecord{WorkspaceID: event.WorkspaceID, ApplicationID: event.ApplicationID, OperationID: op.ID, Seq: event.Seq, NodeID: worker.NodeID, WorkerID: worker.WorkerID, CertificateSerial: worker.CertificateSerial, PayloadHash: hash, Payload: event, CollectedAt: event.CollectedAt}
		if err = tx.Create(&payload).Error; err != nil {
			return err
		}
		if monitoring {
			// 历史发布结果保持 SUCCEEDED，后续失联或不健康仅更新运行观察事实。
			op.KubernetesReady = observationReady(event, release)
			op.PodUID = event.PodUID
			switch {
			case event.Kind == "LOST":
				state.ObservedState = Unknown
			case op.Action == Stop:
				if event.Kind == "STOPPED" && event.CorrectGeneration && event.DeploymentUID != "" && event.Replicas == 0 && event.ReadyReplicas == 0 && event.OldPodsGone {
					state.ObservedState = Stopped
				} else {
					state.ObservedState = Unknown
				}
			case !op.KubernetesReady:
				state.ObservedState = "UNHEALTHY"
			case placement.RuntimeType == runtimeconfig.TypeHTTP:
				// 新 Pod 的就绪不能沿用旧 Pod 的入口成功；下一次控制面探测重新确认。
				state.ObservedState = Unknown
			default:
				serve(&state, &op)
			}
			if err = tx.Save(&op).Error; err != nil {
				return err
			}
			if err = tx.Save(&state).Error; err != nil {
				return err
			}
			ack = Ack{Seq: event.Seq, State: op.State}
			return nil
		}
		switch event.Kind {
		case "LOST":
			op.State = Reconciling
			op.KubernetesReady = false
			state.ObservedState = Unknown
		case "FAILED":
			op.ErrorCode = event.ErrorCode
			if op.ErrorCode == "" {
				op.ErrorCode = "DEPLOYMENT_FAILED"
			}
			if err = finish(tx, &state, &op, Failed); err != nil {
				return err
			}
			// 历史服务版本不被失败发布覆盖；当前健康证据不充分，显示 UNKNOWN。
			state.ObservedState = "UNHEALTHY"
			if state.ServingReleaseID != nil {
				state.ObservedState = Unknown
			}
		case "STOPPED":
			if op.Action != Stop || state.DesiredState != "STOP" || !event.CorrectGeneration || event.DeploymentUID == "" || event.Replicas != 0 || event.ReadyReplicas != 0 || !event.OldPodsGone {
				return invalid("停止缺少副本归零或旧实例退出证据")
			}
			state.ObservedState = Stopped
			state.PodUID = ""
			state.DeploymentUID = event.DeploymentUID
			if err = finish(tx, &state, &op, Succeeded); err != nil {
				return err
			}
		default:
			if op.Action == Stop {
				op.State = Applying
				state.ObservedState = "STOPPING"
				break
			}
			ready := observationReady(event, release)
			op.KubernetesReady = ready
			op.PodUID = event.PodUID
			if ready {
				if placement.RuntimeType == runtimeconfig.TypeHTTP {
					op.State = VerifyingIngress
				} else {
					serve(&state, &op)
					if err = finish(tx, &state, &op, Succeeded); err != nil {
						return err
					}
				}
			} else {
				op.State = WaitingReady
			}
		}
		if err = tx.Save(&op).Error; err != nil {
			return err
		}
		if err = tx.Save(&state).Error; err != nil {
			return err
		}
		if event.Kind == "FAILED" && release.AutoRecoveryEnabled && release.PreviousServingID != nil && (op.Action == Deploy || op.Action == Rollback) {
			if err = s.scheduleRecovery(tx, &state, op, release); err != nil {
				return err
			}
		}
		ack = Ack{Seq: event.Seq, State: op.State}
		return nil
	})
	return ack, err
}
func observationReady(event Observation, release releaseRecord) bool {
	ready := event.Kind == "OBSERVED" && event.CorrectGeneration && event.DeploymentUID != "" && event.PodUID != "" && event.Digest == release.Digest && event.Replicas == 1 && event.ReadyReplicas == 1 && event.OldPodsGone
	if release.PlacementSnapshot.RuntimeType == runtimeconfig.TypeHTTP {
		ready = ready && event.ServiceEndpoints
	}
	return ready
}

// convergeTLS 汇总发布全部域名的当前 TLS 绑定：同一非空 Secret 返回 HTTPS 事实，
// 任一域名未绑定或绑定不一致（含声明行缺失——发布受理时 claimDomains 必然落库，
// 缺失只能是数据异常）都返回 nil。
func convergeTLS(tx *gorm.DB, domains []string) (*DispatchTLS, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	var claims []domainRecord
	if err := tx.Where("domain IN ?", domains).Find(&claims).Error; err != nil {
		return nil, err
	}
	if len(claims) != len(domains) {
		return nil, nil
	}
	secret := ""
	for _, claim := range claims {
		if claim.TLSSecretName == "" {
			return nil, nil
		}
		if secret == "" {
			secret = claim.TLSSecretName
		} else if secret != claim.TLSSecretName {
			return nil, nil
		}
	}
	return &DispatchTLS{HTTPS: true, SecretName: secret}, nil
}

func serve(state *stateRecord, op *operationRecord) {
	state.ServingReleaseID = &op.ReleaseID
	state.ServingGeneration = op.Generation
	state.ObservedState = "RUNNING"
	state.DeploymentUID = op.DeploymentUID
	state.PodUID = op.PodUID
}
func finish(tx *gorm.DB, state *stateRecord, op *operationRecord, result string) error {
	now := time.Now().UTC()
	op.Active = false
	op.State = result
	op.FinishedAt = &now
	state.ActiveOperationID = nil
	if err := tx.Save(op).Error; err != nil {
		return err
	}
	return audit.Record(tx, op.ActorID, "application.deployment."+stringLower(result), "deployment_operation", op.ID, &op.WorkspaceID, &op.ApplicationID)
}
func stringLower(value string) string {
	switch value {
	case Succeeded:
		return "succeeded"
	case Failed:
		return "failed"
	}
	return "changed"
}

// scheduleRecovery 只入队一次恢复意图，仍由 ClaimPending 的不可变配置和外部 verifier 受理检查保护。
// 自动恢复不制造新业务授权；动作继承已受理操作的审计 actor，最多一层，不形成无限循环。
func (s *Service) scheduleRecovery(tx *gorm.DB, state *stateRecord, failed operationRecord, release releaseRecord) error {
	previous, err := readRelease(tx, release.WorkspaceID, release.ApplicationID, text(release.PreviousServingID))
	if err != nil {
		return err
	}
	id, err := security.NewID("rel")
	if err != nil {
		return err
	}
	recovery := previous
	recovery.ID = id
	recovery.CreatedAt = time.Time{}
	recovery.Source = Recovery
	recovery.RollbackOf = &previous.ID
	recovery.PreviousServingID = state.ServingReleaseID
	recovery.AutoRecoveryEnabled = false
	recovery.ActorID = failed.ActorID
	if err = tx.Create(&recovery).Error; err != nil {
		return err
	}
	op, err := newOperation(tx, state, recovery, failed.ActorID, Recovery, "recovery:"+failed.ID, fingerprint([]string{Recovery, failed.ID}), &failed.ID)
	if err != nil {
		return err
	}
	return audit.Record(tx, failed.ActorID, "application.deployment.recovery", "deployment_operation", op.ID, &op.WorkspaceID, &op.ApplicationID)
}

func (s *Service) ProbeTargets(ctx context.Context, limit int) ([]ProbeTarget, error) {
	if limit < 1 || limit > 100 {
		return nil, authorization.ErrInvalid
	}
	result := []ProbeTarget{}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ops []operationRecord
		// 同代可有多个 SUCCEEDED 操作（同产物重部署）：按应用取最新一条作探测
		// 目标——历史操作过期后 ApplyProbe 必被拒绝，混入目标会令探测循环卡在
		// 最旧记录上，永远轮不到当前 serving 操作。
		// 排序把待验证发布（active VERIFYING_INGRESS）排在运行巡检（SUCCEEDED
		// 复探测）之前：已发布成功且频繁出证的应用不得长期占满批次，把停在
		// VERIFYING_INGRESS 的发布饿死在窗口外；巡检按最近一次探测证据时间
		// 升序轮转（未探测过的最先），每轮优先补最久未探测的目标。
		// 【数据库专有】ASC NULLS FIRST 为 PostgreSQL 语法；MySQL 升序默认
		// NULL 在前，可去除此子句。
		if err := tx.Table("deployment_operations AS o").Select("o.*").Joins("JOIN application_runtime_states s ON s.workspace_id = o.workspace_id AND s.application_id = o.application_id AND s.generation = o.generation AND s.desired_release_id = o.release_id").Where("o.kubernetes_ready = TRUE AND o.action <> 'STOP' AND ((o.active = TRUE AND o.state = ?) OR (o.state = ? AND s.active_operation_id IS NULL)) AND o.id = (SELECT MAX(o2.id) FROM deployment_operations o2 WHERE o2.workspace_id = o.workspace_id AND o2.application_id = o.application_id AND o2.generation = o.generation)", VerifyingIngress, Succeeded).Order(clause.OrderBy{Expression: clause.Expr{SQL: "CASE WHEN o.active = TRUE AND o.state = ? THEN 0 ELSE 1 END, (SELECT MAX(p.observed_at) FROM deployment_probe_evidence p WHERE p.operation_id = o.id) ASC NULLS FIRST, o.id ASC", Vars: []interface{}{VerifyingIngress}}}).Limit(limit).Find(&ops).Error; err != nil {
			return err
		}
		for _, op := range ops {
			release, err := readRelease(tx, op.WorkspaceID, op.ApplicationID, op.ReleaseID)
			if err != nil {
				return err
			}
			material, err := s.materialize(tx, op.WorkspaceID, op.ApplicationID, release.RuntimeConfigRevision)
			if err != nil {
				return err
			}
			for _, domain := range release.Domains {
				// 连接固定走平台管理的默认域名：自定义域名公网解析可能仍指向
				// 旧站点，直连会把旧站点的 2xx 响应误记为本次发布的证据；默认
				// 域名的 DNS 由平台管理，连接必然终止在平台入口。快照缺省时
				// 退回直连（历史数据兜底）。
				connect := release.PlacementSnapshot.DefaultDomain
				if connect == "" {
					connect = domain
				}
				result = append(result, ProbeTarget{WorkspaceID: op.WorkspaceID, ApplicationID: op.ApplicationID, OperationID: op.ID, ReleaseID: op.ReleaseID, Generation: op.Generation, Domain: domain, ConnectDomain: connect, Path: material.View.Probe.Path, PodUID: op.PodUID})
			}
		}
		return nil
	})
	return result, err
}
func (s *Service) ApplyProbe(ctx context.Context, e ProbeEvidence) error {
	if e.Generation < 1 || e.PodUID == "" || e.ObservedAt.IsZero() || e.ObservedAt.After(time.Now().Add(30*time.Second)) || time.Since(e.ObservedAt) > time.Minute {
		return authorization.ErrInvalid
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockApplication(tx, e.WorkspaceID, e.ApplicationID); err != nil {
			return err
		}
		state, err := readState(tx, e.WorkspaceID, e.ApplicationID)
		if err != nil {
			return err
		}
		var op operationRecord
		if err = tx.First(&op, "id = ? AND workspace_id = ? AND application_id = ?", e.OperationID, e.WorkspaceID, e.ApplicationID).Error; err != nil {
			return missing(err)
		}
		monitoring := op.State == Succeeded && state.ActiveOperationID == nil && state.DesiredState == Run
		pending := op.Active && text(state.ActiveOperationID) == op.ID && op.State == VerifyingIngress
		if state.Generation != e.Generation || text(state.DesiredReleaseID) != op.ReleaseID || (!monitoring && !pending) || !op.KubernetesReady || op.PodUID != e.PodUID || op.LastObservedAt == nil || e.ObservedAt.Before(*op.LastObservedAt) || time.Since(*op.LastObservedAt) > 90*time.Second {
			return ErrStaleObservation
		}
		release, err := readRelease(tx, e.WorkspaceID, e.ApplicationID, op.ReleaseID)
		if err != nil {
			return err
		}
		if release.PlacementSnapshot.RuntimeType != runtimeconfig.TypeHTTP {
			return authorization.ErrInvalid
		}
		material, err := s.materialize(tx, e.WorkspaceID, e.ApplicationID, release.RuntimeConfigRevision)
		if err != nil {
			return err
		}
		if material.View.Probe.Path != e.Path {
			return authorization.ErrDenied
		}
		found := false
		for _, domain := range release.Domains {
			if domain == e.Domain {
				found = true
			}
		}
		if !found {
			return authorization.ErrDenied
		}
		var claim domainRecord
		if err = tx.First(&claim, "domain = ? AND workspace_id = ? AND application_id = ?", e.Domain, e.WorkspaceID, e.ApplicationID).Error; err != nil {
			return authorization.ErrDenied
		}
		id, err := security.NewID("prb")
		if err != nil {
			return err
		}
		if err = tx.Create(&probeRecord{ID: id, WorkspaceID: e.WorkspaceID, ApplicationID: e.ApplicationID, OperationID: e.OperationID, Generation: e.Generation, Domain: e.Domain, Path: e.Path, PodUID: e.PodUID, TLSVerified: e.TLSVerified, Healthy: e.Healthy, ObservedAt: e.ObservedAt}).Error; err != nil {
			return err
		}
		if !e.Healthy || !e.TLSVerified {
			if monitoring {
				state.ObservedState = "UNHEALTHY"
				return tx.Save(&state).Error
			}
			return nil
		}
		// 每个登记入口都需要对应同一 Pod、此次 Kubernetes 观察之后的新鲜 TLS/健康证据。
		for _, domain := range release.Domains {
			var proof probeRecord
			err = tx.Where("operation_id = ? AND generation = ? AND domain = ? AND path = ? AND pod_uid = ? AND observed_at >= ? AND observed_at >= ?", op.ID, op.Generation, domain, e.Path, e.PodUID, *op.LastObservedAt, time.Now().Add(-time.Minute)).Order("observed_at DESC").Take(&proof).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if !proof.Healthy || !proof.TLSVerified {
				return nil
			}
		}
		serve(&state, &op)
		if !monitoring {
			if err = finish(tx, &state, &op, Succeeded); err != nil {
				return err
			}
		}
		return tx.Save(&state).Error
	})
}

// MarkStale 只把过期观察置为 UNKNOWN，不伪造停止或成功，不删除待收敛操作。
func (s *Service) MarkStale(ctx context.Context, cutoff time.Time) error {
	if cutoff.After(time.Now().Add(-30 * time.Second)) {
		return fmt.Errorf("%w: 陈旧阈值过短", authorization.ErrInvalid)
	}
	return s.db.WithContext(ctx).Model(&stateRecord{}).Where("last_observed_at < ? AND observed_state NOT IN ('STOPPED','NOT_DEPLOYED')", cutoff).Update("observed_state", Unknown).Error
}
