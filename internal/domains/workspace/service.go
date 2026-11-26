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
	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/lib/naming"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 统一命名规范（internal/lib/naming）：空间名与应用名同一规范。
var slugPattern = regexp.MustCompile(`^[a-z](-?[a-z0-9])*$`)

type Service struct {
	DB                 *gorm.DB
	Access             *authorization.Access
	ApplicationCleanup interface {
		HasPendingCleanup(tx *gorm.DB, workspaceID string) (bool, error)
	}
}

func NewService(db *gorm.DB, access *authorization.Access) *Service {
	return &Service{DB: db, Access: access}
}

// validateNameDesc 校验显示名称与说明，创建与更新共用；空间名校验见 validateSlug。
func validateNameDesc(name, description string) error {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(description) || utf8.RuneCountInString(description) > 2000 {
		return fmt.Errorf("%w: 名称1～100字符，说明不超过2000字符", authorization.ErrInvalid)
	}
	return nil
}

// validateSlug 校验空间名格式与保留名单：slug 会派生集群命名空间
// （build-<slug>/prod-<slug>），与 K8s 系统命名空间或平台组件命名空间
// 同名会造成归属冲突，创建时即拒绝。
func validateSlug(slug string) (string, error) {
	if len(slug) > naming.NameMaxLen || !slugPattern.MatchString(slug) {
		return slug, fmt.Errorf("%w: 空间名为小写字母开头、最长30位的小写字母数字中划线（无连续--、不以-结尾）", authorization.ErrInvalid)
	}
	if naming.ReservedWorkspaceNames[slug] {
		return slug, fmt.Errorf("%w: 空间名 %s 为系统保留名称", authorization.ErrInvalid, slug)
	}
	return slug, nil
}

func validate(input CreateInput) (CreateInput, error) {
	input.Name, input.Slug, input.Description = strings.TrimSpace(input.Name), strings.TrimSpace(input.Slug), strings.TrimSpace(input.Description)
	if err := validateNameDesc(input.Name, input.Description); err != nil {
		return input, err
	}
	slug, err := validateSlug(input.Slug)
	if err != nil {
		return input, err
	}
	input.Slug = slug
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
		query, visible := applyWorkspaceListScope(tx, p)
		if !visible {
			return nil
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

// Get 按不可变资源 ID 或唯一空间名（slug）定位工作空间：URL 等面向用户的场景
// 用 slug（唯一且创建后不可修改），内部引用与 CLI 用 ID；两者都提供时以 ID 为准。
func (s *Service) Get(ctx context.Context, p authorization.Principal, id, slug string) (View, error) {
	if (id == "") == (slug == "") {
		return View{}, fmt.Errorf("%w: 工作空间 ID 与空间名必须二选一", authorization.ErrInvalid)
	}
	var result View
	err := s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		locator := id
		if id == "" {
			// slug → 资源 ID：授权检查与后续读取仍按 ID 执行；slug 唯一且不可变，
			// 空间名解析结果与资源 ID 一一对应。
			var value model.Workspace
			if err := tx.First(&value, "slug = ?", slug).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return authorization.ErrNotFound
				}
				return err
			}
			locator = value.ID
		}
		role, err := s.Access.WorkspaceRole(tx, current, locator)
		if err != nil {
			return err
		}
		if current.IsBearer() {
			query, err := s.Access.ApplicationQuery(tx, current, locator)
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
		if err := tx.First(&value, "id = ?", locator).Error; err != nil {
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
		// 授权：全局管理员恒可；非全局管理员仅当实例开启"自由建空间"且为会话
		// 身份（应用范围 PAT 不继承该能力，与实例管理权限同边界）。
		selfCreate := current.InstanceAdmin && !current.IsBearer()
		if !current.InstanceAdmin {
			var settings instanceSettingsRow
			if err := tx.Table("instance_settings").Select("workspace_self_create").Where("singleton = TRUE").Take(&settings).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
			}
			selfCreate = settings.WorkspaceSelfCreate && !current.IsBearer()
		}
		if !selfCreate {
			return authorization.ErrDenied
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
		// 视图角色如实反映创建者身份：全局管理员保留其角色；非全局管理员经
		// 自由建空间入口创建后即为空间管理员（成员插入已赋予）。
		creatorRole := "workspace_admin"
		if current.InstanceAdmin {
			creatorRole = "instance_admin"
		}
		result = view(value, creatorRole, current)
		return nil
	})
	return result, err
}

func (s *Service) Update(ctx context.Context, p authorization.Principal, id string, input UpdateInput) (View, error) {
	// 空间名（slug）创建后不可修改（设计约束）：slug 派生集群命名空间与默认
	// 地址，改名会造成归属漂移；Update 只接受显示名称与说明变更。
	input.Name, input.Slug, input.Description = strings.TrimSpace(input.Name), strings.TrimSpace(input.Slug), strings.TrimSpace(input.Description)
	if err := validateNameDesc(input.Name, input.Description); err != nil {
		return View{}, err
	}
	if input.ExpectedRevision < 1 {
		return View{}, authorization.ErrInvalid
	}
	var result View
	err := s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, id); err != nil {
			return err
		}
		var value model.Workspace
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		// 空间名（slug）创建后不可修改：非空且与现有值不同即拒绝；空 slug 视为
		// "未提供，保持不变"（只改名称/说明的客户端不必回传现有 slug）。
		if input.Slug != "" && input.Slug != value.Slug {
			return fmt.Errorf("%w: 空间名创建后不可修改", authorization.ErrInvalid)
		}
		updated := tx.Model(&model.Workspace{}).Where("id = ? AND revision = ? AND status = ?", id, input.ExpectedRevision, model.ResourceActive).Updates(map[string]any{"name": input.Name, "description": input.Description, "revision": gorm.Expr("revision + 1")})
		if updated.Error != nil {
			return conflict(updated.Error)
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "workspace.update", "workspace", id, &id, nil); err != nil {
			return err
		}
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
		// 空间归档前要求其下应用全部已归档：应用归档自身要求停止运行且无在途
		// 部署，由此传递保证归档空间内不存在仍在运行的工作负载。
		var active int64
		if err := tx.Model(&model.Application{}).Where("workspace_id = ? AND status = ?", id, model.ResourceActive).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("%w: 工作空间仍有未归档的应用，请先归档全部应用", authorization.ErrConflict)
		}
		if s.ApplicationCleanup != nil {
			pending, err := s.ApplicationCleanup.HasPendingCleanup(tx, id)
			if err != nil {
				return err
			}
			if pending {
				return fmt.Errorf("%w: 请先完成空间内已受理的应用清理", authorization.ErrConflict)
			}
		}
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

// instanceSettingsRow 是工作空间服务读取实例开关的轻量投影。
type instanceSettingsRow struct {
	WorkspaceSelfCreate bool `gorm:"column:workspace_self_create"`
}
