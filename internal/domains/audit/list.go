package audit

import (
	"context"
	"errors"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
)

type Service struct {
	access *authorization.Access
}

func NewService(access *authorization.Access) *Service {
	return &Service{access: access}
}

type Page struct {
	Events     []model.AuditEvent
	NextCursor string
}

// List 返回允许查看的审计事实，游标仅可引用当前查询范围内的记录。
func (s *Service) List(ctx context.Context, p authorization.Principal, workspaceID string, limit int, cursor string) (Page, error) {
	if limit == 0 {
		limit = 24
	}
	if limit < 1 || limit > 100 || len(cursor) > 128 || len(workspaceID) > 128 {
		return Page{}, authorization.ErrInvalid
	}
	var page Page
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, actor authorization.Principal) error {
		if workspaceID == "" {
			if err := s.access.RequireInstance(actor); err != nil {
				return err
			}
		} else if err := s.access.RequireWorkspaceAdmin(tx, actor, workspaceID); err != nil {
			return err
		}
		scope := func(query *gorm.DB) *gorm.DB {
			if workspaceID != "" {
				return query.Where("workspace_id = ? AND action NOT LIKE ? AND action NOT LIKE ?", workspaceID, "instance.%", "account.%")
			}
			return query
		}
		events := scope(tx.Model(&model.AuditEvent{}))
		if cursor != "" {
			var last model.AuditEvent
			err := scope(tx.Model(&model.AuditEvent{})).Select("id", "created_at").Take(&last, "id = ?", cursor).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrInvalid
			}
			if err != nil {
				return err
			}
			events = events.Where("(created_at < ? OR (created_at = ? AND id > ?))", last.CreatedAt, last.CreatedAt, last.ID)
		}
		return events.Order("created_at DESC, id ASC").Limit(limit + 1).Find(&page.Events).Error
	})
	if err != nil {
		return Page{}, err
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.NextCursor = page.Events[limit-1].ID
	}
	return page, nil
}
