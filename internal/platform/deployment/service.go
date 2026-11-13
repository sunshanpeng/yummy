package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db       *gorm.DB
	access   *authorization.Access
	configs  RuntimeConfigs
	verifier Verifier
}

func NewService(db *gorm.DB, access *authorization.Access, configs RuntimeConfigs, verifier Verifier) *Service {
	return &Service{db: db, access: access, configs: configs, verifier: verifier}
}
func invalid(message string) error { return fmt.Errorf("%w: %s", authorization.ErrInvalid, message) }
func missing(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrNotFound
	}
	return err
}
func fingerprint(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func validateChange(expected Expected, key string) error {
	if expected.Generation < 0 || expected.ConfigRevision < 0 || len(key) == 0 || len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\x00\r\n") {
		return authorization.ErrInvalid
	}
	return nil
}
func lockApplication(tx *gorm.DB, ws, app string) error {
	var value model.Application
	return missing(tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "workspace_id = ? AND id = ? AND status = ?", ws, app, model.ResourceActive).Error)
}
func readState(tx *gorm.DB, ws, app string) (stateRecord, error) {
	v := stateRecord{WorkspaceID: ws, ApplicationID: app, DesiredState: Run, ObservedState: "NOT_DEPLOYED"}
	err := tx.First(&v, "workspace_id = ? AND application_id = ?", ws, app).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return v, nil
	}
	return v, err
}
func checkExpected(tx *gorm.DB, ws, app string, state stateRecord, e Expected) error {
	var latest struct{ Revision int64 }
	err := tx.Table("runtime_config_revisions").Select("revision").Where("workspace_id = ? AND application_id = ?", ws, app).Order("revision DESC").Take(&latest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if state.Generation != e.Generation || text(state.ServingReleaseID) != e.ServingReleaseID || latest.Revision != e.ConfigRevision {
		return authorization.ErrConflict
	}
	return nil
}
func readArtifact(tx *gorm.DB, ws, app, id string) (artifactRecord, error) {
	var v artifactRecord
	err := tx.Table("build_artifacts AS a").Select("a.*").Joins("JOIN builds b ON b.id = a.build_id AND b.workspace_id = a.workspace_id AND b.application_id = a.application_id").Where("a.workspace_id = ? AND a.application_id = ? AND a.id = ? AND a.state = 'VERIFIED' AND a.verified_at IS NOT NULL AND b.state = 'SUCCEEDED'", ws, app, id).Take(&v).Error
	return v, missing(err)
}
func readRelease(tx *gorm.DB, ws, app, id string) (releaseRecord, error) {
	var v releaseRecord
	err := tx.First(&v, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, id).Error
	return v, missing(err)
}
func existing(tx *gorm.DB, ws, app, actor, key, hash string) (Result, bool, error) {
	var op operationRecord
	err := tx.First(&op, "workspace_id = ? AND application_id = ? AND actor_id = ? AND idempotency_key = ?", ws, app, actor, key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if op.Fingerprint != hash {
		return Result{}, true, authorization.ErrConflict
	}
	release, err := readRelease(tx, ws, app, op.ReleaseID)
	return Result{Release: releaseView(release), Operation: operationView(op)}, true, err
}
func (s *Service) materialize(tx *gorm.DB, ws, app string, revision int64) (runtimeconfig.Materialized, error) {
	if s.configs == nil {
		return runtimeconfig.Materialized{}, ErrNotReady
	}
	return s.configs.MaterializeForDeployment(tx, ws, app, revision)
}

// CreateDeployment 先读取已授权快照、事务外独立验证镜像，再锁应用重验全部期望条件。
func (s *Service) CreateDeployment(ctx context.Context, p authorization.Principal, ws, app string, in CreateInput) (Result, error) {
	return s.create(ctx, p, ws, app, in, "", Deploy)
}
func (s *Service) Rollback(ctx context.Context, p authorization.Principal, ws, app string, in RollbackInput) (Result, error) {
	if in.TargetReleaseID == "" {
		return Result{}, authorization.ErrInvalid
	}
	return s.create(ctx, p, ws, app, CreateInput{ExpectedPolicyRevision: in.ExpectedPolicyRevision, Expected: in.Expected, IdempotencyKey: in.IdempotencyKey}, in.TargetReleaseID, Rollback)
}
func (s *Service) create(ctx context.Context, p authorization.Principal, ws, app string, in CreateInput, target, action string) (Result, error) {
	if err := validateChange(in.Expected, in.IdempotencyKey); err != nil {
		return Result{}, err
	}
	if in.ExpectedPolicyRevision < 1 || (action == Deploy && (in.ArtifactID == "" || in.RuntimeConfigRevision < 1 || in.RuntimeConfigRevision != in.Expected.ConfigRevision)) {
		return Result{}, authorization.ErrInvalid
	}
	hash := fingerprint([]any{action, in, target})
	var result Result
	var snapshot artifactRecord
	var selected releaseRecord
	var duplicate bool
	read := func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.DeploymentWrite); err != nil {
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
		if err = checkExpected(tx, ws, app, state, in.Expected); err != nil {
			return err
		}
		if state.ActiveOperationID != nil {
			return authorization.ErrConflict
		}
		if action == Rollback {
			selected, err = readRelease(tx, ws, app, target)
			if err != nil {
				return err
			}
			var successful int64
			if err = tx.Model(&operationRecord{}).Where("workspace_id = ? AND application_id = ? AND release_id = ? AND state = ?", ws, app, target, Succeeded).Count(&successful).Error; err != nil {
				return err
			}
			if successful == 0 {
				return invalid("只能回滚到已成功的历史发布")
			}
			in.ArtifactID = selected.ArtifactID
			in.RuntimeConfigRevision = selected.RuntimeConfigRevision
		}
		snapshot, err = readArtifact(tx, ws, app, in.ArtifactID)
		if err != nil {
			return err
		}
		_, err = s.materialize(tx, ws, app, in.RuntimeConfigRevision)
		return err
	}
	if err := s.access.Do(ctx, p, false, read); err != nil {
		return Result{}, err
	}
	if duplicate {
		return result, nil
	}
	if s.verifier == nil {
		return Result{}, ErrVerification
	}
	if err := s.verifier.Verify(ctx, artifactView(snapshot)); err != nil {
		return Result{}, ErrVerification
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.DeploymentWrite); err != nil {
			return err
		}
		if err := lockApplication(tx, ws, app); err != nil {
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
		if err = checkExpected(tx, ws, app, state, in.Expected); err != nil {
			return err
		}
		if state.ActiveOperationID != nil {
			return authorization.ErrConflict
		}
		artifact, err := readArtifact(tx, ws, app, in.ArtifactID)
		if err != nil {
			return err
		}
		if artifactView(artifact) != artifactView(snapshot) {
			return authorization.ErrConflict
		}
		material, err := s.materialize(tx, ws, app, in.RuntimeConfigRevision)
		if err != nil {
			return err
		}
		placement, policy, err := s.placement(tx, ws, app, in.ExpectedPolicyRevision, material.View)
		if err != nil {
			return err
		}
		if action == Rollback && selected.PlacementSnapshot != placement {
			return invalid("历史发布运行位置与已固定位置不一致")
		}
		// WORKER 应用无域名；slices.Clone 空切片返回 nil 会被 JSONB 序列化为 SQL NULL，
		// 违反 application_releases.domains NOT NULL 约束，必须显式落为空数组。
		domains := slices.Clone(material.View.Domains)
		if domains == nil {
			domains = []string{}
		}
		if material.View.Type == runtimeconfig.TypeHTTP {
			domains = append(domains, placement.DefaultDomain)
		}
		slices.Sort(domains)
		domains = slices.Compact(domains)
		if err = claimDomains(tx, ws, app, placement.DefaultDomain, domains); err != nil {
			return err
		}
		id, err := security.NewID("rel")
		if err != nil {
			return err
		}
		release := releaseRecord{ID: id, WorkspaceID: ws, ApplicationID: app, ArtifactID: artifact.ID, RuntimeConfigRevision: in.RuntimeConfigRevision, RegistryID: artifact.RegistryID, CredentialVersion: artifact.CredentialVersion, Repository: artifact.Repository, Digest: artifact.Digest, Platform: artifact.Platform, PlacementSnapshot: placement, Domains: domains, PreviousServingID: state.ServingReleaseID, RollbackOf: optional(target), Source: action, AutoRecoveryEnabled: policy.AutoRecoveryEnabled, ActorID: current.UserID}
		if err = tx.Create(&release).Error; err != nil {
			return err
		}
		op, err := newOperation(tx, &state, release, current.UserID, action, in.IdempotencyKey, hash, nil)
		if err != nil {
			return err
		}
		if err = audit.Record(tx, current.UserID, "application.deployment."+strings.ToLower(action), "deployment_operation", op.ID, &ws, &app); err != nil {
			return err
		}
		result = Result{Release: releaseView(release), Operation: operationView(op)}
		return nil
	})
	return result, err
}

func (s *Service) placement(tx *gorm.DB, ws, app string, expectedPolicy int64, config runtimeconfig.View) (Placement, policyRecord, error) {
	var out Placement
	var settings settingsRecord
	var policy policyRecord
	fail := func(err error) (Placement, policyRecord, error) { return out, policy, err }
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&settings, "singleton = TRUE").Error; err != nil {
		return fail(missing(err))
	}
	if settings.PolicyRevision != expectedPolicy {
		return fail(authorization.ErrConflict)
	}
	if err := tx.First(&policy, "instance_id = ? AND revision = ?", settings.ID, expectedPolicy).Error; err != nil {
		return fail(err)
	}
	err := tx.First(&out, "workspace_id = ? AND application_id = ?", ws, app).Error
	fresh := errors.Is(err, gorm.ErrRecordNotFound)
	if err != nil && !fresh {
		return fail(err)
	}
	nodeID := out.NodeID
	if fresh {
		nodeID = policy.RuntimeNodeID
	}
	var allocation allocationRecord
	if err = tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&allocation, "workspace_id = ? AND instance_id = ?", ws, settings.ID).Error; err != nil {
		return fail(ErrNotReady)
	}
	var links int64
	if err = tx.Table("workspace_allocation_nodes").Where("workspace_id = ? AND instance_id = ? AND node_id = ?", ws, settings.ID, nodeID).Count(&links).Error; err != nil {
		return fail(err)
	}
	if links != 1 {
		return fail(ErrNotReady)
	}
	var node nodeRecord
	if err = tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&node, "id = ? AND instance_id = ?", nodeID, settings.ID).Error; err != nil {
		return fail(err)
	}
	if !runtimeNodeReady(node, config.Type == runtimeconfig.TypeHTTP) {
		return fail(ErrNotReady)
	}
	if config.Resources.CPULimitMilli > min(policy.ResourceLimits.CPUMillis, allocation.CPUMillis) || config.Resources.MemoryLimitMiB > min(policy.ResourceLimits.MemoryMiB, allocation.MemoryMiB) {
		return fail(invalid("运行资源超过实例或空间配额"))
	}
	if fresh {
		out = Placement{WorkspaceID: ws, ApplicationID: app, InstanceID: settings.ID, NodeID: node.ID, WorkerID: node.WorkerID, ClusterNodeUID: node.ClusterNodeUID, PolicyRevision: policy.Revision, RuntimeType: config.Type}
		if config.Type == runtimeconfig.TypeHTTP {
			domain, err := runtimeconfig.CanonicalDomain(strings.ReplaceAll(app, "_", "-") + "." + policy.BaseDomain)
			if err != nil {
				return fail(ErrNotReady)
			}
			out.DefaultDomain = domain
		}
		if err = tx.Create(&out).Error; err != nil {
			return fail(err)
		}
	} else if out.InstanceID != settings.ID || out.WorkerID != node.WorkerID || out.ClusterNodeUID != node.ClusterNodeUID || out.RuntimeType != config.Type {
		return fail(invalid("运行位置和类型已固定，不能隐式迁移"))
	}
	return out, policy, nil
}
func runtimeNodeReady(v nodeRecord, http bool) bool {
	now := time.Now()
	return v.Status == "ACTIVE" && v.OS == "linux" && v.Arch == "amd64" && v.ClusterNodeUID != "" && v.RuntimeReady && v.MetricsReady && (!http || v.IngressReady) && v.LastSeen != nil && now.Sub(*v.LastSeen) <= 90*time.Second && !v.LastSeen.After(now.Add(30*time.Second))
}
func claimDomains(tx *gorm.DB, ws, app, defaultDomain string, domains []string) error {
	for _, domain := range domains {
		canonical, err := runtimeconfig.CanonicalDomain(domain)
		if err != nil || canonical != domain {
			return invalid("发布域名不是规范DNS名称")
		}
		v := domainRecord{Domain: domain, WorkspaceID: ws, ApplicationID: app, IsDefault: domain == defaultDomain}
		if err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&v).Error; err != nil {
			return err
		}
		if err = tx.First(&v, "domain = ?", domain).Error; err != nil {
			return err
		}
		if v.WorkspaceID != ws || v.ApplicationID != app {
			return fmt.Errorf("%w: 域名已被其他应用占用", authorization.ErrConflict)
		}
	}
	return nil
}
func newOperation(tx *gorm.DB, state *stateRecord, release releaseRecord, actor, action, key, hash string, recoveryOf *string) (operationRecord, error) {
	id, err := security.NewID("dop")
	if err != nil {
		return operationRecord{}, err
	}
	// next_dispatch_at 用数据库时钟写入：领取侧以 NOW() 比较，混合宿主机时钟会在
	// 时钟偏差（如 Docker Desktop 虚拟机漂移）下让到期任务永远不可领取。
	op := operationRecord{ID: id, WorkspaceID: release.WorkspaceID, ApplicationID: release.ApplicationID, ReleaseID: release.ID, Generation: state.Generation + 1, Action: action, State: Queued, Active: true, ActorID: actor, IdempotencyKey: key, Fingerprint: hash, RecoveryOf: recoveryOf}
	if err = tx.Create(&op).Error; err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return op, authorization.ErrConflict
		}
		return op, err
	}
	if err = tx.Model(&op).Updates(map[string]any{"next_dispatch_at": gorm.Expr("NOW()"), "next_observation_at": gorm.Expr("NOW()")}).Error; err != nil {
		return op, err
	}
	state.Generation = op.Generation
	state.DesiredReleaseID = &release.ID
	state.ActiveOperationID = &op.ID
	state.DesiredState = Run
	if action == Stop {
		state.DesiredState = "STOP"
		state.ObservedState = "STOPPING"
	} else if state.ServingReleaseID == nil {
		state.ObservedState = "PENDING"
	}
	return op, tx.Save(state).Error
}

var safeCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
