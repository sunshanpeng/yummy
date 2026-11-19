package inventory

import (
	"context"
	"errors"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func normalizeDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if value == "" {
		return "", nil
	}
	if len(value) > 253 || net.ParseIP(value) != nil || !strings.Contains(value, ".") {
		return "", invalid("根域名需要合法DNS名称")
	}
	for _, label := range strings.Split(value, ".") {
		if !domainLabel.MatchString(label) {
			return "", invalid("根域名需要ASCII DNS名称，国际域名请先转punycode")
		}
	}
	return value, nil
}
func validResources(defaults, limits Resources) bool {
	return defaults.CPUMillis > 0 && defaults.MemoryMiB > 0 && limits.CPUMillis >= defaults.CPUMillis && limits.MemoryMiB >= defaults.MemoryMiB && limits.CPUMillis <= 1000000 && limits.MemoryMiB <= 1048576
}
func policyView(value policyRecord) PolicyView {
	return PolicyView{InstanceID: value.InstanceID, Revision: value.Revision, BuildNodeID: value.BuildNodeID, RuntimeNodeID: value.RuntimeNodeID, RegistryID: value.RegistryID, RepositoryPrefix: value.RepositoryPrefix, BaseDomain: value.BaseDomain, ResourceDefault: value.ResourceDefault, ResourceLimits: value.ResourceLimits, AutoRecoveryEnabled: value.AutoRecoveryEnabled, CreatedAt: value.CreatedAt}
}
func (s *Service) SavePolicy(ctx context.Context, p authorization.Principal, input PolicyInput) (PolicyView, error) {
	if input.ExpectedRevision < 0 || input.BuildNodeID == "" || input.RuntimeNodeID == "" || input.RegistryID == "" || !validResources(input.ResourceDefault, input.ResourceLimits) {
		return PolicyView{}, authorization.ErrInvalid
	}
	if err := validatePrefix(input.RepositoryPrefix); err != nil {
		return PolicyView{}, err
	}
	domain, err := normalizeDomain(input.BaseDomain)
	if err != nil {
		return PolicyView{}, err
	}
	var result PolicyView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		settings, err := instance(tx)
		if err != nil {
			return err
		}
		if settings.PolicyRevision != input.ExpectedRevision {
			return authorization.ErrConflict
		}
		for _, id := range []string{input.BuildNodeID, input.RuntimeNodeID} {
			var node nodeRecord
			if err := tx.First(&node, "id = ? AND instance_id = ?", id, settings.ID).Error; err != nil {
				return missing(err)
			}
		}
		var registry registryRecord
		if err := tx.First(&registry, "id = ? AND instance_id = ?", input.RegistryID, settings.ID).Error; err != nil {
			return missing(err)
		}
		value := policyRecord{InstanceID: settings.ID, Revision: settings.PolicyRevision + 1, BuildNodeID: input.BuildNodeID, RuntimeNodeID: input.RuntimeNodeID, RegistryID: input.RegistryID, RepositoryPrefix: input.RepositoryPrefix, BaseDomain: domain, ResourceDefault: input.ResourceDefault, ResourceLimits: input.ResourceLimits, AutoRecoveryEnabled: input.AutoRecoveryEnabled}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		updated := tx.Model(&instanceRecord{}).Where("singleton = TRUE AND policy_revision = ?", input.ExpectedRevision).Update("policy_revision", value.Revision)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "instance.policy.save", "instance", settings.ID, nil, nil); err != nil {
			return err
		}
		result = policyView(value)
		return nil
	})
	return result, err
}
func (s *Service) GetPolicy(ctx context.Context, p authorization.Principal, revision int64) (PolicyView, error) {
	var result PolicyView
	if revision < 0 {
		return result, authorization.ErrInvalid
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var settings instanceRecord
		if err := tx.First(&settings, "singleton = TRUE").Error; err != nil {
			return missing(err)
		}
		if revision == 0 {
			revision = settings.PolicyRevision
		}
		var value policyRecord
		if err := tx.First(&value, "instance_id = ? AND revision = ?", settings.ID, revision).Error; err != nil {
			return missing(err)
		}
		result = policyView(value)
		return nil
	})
	return result, err
}

func allocationView(value allocationRecord, nodes []allocationNodeRecord) AllocationView {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.NodeID)
	}
	sort.Strings(ids)
	return AllocationView{WorkspaceID: value.WorkspaceID, AllowedNodeIDs: ids, MaxApplications: value.MaxApplications, BuildConcurrency: value.BuildConcurrency, CPUMillis: value.CPUMillis, MemoryMiB: value.MemoryMiB, Revision: value.Revision}
}
func (s *Service) SetAllocation(ctx context.Context, p authorization.Principal, workspaceID string, input AllocationInput) (AllocationView, error) {
	if input.ExpectedRevision < 0 || len(input.AllowedNodeIDs) < 1 || len(input.AllowedNodeIDs) > 100 || input.MaxApplications < 1 || input.BuildConcurrency < 1 || input.CPUMillis < 1 || input.MemoryMiB < 1 {
		return AllocationView{}, authorization.ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range input.AllowedNodeIDs {
		if id == "" || seen[id] {
			return AllocationView{}, authorization.ErrInvalid
		}
		seen[id] = true
	}
	var result AllocationView
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		settings, err := instance(tx)
		if err != nil {
			return err
		}
		var space model.Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&space, "id = ? AND status = ?", workspaceID, model.ResourceActive).Error; err != nil {
			return missing(err)
		}
		for _, id := range input.AllowedNodeIDs {
			var node nodeRecord
			if err := tx.First(&node, "id = ? AND instance_id = ?", id, settings.ID).Error; err != nil {
				return missing(err)
			}
		}
		var value allocationRecord
		err = tx.First(&value, "workspace_id = ?", workspaceID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.ExpectedRevision != 0 {
				return authorization.ErrConflict
			}
			value = allocationRecord{WorkspaceID: workspaceID, InstanceID: settings.ID, MaxApplications: input.MaxApplications, BuildConcurrency: input.BuildConcurrency, CPUMillis: input.CPUMillis, MemoryMiB: input.MemoryMiB, Revision: 1}
			if err := tx.Create(&value).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if value.Revision != input.ExpectedRevision {
				return authorization.ErrConflict
			}
			if err := tx.Model(&allocationRecord{}).Where("workspace_id = ? AND revision = ?", workspaceID, input.ExpectedRevision).Updates(map[string]any{"max_applications": input.MaxApplications, "build_concurrency": input.BuildConcurrency, "cpu_millis": input.CPUMillis, "memory_mib": input.MemoryMiB, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
			if err := tx.First(&value, "workspace_id = ?", workspaceID).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("workspace_id = ?", workspaceID).Delete(&allocationNodeRecord{}).Error; err != nil {
			return err
		}
		nodes := make([]allocationNodeRecord, 0, len(input.AllowedNodeIDs))
		for _, id := range input.AllowedNodeIDs {
			nodes = append(nodes, allocationNodeRecord{WorkspaceID: workspaceID, InstanceID: settings.ID, NodeID: id})
		}
		if err := tx.Create(&nodes).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "instance.allocation.set", "workspace", workspaceID, &workspaceID, nil); err != nil {
			return err
		}
		result = allocationView(value, nodes)
		return nil
	})
	return result, err
}
func (s *Service) GetAllocation(ctx context.Context, p authorization.Principal, workspaceID string) (AllocationView, error) {
	var result AllocationView
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value allocationRecord
		if err := tx.First(&value, "workspace_id = ?", workspaceID).Error; err != nil {
			return missing(err)
		}
		var nodes []allocationNodeRecord
		if err := tx.Where("workspace_id = ?", workspaceID).Find(&nodes).Error; err != nil {
			return err
		}
		result = allocationView(value, nodes)
		return nil
	})
	return result, err
}
