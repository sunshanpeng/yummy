package deployment

import (
	"context"
	"errors"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
)

// autoDeployBuild 是 builds/build_artifacts 的跨模块只读投影，仅用于自动发布扫描，
// 不通过它修改构建模块事实（与 readArtifact 的投影约定一致）。
type autoDeployBuild struct {
	ID, WorkspaceID, ApplicationID, ActorID string
	ActorAuthVersion                        int64
	RuntimeConfigRevision                   *int64
	ExpectedServingRelease                  string
	ArtifactID                              string
}

func (autoDeployBuild) TableName() string { return "builds" }

// AutoPublish 扫描 auto_deploy=TRUE、state='SUCCEEDED' 且产物已 VERIFIED、尚未产生发布的构建并受理发布。
// 判重采用双重手段：扫描时排除已有关联 release 的 artifact（任何路径发布过该产物即算已发布），
// 受理时再以 idempotency_key='autodeploy:'+build_id 预检并由 create 的幂等约束兜底，
// 因此并发或重试最多产生一个 release+operation。
// 单个构建不满足前置（serving 期望不匹配、配置已前移、凭据不可用等）时审计原因后跳过，
// 返回错误不会中断整轮扫描；只有无法归类的底层错误才向上透出。
// 永久失效的候选（配置版本被取代、构建者身份失效、数据异常）在首次跳过时同时关停
// auto_deploy 移出重试队列，避免按创建时间取批次的扫描被最老的死候选占满、饿死后续有效构建。
func (s *Service) AutoPublish(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, authorization.ErrInvalid
	}
	candidates, err := s.autoDeployCandidates(ctx, limit)
	if err != nil {
		return 0, err
	}
	published := 0
	var failures []error
	for _, candidate := range candidates {
		done, err := s.autoPublishBuild(ctx, candidate)
		if err != nil {
			failures = append(failures, fmt.Errorf("build %s: %w", candidate.ID, err))
			continue
		}
		if done {
			published++
		}
	}
	return published, errors.Join(failures...)
}

// autoDeployCandidates 只选出“构建成功、产物验证通过且从未发布”的自动发布候选，按创建时间排序。
func (s *Service) autoDeployCandidates(ctx context.Context, limit int) ([]autoDeployBuild, error) {
	var out []autoDeployBuild
	err := s.db.WithContext(ctx).Table("builds AS b").
		Select("b.id, b.workspace_id, b.application_id, b.actor_id, b.actor_auth_version, b.runtime_config_revision, b.expected_serving_release, a.id AS artifact_id").
		Joins("JOIN build_artifacts a ON a.build_id = b.id AND a.workspace_id = b.workspace_id AND a.application_id = b.application_id").
		Where("b.auto_deploy = TRUE AND b.state = ? AND a.state = 'VERIFIED' AND a.verified_at IS NOT NULL", Succeeded).
		Where("NOT EXISTS (SELECT 1 FROM application_releases r WHERE r.workspace_id = b.workspace_id AND r.application_id = b.application_id AND r.artifact_id = a.id)").
		Order("b.created_at, b.id").Limit(limit).Scan(&out).Error
	return out, err
}

// autoPublishBuild 受理单个候选。返回 (是否新发布, 错误)；跳过情形返回 (false, nil) 并已审计。
func (s *Service) autoPublishBuild(ctx context.Context, candidate autoDeployBuild) (bool, error) {
	// CHECK 约束保证 auto_deploy 构建必有冻结的运行配置版本；空值视为数据异常直接跳过。
	if candidate.RuntimeConfigRevision == nil {
		return false, s.auditAutoDeploySkip(ctx, candidate, "missing_runtime_config")
	}
	key := "autodeploy:" + candidate.ID
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
	// 构建提交时锁定的 serving 期望与当前不一致说明期间发生过其他发布/回滚，跳过而不是误回滚。
	if text(state.ServingReleaseID) != candidate.ExpectedServingRelease {
		return false, s.auditAutoDeploySkip(ctx, candidate, "serving_mismatch")
	}
	var latest struct{ Revision int64 }
	err = s.db.WithContext(ctx).Table("runtime_config_revisions").Select("revision").Where("workspace_id = ? AND application_id = ?", candidate.WorkspaceID, candidate.ApplicationID).Order("revision DESC").Take(&latest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	// 受理路径只允许部署最新已确认配置；构建冻结的版本已被新保存的配置取代时跳过。
	if latest.Revision != *candidate.RuntimeConfigRevision {
		return false, s.auditAutoDeploySkip(ctx, candidate, "stale_config")
	}
	var settings settingsRecord
	if err := s.db.WithContext(ctx).First(&settings, "singleton = TRUE").Error; err != nil {
		return false, err
	}
	// actor 使用构建提交者身份（含构建时冻结的 AuthVersion）；权限、状态机与凭据校验全部复用 create 路径。
	principal := authorization.Principal{UserID: candidate.ActorID, AuthVersion: candidate.ActorAuthVersion}
	input := CreateInput{ArtifactID: candidate.ArtifactID, RuntimeConfigRevision: *candidate.RuntimeConfigRevision, ExpectedPolicyRevision: settings.PolicyRevision, Expected: Expected{Generation: state.Generation, ServingReleaseID: text(state.ServingReleaseID), ConfigRevision: *candidate.RuntimeConfigRevision}, IdempotencyKey: key}
	if _, err = s.create(ctx, principal, candidate.WorkspaceID, candidate.ApplicationID, input, "", Deploy); err != nil {
		reason, expected := autoDeploySkipReason(err)
		if !expected {
			return false, err
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
// 版本被新版本取代后永远不再最新、构建者身份/数据事实失效），候选每轮都会重现；
// 首次跳过时关停构建的 auto_deploy 移出重试队列。serving_mismatch 例外：期望的
// serving 可经回滚/再发布恢复一致，保持候选等待条件解除。
var permanentSkipReasons = map[string]bool{
	"missing_runtime_config": true,
	"stale_config":           true,
	"actor_forbidden":        true,
	"missing":                true,
	"invalid":                true,
}

// auditAutoDeploySkip 记录跳过原因。跳过条件每轮都会重现，同一构建同一原因只记录一次，
// 避免后台循环以发布间隔刷屏审计；瞬时原因（conflict/not_ready 等）条件解除后仍会正常发布。
// 永久失效的候选在同一事务内关停 auto_deploy：关停以审计已记为前提，保证审计与
// 移出队列要么同轮发生、要么都不发生。
func (s *Service) auditAutoDeploySkip(ctx context.Context, candidate autoDeployBuild, reason string) error {
	action := "application.deployment.autodeploy.skip." + reason
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var seen int64
		if err := tx.Model(&model.AuditEvent{}).Where("actor_id = ? AND action = ? AND object_id = ?", candidate.ActorID, action, candidate.ID).Count(&seen).Error; err != nil {
			return err
		}
		if seen > 0 {
			return nil
		}
		if err := audit.Record(tx, candidate.ActorID, action, "build", candidate.ID, &candidate.WorkspaceID, &candidate.ApplicationID); err != nil {
			return err
		}
		if permanentSkipReasons[reason] {
			// builds 的 CHECK 约束把 auto_deploy 与 runtime_config_revision 同现
			// 绑定（auto_deploy = (runtime_config_revision IS NOT NULL)）：关停
			// 标记必须同时释放冻结版本指针，版本事实本身仍在 runtime_config_revisions。
			return tx.Table("builds").Where("id = ? AND auto_deploy = TRUE", candidate.ID).Updates(map[string]any{"auto_deploy": false, "runtime_config_revision": nil}).Error
		}
		return nil
	})
}
