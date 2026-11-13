package inventory

import (
	"context"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
)

func nodeView(value nodeRecord) NodeView {
	return NodeView{ID: value.ID, Name: value.Name, WorkerID: value.WorkerID, Status: value.Status, ClusterNodeUID: value.ClusterNodeUID, OS: value.OS, Arch: value.Arch, ConnectedAt: value.ConnectedAt, LastSeen: value.LastSeen, BuildReady: value.BuildReady, RuntimeReady: value.RuntimeReady, IngressReady: value.IngressReady, MetricsReady: value.MetricsReady, ReadinessReason: value.ReadinessReason, Capacity: value.Capacity, Revision: value.Revision}
}
func (s *Service) CreateNode(ctx context.Context, p authorization.Principal, input NodeInput) (NodeView, error) {
	input, err := validateNode(input)
	if err != nil {
		return NodeView{}, err
	}
	var result NodeView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		settings, err := instance(tx)
		if err != nil {
			return err
		}
		id, err := security.NewID("nod")
		if err != nil {
			return err
		}
		value := nodeRecord{ID: id, InstanceID: settings.ID, Name: input.Name, WorkerID: input.WorkerID, Status: "ACTIVE", Capacity: input.Capacity, Revision: 1, ReadinessReason: "等待真实节点资格检查"}
		if err := tx.Create(&value).Error; err != nil {
			return conflicting(err)
		}
		if err := audit.Record(tx, current.UserID, "instance.node.create", "node", id, nil, nil); err != nil {
			return err
		}
		result = nodeView(value)
		return nil
	})
	return result, err
}
func (s *Service) UpdateNode(ctx context.Context, p authorization.Principal, id string, input UpdateNodeInput) (NodeView, error) {
	name, err := validateName(input.Name)
	if err != nil {
		return NodeView{}, err
	}
	if input.ExpectedRevision < 1 || input.Capacity < 1 || input.Capacity > 1000 || (input.Status != "ACTIVE" && input.Status != "DRAINING") {
		return NodeView{}, authorization.ErrInvalid
	}
	var result NodeView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value nodeRecord
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		if value.Status == "DISABLED" {
			return invalid("已停用节点必须通过重新登记流程启用，不能复用旧证书")
		}
		// 停用/重新登记归 enrollment 管理，不能经普通配置入口复活旧证书。
		// 任何管理变更均撤销旧资格，随后必须由真实节点报告重新验证。
		updated := tx.Model(&nodeRecord{}).Where("id = ? AND revision = ? AND status <> 'DISABLED'", id, input.ExpectedRevision).Updates(map[string]any{"name": name, "capacity": input.Capacity, "status": input.Status, "revision": gorm.Expr("revision + 1"), "build_ready": false, "runtime_ready": false, "ingress_ready": false, "metrics_ready": false, "readiness_reason": "配置已变更，等待重新检查"})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "instance.node.update", "node", id, nil, nil); err != nil {
			return err
		}
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		result = nodeView(value)
		return nil
	})
	return result, err
}
func (s *Service) GetNode(ctx context.Context, p authorization.Principal, id string) (NodeView, error) {
	var result NodeView
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value nodeRecord
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		result = nodeView(value)
		return nil
	})
	return result, err
}
func (s *Service) ListNodes(ctx context.Context, p authorization.Principal, limit int, cursor string) (NodePage, error) {
	result := NodePage{Items: make([]NodeView, 0)}
	scope := "instance-nodes:" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		query := tx.Model(&nodeRecord{})
		if after != "" {
			query = query.Where("id > ?", after)
		}
		var values []nodeRecord
		if err := query.Order("id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(scope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			result.Items = append(result.Items, nodeView(value))
		}
		return nil
	})
	return result, err
}
