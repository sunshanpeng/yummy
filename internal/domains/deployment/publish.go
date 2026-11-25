package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
)

const (
	// autoDeployOverfetch 是候选超取倍数：按创建时间多取数倍候选后在部署域
	// 过滤「已发布 + 退避中」，避免最老的暂态失败候选占满处理名额、饿死后续
	// 有效构建（永久失效候选已被关停，不再进入扫描）。
	autoDeployOverfetch = 4
	// publishRetryBackoff 是仓库暂不可用的内存退避窗口：失败候选在窗口内跳过
	// 扫描处理，不重复访问仓库；重启即清零重新重试，幂等无害。
	publishRetryBackoff = time.Minute
)

// AutoPublish 扫描 auto_deploy=TRUE、state='SUCCEEDED' 且产物已 VERIFIED、尚未产生发布的构建并受理发布。
// 判重采用双重手段：扫描后过滤已有关联 release 的 artifact（任何路径发布过该产物即算已发布），
// 受理时再以 idempotency_key='autodeploy:'+build_id 预检并由 create 的幂等约束兜底，
// 因此并发或重试最多产生一个 release+operation。
// 单个构建不满足前置（serving 期望不匹配、配置已前移、凭据不可用等）时审计原因后跳过，
// 返回错误不会中断整轮扫描；只有无法归类的底层错误才向上透出。
// 永久失效的候选（配置版本被取代、构建者身份失效、产物/凭据永久不可用、数据异常）
// 在首次跳过时同时关停 auto_deploy 移出重试队列；仓库暂不可用等网络暂态按退避窗口重试。
func (s *Service) AutoPublish(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, authorization.ErrInvalid
	}
	candidates, err := s.autoPublishCandidates(ctx, limit)
	if err != nil {
		return 0, err
	}
	published := 0
	var failures []error
	for _, candidate := range candidates {
		done, err := s.autoPublishBuild(ctx, candidate)
		if err != nil {
			failures = append(failures, fmt.Errorf("build %s: %w", candidate.BuildID, err))
			continue
		}
		if done {
			// 成功发布后才清除退避；跳过（含刚写入的暂态退避）必须保留窗口。
			s.clearPublishBackoff(candidate.BuildID)
			published++
		}
	}
	return published, errors.Join(failures...)
}

// autoPublishCandidates 取部署域视角下的待处理候选：构建域提供「已验证」的原始
// 候选（超取），部署域按自身发布事实过滤已发布产物，再按内存退避过滤暂态失败
// 的候选，最后按 limit 截断。
func (s *Service) autoPublishCandidates(ctx context.Context, limit int) ([]build.AutoDeployCandidate, error) {
	fetched, err := s.builds.AutoDeployCandidates(ctx, limit*autoDeployOverfetch)
	if err != nil {
		return nil, err
	}
	if len(fetched) == 0 {
		return nil, nil
	}
	artifacts := make([]string, 0, len(fetched))
	for _, candidate := range fetched {
		artifacts = append(artifacts, candidate.Artifact.ID)
	}
	var released []struct {
		ArtifactID string `gorm:"column:artifact_id"`
	}
	if err := s.db.WithContext(ctx).Table("application_releases").Select("DISTINCT artifact_id").Where("artifact_id IN ?", artifacts).Scan(&released).Error; err != nil {
		return nil, err
	}
	publishedArtifacts := make(map[string]bool, len(released))
	for _, row := range released {
		publishedArtifacts[row.ArtifactID] = true
	}
	out := make([]build.AutoDeployCandidate, 0, limit)
	for _, candidate := range fetched {
		if publishedArtifacts[candidate.Artifact.ID] || s.publishDeferred(candidate.BuildID) {
			continue
		}
		out = append(out, candidate)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// autoPublishBuild 受理单个候选。返回 (是否新发布, 错误)；跳过情形返回 (false, nil) 并已审计。
func (s *Service) autoPublishBuild(ctx context.Context, candidate build.AutoDeployCandidate) (bool, error) {
	// CHECK 约束保证 auto_deploy 构建必有冻结的运行配置版本；异常零值视为数据异常直接跳过。
	if candidate.RuntimeConfigRevision < 1 {
		return false, s.auditAutoDeploySkip(ctx, candidate, "missing_runtime_config")
	}
	key := "autodeploy:" + candidate.BuildID
	// 幂等预检：该构建已受理过自动发布操作时本轮直接跳过，不重复计数。
	var done int64
	if err := s.db.WithContext(ctx).Model(&operationRecord{}).Where("workspace_id = ? AND application_id = ? AND actor_id = ? AND idempotency_key = ?", candidate.WorkspaceID, candidate.ApplicationID, candidate.ActorID, key).Count(&done).Error; err != nil {
		return false, err
	}
	if done > 0 {
		return false, nil
	}
	// 以下预读只用于构造期望条件和具体跳过原因；create 会在自己的事务内重验全部条件。
	state, err := readState(s.db.WithContext(ctx), candidate.WorkspaceID, candidate.ApplicationID)
	if err != nil {
		return false, err
	}
	if candidate.ExpectedRuntimeGeneration != nil && state.Generation != *candidate.ExpectedRuntimeGeneration {
		return false, s.auditAutoDeploySkip(ctx, candidate, "stale_generation")
	}
	// 构建提交时锁定的 serving 期望与当前不一致说明期间发生过其他发布/回滚，跳过而不是误回滚。
	if text(state.ServingReleaseID) != candidate.ExpectedServingRelease {
		return false, s.auditAutoDeploySkip(ctx, candidate, "serving_mismatch")
	}
	revision, _, err := s.configs.LatestRevision(s.db.WithContext(ctx), candidate.WorkspaceID, candidate.ApplicationID)
	if err != nil {
		return false, err
	}
	// 受理路径只允许部署最新已确认配置；构建冻结的版本已被新保存的配置取代时跳过。
	if revision != candidate.RuntimeConfigRevision {
		return false, s.auditAutoDeploySkip(ctx, candidate, "stale_config")
	}
	var settings settingsRecord
	if err := s.db.WithContext(ctx).First(&settings, "singleton = TRUE").Error; err != nil {
		return false, err
	}
	// actor 使用构建提交者身份（含构建时冻结的 AuthVersion）；权限、状态机与凭据校验全部复用 create 路径。
	principal := authorization.Principal{UserID: candidate.ActorID, AuthVersion: candidate.ActorAuthVersion}
	input := CreateInput{ArtifactID: candidate.Artifact.ID, RuntimeConfigRevision: candidate.RuntimeConfigRevision, ExpectedPolicyRevision: settings.PolicyRevision, Expected: Expected{Generation: state.Generation, ServingReleaseID: text(state.ServingReleaseID), ConfigRevision: candidate.RuntimeConfigRevision}, IdempotencyKey: key}
	if _, err = s.create(ctx, principal, candidate.WorkspaceID, candidate.ApplicationID, input, "", Deploy); err != nil {
		reason, expected := autoDeploySkipReason(err)
		if !expected {
			return false, err
		}
		if reason == "verification" {
			// 仓库暂不可用属网络暂态：安排内存退避，不在每个扫描周期重复访问仓库；
			// 窗口过后候选重新参与扫描。
			s.deferPublish(candidate.BuildID)
		}
		return false, s.auditAutoDeploySkip(ctx, candidate, reason)
	}
	return true, nil
}

// autoDeploySkipReason 把受理失败归类为审计后缀；第二返回值为 false 表示不该静默跳过的未知错误。
func autoDeploySkipReason(err error) (string, bool) {
	switch {
	case errors.Is(err, authorization.ErrConflict):
		return "conflict", true
	case errors.Is(err, ErrNotReady):
		return "not_ready", true
	case errors.Is(err, ErrArtifactInvalid):
		return "artifact_invalid", true
	case errors.Is(err, ErrVerification):
		return "verification", true
	case errors.Is(err, authorization.ErrInvalid):
		return "invalid", true
	case errors.Is(err, authorization.ErrUnauthenticated), errors.Is(err, authorization.ErrDenied):
		return "actor_forbidden", true
	case errors.Is(err, authorization.ErrNotFound):
		return "missing", true
	}
	return "", false
}

// permanentSkipReasons 是构建本身已永久失效的跳过原因：条件不可逆转（冻结配置
// 版本被新版本取代后永远不再最新、构建者身份/产物/数据事实失效），候选每轮都会
// 重现；首次跳过时关停构建的 auto_deploy 移出重试队列。serving_mismatch 例外：
// 期望的 serving 可经回滚/再发布恢复一致，保持候选等待条件解除。
var permanentSkipReasons = map[string]bool{
	"missing_runtime_config": true,
	"stale_config":           true,
	"stale_generation":       true,
	"artifact_invalid":       true,
	"actor_forbidden":        true,
	"missing":                true,
	"invalid":                true,
}

// auditAutoDeploySkip 记录跳过原因。跳过条件每轮都会重现，同一构建同一原因只记录一次，
// 避免后台循环以发布间隔刷屏审计；瞬时原因（conflict/not_ready 等）条件解除后仍会正常发布。
// 永久失效的候选在同一事务内关停 auto_deploy：关停以审计已记为前提，保证审计与
// 移出队列要么同轮发生、要么都不发生。
func (s *Service) auditAutoDeploySkip(ctx context.Context, candidate build.AutoDeployCandidate, reason string) error {
	action := "application.deployment.autodeploy.skip." + reason
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var seen int64
		if err := tx.Model(&model.AuditEvent{}).Where("actor_id = ? AND action = ? AND object_id = ?", candidate.ActorID, action, candidate.BuildID).Count(&seen).Error; err != nil {
			return err
		}
		if seen > 0 {
			return nil
		}
		if err := audit.Record(tx, candidate.ActorID, action, "build", candidate.BuildID, &candidate.WorkspaceID, &candidate.ApplicationID); err != nil {
			return err
		}
		if permanentSkipReasons[reason] {
			// builds 的 CHECK 约束把 auto_deploy 与 runtime_config_revision 同现
			// 绑定（auto_deploy = (runtime_config_revision IS NOT NULL)）：关停
			// 标记必须同时释放冻结版本指针，版本事实本身仍在 runtime_config_revisions。
			if err := tx.Table("builds").Where("id = ? AND auto_deploy = TRUE", candidate.BuildID).Updates(map[string]any{"auto_deploy": false, "runtime_config_revision": nil}).Error; err != nil {
				return err
			}
			// 候选已永久离开扫描队列，退避条目随之回收；瞬时跳过保留退避窗口。
			s.clearPublishBackoff(candidate.BuildID)
		}
		return nil
	})
}

// publishDeferred 报告候选是否处于暂态退避窗口内；窗口由 deferPublish 写入。
func (s *Service) publishDeferred(buildID string) bool {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	next, ok := s.publishBackoff[buildID]
	return ok && time.Now().Before(next)
}

func (s *Service) deferPublish(buildID string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if s.publishBackoff == nil {
		s.publishBackoff = map[string]time.Time{}
	}
	s.publishBackoff[buildID] = time.Now().Add(publishRetryBackoff)
}

func (s *Service) clearPublishBackoff(buildID string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	delete(s.publishBackoff, buildID)
}
