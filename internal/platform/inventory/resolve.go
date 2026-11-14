package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"gorm.io/gorm"

	"github.com/sunshanpeng/yummy/internal/platform/model"
)

// ResolveBuild 仅预览已通过资格门禁的冻结摘要，不领取任务或调用外部网络。
func (s *Service) ResolveBuild(ctx context.Context, p authorization.Principal, workspaceID, applicationID string, expectedPolicyRevision int64) (ExecutionSummary, error) {
	return s.resolve(ctx, p, workspaceID, applicationID, expectedPolicyRevision, false)
}

// ResolveRuntime 不创建 Placement；部署受理必须在自身事务首次固定运行位置。
func (s *Service) ResolveRuntime(ctx context.Context, p authorization.Principal, workspaceID, applicationID string, expectedPolicyRevision int64) (ExecutionSummary, error) {
	return s.resolve(ctx, p, workspaceID, applicationID, expectedPolicyRevision, true)
}
func (s *Service) resolve(ctx context.Context, p authorization.Principal, workspaceID, applicationID string, expected int64, runtime bool) (ExecutionSummary, error) {
	var result ExecutionSummary
	if expected < 0 {
		return result, authorization.ErrInvalid
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		action := authorization.BuildRead
		if runtime {
			action = authorization.ConfigRead
		}
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, action); err != nil {
			return err
		}
		// 默认地址需要应用名与不可变地址后缀（2026-10-26 设计）。
		var appRow model.Application
		if err := tx.First(&appRow, "workspace_id = ? AND id = ?", workspaceID, applicationID).Error; err != nil {
			return err
		}
		var settings instanceRecord
		if err := tx.First(&settings, "singleton = TRUE").Error; err != nil {
			// 实例行由首次实例管理操作懒创建；全新实例在此应表达为"尚未配置执行
			// 策略"，而不是通用资源不存在让用户误以为无权限。
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return notReady(&result, "实例尚未配置执行策略")
			}
			return err
		}
		if expected == 0 {
			expected = settings.PolicyRevision
		}
		result.PolicyRevision = settings.PolicyRevision
		result.Platform = Platform
		if expected < 1 {
			return notReady(&result, "实例尚未配置执行策略")
		}
		if settings.PolicyRevision != expected {
			return authorization.ErrConflict
		}
		var policy policyRecord
		if err := tx.First(&policy, "instance_id = ? AND revision = ?", settings.ID, expected).Error; err != nil {
			return missing(err)
		}
		var allocation allocationRecord
		if err := tx.First(&allocation, "workspace_id = ? AND instance_id = ?", workspaceID, settings.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: 工作空间尚未分配执行额度", ErrNotReady)
			}
			return err
		}
		nodeID := policy.BuildNodeID
		if runtime {
			nodeID = policy.RuntimeNodeID
		}
		var link allocationNodeRecord
		if err := tx.First(&link, "workspace_id = ? AND instance_id = ? AND node_id = ?", workspaceID, settings.ID, nodeID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: 策略目标不在工作空间分配范围", ErrNotReady)
			}
			return err
		}
		var node nodeRecord
		if err := tx.First(&node, "id = ? AND instance_id = ?", nodeID, settings.ID).Error; err != nil {
			return missing(err)
		}
		var registry registryRecord
		if err := tx.First(&registry, "id = ? AND instance_id = ?", policy.RegistryID, settings.ID).Error; err != nil {
			return missing(err)
		}
		prefix := strings.Trim(strings.Join([]string{registry.RepositoryPrefix, policy.RepositoryPrefix}, "/"), "/")
		repository := strings.TrimPrefix(registry.URL, "https://") + "/"
		if prefix != "" {
			repository += prefix + "/"
		}
		repository += applicationID
		result = ExecutionSummary{PolicyRevision: policy.Revision, Platform: Platform, NodeID: node.ID, NodeName: node.Name, ImageRepository: repository, ResourceDefault: policy.ResourceDefault, ResourceLimits: policy.ResourceLimits, AutoRecoveryEnabled: policy.AutoRecoveryEnabled}
		if policy.BaseDomain != "" {
			// 默认地址规范：<应用名>-<6位后缀>.<应用根域名>（后缀创建时生成且不变）；
			// 存量应用迁移回填的后缀同样满足规范。
			result.DefaultDomain = appRow.Name + "-" + appRow.AddressSuffix + "." + policy.BaseDomain
		}
		if policy.ResourceDefault.CPUMillis > allocation.CPUMillis || policy.ResourceDefault.MemoryMiB > allocation.MemoryMiB {
			return notReady(&result, "实例默认资源超过工作空间分配额度")
		}
		if result.ResourceLimits.CPUMillis > allocation.CPUMillis {
			result.ResourceLimits.CPUMillis = allocation.CPUMillis
		}
		if result.ResourceLimits.MemoryMiB > allocation.MemoryMiB {
			result.ResourceLimits.MemoryMiB = allocation.MemoryMiB
		}
		if node.Status != "ACTIVE" {
			return notReady(&result, "目标节点已停用或正在排空")
		}
		if node.OS != "linux" || node.Arch != "amd64" || node.ClusterNodeUID == "" {
			return notReady(&result, "目标节点尚未通过Linux/AMD64身份检查")
		}
		if node.LastSeen == nil || time.Since(*node.LastSeen) > 90*time.Second || node.LastSeen.After(time.Now().Add(30*time.Second)) {
			return notReady(&result, "目标节点连接状态已过期")
		}
		if runtime {
			if !node.RuntimeReady || !node.IngressReady || !node.MetricsReady {
				return notReady(&result, "节点运行、入口或监控资格未通过")
			}
		} else if !node.BuildReady {
			return notReady(&result, "节点构建资格未通过")
		}
		if registry.Status != "ACTIVE" || registry.VerifiedAt == nil || registry.VerifiedRevision == nil || *registry.VerifiedRevision != registry.Revision {
			return notReady(&result, "输出仓库尚未验证当前配置的推送与读取权限")
		}
		result.Ready = true
		return nil
	})
	return result, err
}
func notReady(result *ExecutionSummary, reason string) error {
	result.Reason = reason
	return fmt.Errorf("%w: %s", ErrNotReady, reason)
}
