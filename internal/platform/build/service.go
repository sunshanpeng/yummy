package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db       *gorm.DB
	access   *authorization.Access
	resolver BuildResolver
	secrets  SecretResolver
}

// SetSecretResolver 只在装配阶段调用，不得在处理请求时更换依赖。
func (s *Service) SetSecretResolver(resolver SecretResolver) { s.secrets = resolver }

func NewService(db *gorm.DB, access *authorization.Access, resolver BuildResolver) *Service {
	return &Service{db: db, access: access, resolver: resolver}
}
func hash(data []byte) string { v := sha256.Sum256(data); return hex.EncodeToString(v[:]) }
func missing(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrNotFound
	}
	return err
}
func invalid(message string) error { return fmt.Errorf("%w: %s", authorization.ErrInvalid, message) }

var nameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

func validPath(value string, dot bool) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsAny(value, "\\\x00\r\n") && !path.IsAbs(value) && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../") && (dot || value != ".")
}
func validateConfig(c Config) (Config, error) {
	if c.Dockerfile == "" {
		c.Dockerfile = "Dockerfile"
	}
	if c.Context == "" {
		c.Context = "."
	}
	if !validPath(c.Dockerfile, false) || !validPath(c.Context, true) || (c.Target != "" && !nameRE.MatchString(c.Target)) {
		return c, invalid("构建路径或目标非法")
	}
	if len(c.Args) > 64 {
		return c, invalid("构建参数过多")
	}
	for k, v := range c.Args {
		if !nameRE.MatchString(k) || len(v) > 4096 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return c, invalid("非敏感构建参数非法")
		}
	}
	if len(c.SecretRefs) > 32 {
		return c, invalid("构建密钥过多")
	}
	for _, ref := range c.SecretRefs {
		if ref.SecretID == "" || len(ref.SecretID)>128 || !utf8.ValidString(ref.SecretID) || strings.ContainsRune(ref.SecretID,0) || ref.Version < 0 || (ref.Name!=""&&!nameRE.MatchString(ref.Name)) {
			return c, invalid("构建密钥引用非法")
		}
	}
	return c, nil
}
func applyOverrides(c Config, o Overrides) (Config, error) {
	if o.Dockerfile != nil {
		c.Dockerfile = *o.Dockerfile
	}
	if o.Context != nil {
		c.Context = *o.Context
	}
	if o.Target != nil {
		c.Target = *o.Target
	}
	if o.Args != nil {
		c.Args = o.Args
	}
	if o.SecretRefs != nil {
		c.SecretRefs = o.SecretRefs
	}
	return validateConfig(c)
}
func configAt(tx *gorm.DB, ws, app string, expected int64) (Config, error) {
	var record configRecord
	err := tx.Where("workspace_id = ? AND application_id = ?", ws, app).Order("revision DESC").Take(&record).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return Config{}, err
	}
	if record.Revision != expected {
		return Config{}, authorization.ErrConflict
	}
	if record.Revision == 0 {
		return Config{Dockerfile: "Dockerfile", Context: "."}, nil
	}
	return record.Spec, nil
}

// SaveConfig 使用应用行锁做 revision CAS；不允许把平台目的地作为业务配置保存。
func (s *Service) SaveConfig(ctx context.Context, p authorization.Principal, ws, app string, expected int64, c Config) (int64, error) {
	c, err := validateConfig(c)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.ConfigWrite); err != nil {
			return err
		}
		var application model.Application
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&application, "workspace_id = ? AND id = ?", ws, app).Error; err != nil {
			return err
		}
		if _, err := configAt(tx, ws, app, expected); err != nil {
			return err
		}
		c.SecretRefs, err = s.resolveSecrets(tx, ws, app, c.SecretRefs)
		if err != nil {
			return err
		}
		revision = expected + 1
		if err := tx.Create(&configRecord{WorkspaceID: ws, ApplicationID: app, Revision: revision, Spec: c, CreatedBy: current.UserID}).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "build.config.save", "application", app, &ws, &app)
	})
	return revision, err
}
func (s *Service) authorizeInput(tx *gorm.DB, p authorization.Principal, ws, app string, in CreateInput) error {
	actions := []authorization.Action{authorization.BuildWrite, authorization.SourceRead}
	if in.AutoDeploy {
		actions = append(actions, authorization.DeploymentWrite, authorization.ConfigRead)
	}
	for _, action := range actions {
		if _, _, err := s.access.Application(tx, p, ws, app, action); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) prepare(tx *gorm.DB, ws, app string, in CreateInput) (Preview, settingsRecord, policyRecord, registryRecord, error) {
	var out Preview
	var settings settingsRecord
	var policy policyRecord
	var registry registryRecord
	fail := func(err error) (Preview, settingsRecord, policyRecord, registryRecord, error) {
		return out, settings, policy, registry, err
	}
	if in.ExpectedPolicyRevision < 1 || in.ConfigRevision < 0 || (!in.AutoDeploy && (in.RuntimeConfigRevision != 0 || in.ExpectedServingRelease!="")) || len(in.SourceSnapshotID)>128 || strings.ContainsRune(in.SourceSnapshotID,0) || len(in.ExpectedServingRelease)>128 || strings.ContainsRune(in.ExpectedServingRelease,0) || !utf8.ValidString(in.SourceSnapshotID) || !utf8.ValidString(in.ExpectedServingRelease) {
		return fail(authorization.ErrInvalid)
	}
	config, err := configAt(tx, ws, app, in.ConfigRevision)
	if err != nil {
		return fail(err)
	}
	config, err = applyOverrides(config, in.Overrides)
	if err != nil {
		return fail(err)
	}
	config.SecretRefs, err = s.resolveSecrets(tx, ws, app, config.SecretRefs)
	if err != nil {
		return fail(err)
	}
	var src sourceRecord
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&src, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, in.SourceSnapshotID).Error; err != nil {
		return fail(missing(err))
	}
	if src.State != "READY" {
		return fail(ErrSourceInvalid)
	}
	if !src.ExpiresAt.After(time.Now()) {
		return fail(ErrSourceExpired)
	}
	found := false
	for _, entry := range src.Entries {
		if entry.Path == config.Dockerfile && !entry.Directory {
			found = true
		}
	}
	if !found || (config.Context != "." && !strings.HasPrefix(config.Dockerfile, config.Context+"/")) {
		return fail(ErrSourceInvalid)
	}
	out = Preview{SourceSnapshotID: src.ID, SourceSHA256: src.SHA256, ConfigRevision: in.ConfigRevision, Config: config, AutoDeploy: in.AutoDeploy}
	if in.AutoDeploy {
		if in.RuntimeConfigRevision < 1 {
			return fail(invalid("自动部署必须绑定已保存且确认的运行配置"))
		}
		var cfg struct {
			Revision  int64
			Confirmed bool
		}
		if err := tx.Table("runtime_config_revisions").Where("workspace_id = ? AND application_id = ?", ws, app).Order("revision DESC").Take(&cfg).Error; err != nil {
			return fail(missing(err))
		}
		if cfg.Revision != in.RuntimeConfigRevision {
			return fail(authorization.ErrConflict)
		}
		if !cfg.Confirmed {
			return fail(invalid("运行配置尚未确认"))
		}
		v := cfg.Revision
		out.RuntimeConfigRevision = &v
	}
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("singleton = TRUE").Take(&settings).Error; err != nil {
		return fail(missing(err))
	}
	if settings.PolicyRevision != in.ExpectedPolicyRevision {
		return fail(authorization.ErrConflict)
	}
	if err := tx.Where("instance_id = ? AND revision = ?", settings.ID, settings.PolicyRevision).Take(&policy).Error; err != nil {
		return fail(err)
	}
	var allocation allocationRecord
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("workspace_id = ? AND instance_id = ?", ws, settings.ID).Take(&allocation).Error; err != nil {
		return fail(fmt.Errorf("%w: 分配不存在: %v", inventory.ErrNotReady, err))
	}
	var count int64
	if err := tx.Table("workspace_allocation_nodes").Where("workspace_id = ? AND instance_id = ? AND node_id = ?", ws, settings.ID, policy.BuildNodeID).Count(&count).Error; err != nil {
		return fail(err)
	}
	if count != 1 {
		return fail(fmt.Errorf("%w: 策略节点不在工作空间分配范围", inventory.ErrNotReady))
	}
	var node nodeRecord
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&node, "id = ? AND instance_id = ?", policy.BuildNodeID, settings.ID).Error; err != nil {
		return fail(err)
	}
	if !nodeReady(node, time.Now()) {
		return fail(fmt.Errorf("%w: 构建节点资格未通过 (%s/%s, %s, buildReady=%t)", inventory.ErrNotReady, node.OS, node.Arch, node.Status, node.BuildReady))
	}
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&registry, "id = ? AND instance_id = ?", policy.RegistryID, settings.ID).Error; err != nil {
		return fail(err)
	}
	if registry.Status != "ACTIVE" || registry.VerifiedAt == nil || registry.VerifiedRevision == nil || *registry.VerifiedRevision != registry.Revision {
		return fail(fmt.Errorf("%w: 输出仓库当前版本尚未验证", inventory.ErrNotReady))
	}
	if policy.ResourceDefault.CPUMillis > allocation.CPUMillis || policy.ResourceDefault.MemoryMiB > allocation.MemoryMiB {
		return fail(fmt.Errorf("%w: 默认资源超过工作空间分配额度", inventory.ErrNotReady))
	}
	if policy.ResourceLimits.CPUMillis > allocation.CPUMillis {
		policy.ResourceLimits.CPUMillis = allocation.CPUMillis
	}
	if policy.ResourceLimits.MemoryMiB > allocation.MemoryMiB {
		policy.ResourceLimits.MemoryMiB = allocation.MemoryMiB
	}
	prefix := strings.Trim(strings.Join([]string{registry.RepositoryPrefix, policy.RepositoryPrefix}, "/"), "/")
	repo := strings.TrimPrefix(registry.URL, "https://") + "/"
	if prefix != "" {
		repo += prefix + "/"
	}
	repo += app
	out.Execution = inventory.ExecutionSummary{PolicyRevision: policy.Revision, Platform: Platform, NodeID: node.ID, NodeName: node.Name, ImageRepository: repo, ResourceDefault: policy.ResourceDefault, ResourceLimits: policy.ResourceLimits, Ready: true}
	return out, settings, policy, registry, nil
}
func nodeReady(node nodeRecord, now time.Time) bool {
	return node.Status == "ACTIVE" && node.BuildReady && node.OS == "linux" && node.Arch == "amd64" && node.ClusterNodeUID != "" && node.LastSeen != nil && now.Sub(*node.LastSeen) <= 90*time.Second && !node.LastSeen.After(now.Add(30*time.Second))
}
func (s *Service) CreatePreview(ctx context.Context, p authorization.Principal, ws, app string, in CreateInput) (Preview, error) {
	var out Preview
	// 复用库存的公开解析契约，随后在同一授权事务里重新验证全部冻结事实。
	if s.resolver != nil {
		if _, err := s.resolver.ResolveBuild(ctx, p, ws, app, in.ExpectedPolicyRevision); err != nil {
			return out, err
		}
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorizeInput(tx, current, ws, app, in); err != nil {
			return err
		}
		var err error
		out, _, _, _, err = s.prepare(tx, ws, app, in)
		return err
	})
	return out, err
}
func (s *Service) CreateBuild(ctx context.Context, p authorization.Principal, ws, app string, in CreateInput) (View, error) {
	return s.create(ctx, p, ws, app, in, nil)
}
func (s *Service) create(ctx context.Context, p authorization.Principal, ws, app string, in CreateInput, retry *string) (View, error) {
	var out View
	if len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > 200 || !utf8.ValidString(in.IdempotencyKey) || strings.ContainsRune(in.IdempotencyKey,0) {
		return out, authorization.ErrInvalid
	}
	raw, _ := json.Marshal(struct {
		App   string
		Input CreateInput
		Retry *string
	}{app, in, retry})
	fingerprint := hash(raw)
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorizeInput(tx, current, ws, app, in); err != nil {
			return err
		}
		// 同一 actor/key 的跨应用请求也必须串行，不能仅锁应用。
		key, _ := json.Marshal([]string{ws, current.UserID, in.IdempotencyKey})
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", string(key)).Error; err != nil {
			return err
		}
		var previous buildRecord
		err := tx.Where("workspace_id = ? AND actor_id = ? AND idempotency_key = ?", ws, current.UserID, in.IdempotencyKey).Take(&previous).Error
		if err == nil {
			if previous.Fingerprint != fingerprint {
				return authorization.ErrConflict
			}
			var err error
			out, err = s.view(tx, previous)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var application model.Application
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&application, "workspace_id = ? AND id = ?", ws, app).Error; err != nil {
			return err
		}
		preview, settings, policy, registry, err := s.prepare(tx, ws, app, in)
		if err != nil {
			return err
		}
		id, err := security.NewID("bld")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		record := buildRecord{ID: id, WorkspaceID: ws, ApplicationID: app, ActorID: current.UserID, ActorAuthVersion: current.AuthVersion, IdempotencyKey: in.IdempotencyKey, Fingerprint: fingerprint, SourceSnapshotID: preview.SourceSnapshotID, SourceSHA256: preview.SourceSHA256, ConfigRevision: in.ConfigRevision, ConfigSnapshot: preview.Config, InstanceID: settings.ID, PolicyRevision: policy.Revision, NodeID: policy.BuildNodeID, RegistryID: registry.ID, CredentialVersion: registry.CredentialVersion, Repository: preview.Execution.ImageRepository, Tag: id, Platform: Platform, ExecutionSnapshot: preview.Execution, AutoDeploy: in.AutoDeploy, RuntimeConfigRevision: preview.RuntimeConfigRevision, ExpectedServingRelease: in.ExpectedServingRelease, RetryOf: retry, State: Queued, DesiredState: "RUN", Version: 1, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)}
		if current.TokenID != "" {
			token := current.TokenID
			record.TokenID = &token
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		for _, ref := range record.ConfigSnapshot.SecretRefs {
			if err := tx.Create(&secretReferenceRecord{WorkspaceID: ws, ApplicationID: app, BuildID: id, SecretID: ref.SecretID, Version: ref.Version, Name: ref.Name}).Error; err != nil {
				return err
			}
		}
		if err := audit.Record(tx, current.UserID, "build.create", "build", id, &ws, &app); err != nil {
			return err
		}
		out, err = s.view(tx, record)
		return err
	})
	return out, err
}
func (s *Service) view(tx *gorm.DB, b buildRecord) (View, error) {
	out := View{ID: b.ID, WorkspaceID: b.WorkspaceID, ApplicationID: b.ApplicationID, ActorID: b.ActorID, SourceSnapshotID: b.SourceSnapshotID, ConfigRevision: b.ConfigRevision, Config: b.ConfigSnapshot, PolicyRevision: b.PolicyRevision, NodeID: b.NodeID, Repository: b.Repository, Platform: b.Platform, State: b.State, Stage: b.Stage, DesiredState: b.DesiredState, ErrorCode: b.ErrorCode, QueueReason: b.QueueReason, Version: b.Version, AutoDeploy: b.AutoDeploy, RuntimeConfigRevision: b.RuntimeConfigRevision, ExpectedServingRelease: b.ExpectedServingRelease, RetryOf: b.RetryOf, LogsComplete: b.LogsComplete, LogsTruncated: b.LogsTruncated, CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt}
	var a artifactRecord
	if err := tx.Where("build_id = ?", b.ID).Take(&a).Error; err == nil {
		out.Artifact = &Artifact{ID: a.ID, RegistryID: a.RegistryID, CredentialVersion: a.CredentialVersion, Repository: a.Repository, Tag: a.Tag, Digest: a.Digest, Platform: a.Platform, State: a.State, VerifiedAt: a.VerifiedAt}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	return out, nil
}
