package oauth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
)

// ClientView 是实例管理接口可见的客户端视图。公共客户端本无 secret，
// 这里也只投影 oauth_clients 的非凭证字段；source 区分预登记与 CIMD 自动登记。
type ClientView struct {
	ID           string
	Name         string
	Source       string
	RedirectURIs []string
	Scopes       []string
	Resources    []string
	Active       bool
	CreatedAt    time.Time
}

type ClientPage struct {
	Items      []ClientView
	NextCursor string
}

// clientListScope 把列表游标绑定到本列表，防止跨列表重放游标。
const clientListScope = "instance-oauth-clients"

func clientView(value clientRecord) ClientView {
	return ClientView{ID: value.ID, Name: value.Name, Source: value.Source, RedirectURIs: slices.Clone(value.RedirectURIs), Scopes: slices.Clone(value.Scopes), Resources: slices.Clone(value.Resources), Active: value.Active, CreatedAt: value.CreatedAt}
}

// RegisterClientView 复用 RegisterClient 的校验、实例管理员授权与审计事务，
// 并为管理入口补充显式冲突语义：底层登记对重复主键静默幂等，重复登记必须可感知。
// 冲突预检与实例管理员复核在同一只读事务内先行完成，避免未授权调用者借冲突差异探测 client 存在性；
// 预检与最终写入之间仍存在极小竞窗，数据库主键约束兜底，最坏结果是并发重复登记静默幂等。
func (s *Service) RegisterClientView(ctx context.Context, p authorization.Principal, input ClientInput) (ClientView, error) {
	var exists bool
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		err := tx.First(&clientRecord{}, "id = ?", input.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		return nil
	})
	if err != nil {
		return ClientView{}, err
	}
	if exists {
		return ClientView{}, fmt.Errorf("%w: OAuth 客户端 %s 已存在", authorization.ErrConflict, input.ID)
	}
	if err := s.RegisterClient(ctx, p, input); err != nil {
		return ClientView{}, err
	}
	var client clientRecord
	if err := s.db.WithContext(ctx).First(&client, "id = ?", input.ID).Error; err != nil {
		return ClientView{}, err
	}
	return clientView(client), nil
}

// SetClientActiveView 复用 SetClientActive（停用即撤销既有授权），返回更新后的客户端视图。
func (s *Service) SetClientActiveView(ctx context.Context, p authorization.Principal, id string, active bool) (ClientView, error) {
	if err := s.SetClientActive(ctx, p, id, active); err != nil {
		return ClientView{}, err
	}
	var client clientRecord
	if err := s.db.WithContext(ctx).First(&client, "id = ?", id).Error; err != nil {
		return ClientView{}, err
	}
	return clientView(client), nil
}

// ListClients 按实例列出全部 OAuth 客户端（含停用）。id 是稳定主键，用 id ASC 键集分页；
// 游标经 store.ParsePage 绑定列表作用域，跨列表或损坏的游标一律拒绝，不回退首页。
func (s *Service) ListClients(ctx context.Context, p authorization.Principal, limit int, cursor string) (ClientPage, error) {
	result := ClientPage{Items: make([]ClientView, 0)}
	limit, after, err := store.ParsePage(limit, cursor, clientListScope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		query := tx.Model(&clientRecord{})
		if after != "" {
			query = query.Where("id > ?", after)
		}
		var values []clientRecord
		if err := query.Order("id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(clientListScope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			result.Items = append(result.Items, clientView(value))
		}
		return nil
	})
	return result, err
}
