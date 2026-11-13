// Package workspace 管理工作空间及成员事实，不管理实例基础设施。
package workspace

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Service struct {
	DB     *gorm.DB
	Access *authorization.Access
}

func NewService(db *gorm.DB, access *authorization.Access) *Service {
	return &Service{DB: db, Access: access}
}

func validate(input CreateInput) (CreateInput, error) {
	input.Name, input.Slug, input.Description = strings.TrimSpace(input.Name), strings.TrimSpace(input.Slug), strings.TrimSpace(input.Description)
	if !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 100 || !slugPattern.MatchString(input.Slug) || strings.HasSuffix(input.Slug, "-") || !utf8.ValidString(input.Description) || utf8.RuneCountInString(input.Description) > 2000 {
		return input, fmt.Errorf("%w: 名称1～100字符，slug为小写字母开头的1～63位字母数字连字符且不以连字符结尾，说明不超过2000字符", authorization.ErrInvalid)
	}
	return input, nil
}

func view(value model.Workspace, role string, p authorization.Principal) View {
	capabilities := []string{"workspace:read"}
	if !p.IsBearer() && (role == "instance_admin" || role == "workspace_admin") {
		capabilities = append(capabilities, "workspace:write", "member:write", "application:create")
	}
	return View{ID: value.ID, Name: value.Name, Slug: value.Slug, Description: value.Description, Status: value.Status, Revision: value.Revision, Role: role, Capabilities: capabilities, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func conflict(err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return fmt.Errorf("%w: 工作空间标识已存在", authorization.ErrConflict)
	}
	return err
}

func (s *Service) List(ctx context.Context, supplied authorization.Principal, limit int, cursor string) (Page, error) {
	result := Page{Items: make([]View, 0)}
	scope := "workspaces:" + supplied.UserID + ":" + supplied.TokenID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.Access.Do(ctx, supplied, false, func(tx *gorm.DB, p authorization.Principal) error {
		query := tx.Model(&model.Workspace{}).Where("workspaces.status = ?", model.ResourceActive)
		if !p.InstanceAdmin {
			query = query.Where("EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = workspaces.id AND wm.user_id = ?)", p.UserID)
		}
		if p.IsBearer() {
			if !p.AllowsAnyApplicationRead() || len(p.ApplicationIDs) == 0 {
				return nil
			}
			appQuery := "EXISTS (SELECT 1 FROM applications a WHERE a.workspace_id = workspaces.id AND a.status = ? AND a.id IN ?"
			args := []any{model.ResourceActive, p.ApplicationIDs}
			if !p.InstanceAdmin {
				appQuery += " AND (EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = workspaces.id AND wm.user_id = ? AND wm.role = 'admin') OR EXISTS (SELECT 1 FROM application_members am WHERE am.workspace_id = workspaces.id AND am.application_id = a.id AND am.user_id = ?))"
				args = append(args, p.UserID, p.UserID)
			}
			query = query.Where(appQuery+")", args...)
		}
		if after != "" {
			query = query.Where("workspaces.id > ?", after)
		}
		var values []model.Workspace
		if err := query.Order("workspaces.id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(scope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			role, err := s.Access.WorkspaceRole(tx, p, value.ID)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, view(value, role, p))
		}
		return nil
	})
	return result, err
}

func (s *Service) Get(ctx context.Context, p authorization.Principal, id string) (View, error) {
	var result View
	err := s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		role, err := s.Access.WorkspaceRole(tx, current, id)
		if err != nil {
			return err
		}
		if current.IsBearer() {
			query, err := s.Access.ApplicationQuery(tx, current, id)
			if err != nil {
				return err
			}
			var count int64
			if err := query.Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return authorization.ErrNotFound
			}
		}
		var value model.Workspace
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		result = view(value, role, current)
		return nil
	})
	return result, err
}

func (s *Service) Create(ctx context.Context, p authorization.Principal, input CreateInput) (View, error) {
	input, err := validate(input)
	if err != nil {
		return View{}, err
	}
	id, err := security.NewID("wsp")
	if err != nil {
		return View{}, err
	}
	var result View
	err = s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireInstance(current); err != nil {
			return err
		}
		value := model.Workspace{ID: id, Name: input.Name, Slug: input.Slug, Description: input.Description, Status: model.ResourceActive, Revision: 1}
		if err := tx.Create(&value).Error; err != nil {
			return conflict(err)
		}
		if err := tx.Create(&model.WorkspaceMember{WorkspaceID: id, UserID: current.UserID, Role: model.WorkspaceAdmin}).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "workspace.create", "workspace", id, &id, nil); err != nil {
			return err
		}
		result = view(value, "instance_admin", current)
		return nil
	})
	return result, err
}

func (s *Service) Update(ctx context.Context, p authorization.Principal, id string, input UpdateInput) (View, error) {
	validated, err := validate(CreateInput{Name: input.Name, Slug: input.Slug, Description: input.Description})
	if err != nil {
		return View{}, err
	}
	if input.ExpectedRevision < 1 {
		return View{}, authorization.ErrInvalid
	}
	var result View
	err = s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, id); err != nil {
			return err
		}
		updated := tx.Model(&model.Workspace{}).Where("id = ? AND revision = ? AND status = ?", id, input.ExpectedRevision, model.ResourceActive).Updates(map[string]any{"name": validated.Name, "slug": validated.Slug, "description": validated.Description, "revision": gorm.Expr("revision + 1")})
		if updated.Error != nil {
			return conflict(updated.Error)
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "workspace.update", "workspace", id, &id, nil); err != nil {
			return err
		}
		var value model.Workspace
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		role, err := s.Access.WorkspaceRole(tx, current, id)
		if err != nil {
			return err
		}
		result = view(value, role, current)
		return nil
	})
	return result, err
}

func (s *Service) Archive(ctx context.Context, p authorization.Principal, id string, expectedRevision int64) error {
	if expectedRevision < 1 {
		return authorization.ErrInvalid
	}
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, id); err != nil {
			return err
		}
		var value model.Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		// 后续构建/运行模块接入前不对未知活动任务作停止承诺。
		updated := tx.Model(&model.Workspace{}).Where("id = ? AND revision = ?", id, expectedRevision).Updates(map[string]any{"status": model.ResourceArchived, "revision": gorm.Expr("revision + 1")})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		return audit.Record(tx, current.UserID, "workspace.archive", "workspace", id, &id, nil)
	})
}
