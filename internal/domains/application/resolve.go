package application

import (
	"context"
	"errors"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
)

// ResolveWorkspace 只解析当前身份有权读取的应用归属，供按应用固定的 MCP 入口使用。
func (s *Service) ResolveWorkspace(ctx context.Context, p authorization.Principal, applicationID string) (string, error) {
	var workspaceID string
	err := s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		var app model.Application
		if err := tx.Select("id", "workspace_id").First(&app, "id = ?", applicationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if _, _, err := s.Access.Application(tx, current, app.WorkspaceID, applicationID, authorization.ApplicationRead); err != nil {
			return err
		}
		workspaceID = app.WorkspaceID
		return nil
	})
	return workspaceID, err
}
