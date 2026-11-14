// Package application 实现应用业务边界；应用不能覆盖平台节点、仓库与架构。
package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 统一命名规范（internal/domain/name.go）：应用名与空间名同一规范。
var namePattern = regexp.MustCompile(`^[a-z](-?[a-z0-9])*$`)

type Service struct {
	DB     *gorm.DB
	Access *authorization.Access
}

func NewService(db *gorm.DB, access *authorization.Access) *Service {
	return &Service{DB: db, Access: access}
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) > domain.NameMaxLen || !namePattern.MatchString(name) {
		return "", fmt.Errorf("%w: 应用名称为小写字母开头、最长30位的小写字母数字中划线（无连续--、不以-结尾）", authorization.ErrInvalid)
	}
	return name, nil
}
func (s *Service) view(value model.Application, role string, p authorization.Principal) View {
	return View{ID: value.ID, WorkspaceID: value.WorkspaceID, Name: value.Name, Description: value.Description, Status: value.Status, Revision: value.Revision, Capabilities: s.Access.Policy.Capabilities(role, p, value.ID), CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}
func conflict(err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return fmt.Errorf("%w: 当前空间已有同名应用", authorization.ErrConflict)
	}
	return err
}

func (s *Service) List(ctx context.Context, p authorization.Principal, workspaceID string, limit int, cursor string) (Page, error) {
	result := Page{Items: make([]View, 0)}
	scope := "applications:" + workspaceID + ":" + p.UserID + ":" + p.TokenID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		query, err := s.Access.ApplicationQuery(tx, current, workspaceID)
		if err != nil {
			return err
		}
		if after != "" {
			query = query.Where("applications.id > ?", after)
		}
		var values []model.Application
		if err := query.Order("applications.id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(scope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			_, role, err := s.Access.Application(tx, current, workspaceID, value.ID, authorization.ApplicationRead)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, s.view(value, role, current))
		}
		return nil
	})
	return result, err
}

func (s *Service) Get(ctx context.Context, p authorization.Principal, workspaceID, id string) (View, error) {
	var result View
	err := s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		value, role, err := s.Access.Application(tx, current, workspaceID, id, authorization.ApplicationRead)
		if err != nil {
			return err
		}
		result = s.view(value, role, current)
		return nil
	})
	return result, err
}

func (s *Service) Create(ctx context.Context, p authorization.Principal, workspaceID string, input CreateInput) (View, error) {
	name, err := normalizeName(input.Name)
	if err != nil {
		return View{}, err
	}
	if input.IdempotencyKey == "" || input.IdempotencyKey != strings.TrimSpace(input.IdempotencyKey) || len(input.IdempotencyKey) > 128 || !utf8.ValidString(input.IdempotencyKey) {
		return View{}, fmt.Errorf("%w: 必须提供1～128字节的幂等键", authorization.ErrInvalid)
	}
	payload, _ := json.Marshal(struct {
		Name string `json:"name"`
	}{name})
	hash := sha256.Sum256(payload)
	fingerprint := hex.EncodeToString(hash[:])
	var result View
	err = s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		// 锁同一空间保证并发首次插入幂等键无需跨事务占位，未来再按测量细化粒度。
		var workspace model.Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&workspace, "id = ?", workspaceID).Error; err != nil {
			return err
		}
		const action = "application.create"
		var key model.IdempotencyKey
		err := tx.First(&key, "workspace_id = ? AND actor_id = ? AND action = ? AND key = ?", workspaceID, current.UserID, action, input.IdempotencyKey).Error
		if err == nil {
			if key.Fingerprint != fingerprint {
				return fmt.Errorf("%w: 相同幂等键对应不同创建请求", authorization.ErrConflict)
			}
			var value model.Application
			if err := tx.First(&value, "workspace_id = ? AND id = ?", workspaceID, key.ResultID).Error; err != nil {
				return err
			}
			role, err := s.Access.WorkspaceRole(tx, current, workspaceID)
			if err != nil {
				return err
			}
			result = s.view(value, role, current)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		id, err := security.NewID("app")
		if err != nil {
			return err
		}
		// 默认地址后缀：创建时生成且不变（小写字母开头的6位小写字母数字）。
		suffixBytes := make([]byte, 6)
		if _, err := rand.Read(suffixBytes); err != nil {
			return err
		}
		const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
		suffix := string(alphabet[int(suffixBytes[0])%26])
		for _, b := range suffixBytes[1:] {
			suffix += string(alphabet[int(b)%len(alphabet)])
		}
		value := model.Application{ID: id, WorkspaceID: workspaceID, Name: name, AddressSuffix: suffix, Status: model.ResourceActive, Revision: 1}
		if err := tx.Create(&value).Error; err != nil {
			return conflict(err)
		}
		key = model.IdempotencyKey{WorkspaceID: workspaceID, ActorID: current.UserID, Action: action, Key: input.IdempotencyKey, Fingerprint: fingerprint, ResultID: id}
		if err := tx.Create(&key).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, action, "application", id, &workspaceID, &id); err != nil {
			return err
		}
		role, err := s.Access.WorkspaceRole(tx, current, workspaceID)
		if err != nil {
			return err
		}
		result = s.view(value, role, current)
		return nil
	})
	return result, err
}

func (s *Service) Update(ctx context.Context, p authorization.Principal, workspaceID, id string, input UpdateInput) (View, error) {
	name, err := normalizeName(input.Name)
	if err != nil {
		return View{}, err
	}
	description := strings.TrimSpace(input.Description)
	if input.ExpectedRevision < 1 || !utf8.ValidString(description) || utf8.RuneCountInString(description) > 2000 {
		return View{}, authorization.ErrInvalid
	}
	var result View
	err = s.Access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if current.IsBearer() {
			return authorization.ErrDenied
		}
		_, role, err := s.Access.Application(tx, current, workspaceID, id, authorization.ConfigWrite)
		if err != nil {
			return err
		}
		updated := tx.Model(&model.Application{}).Where("workspace_id = ? AND id = ? AND revision = ? AND status = ?", workspaceID, id, input.ExpectedRevision, model.ResourceActive).Updates(map[string]any{"name": name, "description": description, "revision": gorm.Expr("revision + 1")})
		if updated.Error != nil {
			return conflict(updated.Error)
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "application.update", "application", id, &workspaceID, &id); err != nil {
			return err
		}
		var value model.Application
		if err := tx.First(&value, "workspace_id = ? AND id = ?", workspaceID, id).Error; err != nil {
			return err
		}
		result = s.view(value, role, current)
		return nil
	})
	return result, err
}

func (s *Service) Archive(ctx context.Context, p authorization.Principal, workspaceID, id string, expectedRevision int64) error {
	if expectedRevision < 1 {
		return authorization.ErrInvalid
	}
	return s.Access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.Access.RequireWorkspaceAdmin(tx, current, workspaceID); err != nil {
			return err
		}
		if _, _, err := s.Access.Application(tx, current, workspaceID, id, authorization.ApplicationWrite); err != nil {
			return err
		}
		updated := tx.Model(&model.Application{}).Where("workspace_id = ? AND id = ? AND revision = ? AND status = ?", workspaceID, id, expectedRevision, model.ResourceActive).Updates(map[string]any{"status": model.ResourceArchived, "revision": gorm.Expr("revision + 1")})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		return audit.Record(tx, current.UserID, "application.archive", "application", id, &workspaceID, &id)
	})
}
