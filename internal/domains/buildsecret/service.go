package buildsecret

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
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const MaxValueBytes = 64 << 10

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)

type Service struct {
	db     *gorm.DB
	access *authorization.Access
	box    *security.SecretBox
}

func NewService(db *gorm.DB, access *authorization.Access, box *security.SecretBox) *Service {
	return &Service{db: db, access: access, box: box}
}
func view(value secretRecord) View {
	return View{ID: value.ID, Name: value.Name, Purpose: value.Purpose, Status: value.Status, CurrentVersion: value.CurrentVersion, Configured: true, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}
func aad(space, app, id string, version int64) string {
	return fmt.Sprintf("workspace/%s/application/%s/build-secret/%s/version/%d", space, app, id, version)
}
func validateValue(value string) error {
	if len(value) < 1 || len(value) > MaxValueBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%w: 密钥需要1～65536字节UTF-8文本", authorization.ErrInvalid)
	}
	return nil
}
func (s *Service) authorized(tx *gorm.DB, p authorization.Principal, space, app string) error {
	if p.IsBearer() {
		return authorization.ErrDenied
	}
	_, _, err := s.access.Application(tx, p, space, app, authorization.SecretWrite)
	return err
}
func mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrNotFound
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return fmt.Errorf("%w: 应用内已存在同名构建密钥", authorization.ErrConflict)
	}
	return err
}
func (s *Service) Create(ctx context.Context, p authorization.Principal, space, app string, input CreateInput) (View, error) {
	if !namePattern.MatchString(input.Name) || input.Name != strings.TrimSpace(input.Name) || !utf8.ValidString(input.Purpose) || utf8.RuneCountInString(input.Purpose) > 500 {
		return View{}, authorization.ErrInvalid
	}
	if err := validateValue(input.Value); err != nil {
		return View{}, err
	}
	var result View
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorized(tx, current, space, app); err != nil {
			return err
		}
		id, err := security.NewID("bsec")
		if err != nil {
			return err
		}
		encrypted, err := s.box.Encrypt([]byte(input.Value), aad(space, app, id, 1))
		if err != nil {
			return fmt.Errorf("构建密钥加密失败")
		}
		value := secretRecord{ID: id, WorkspaceID: space, ApplicationID: app, Name: input.Name, Purpose: strings.TrimSpace(input.Purpose), Status: "ACTIVE", CurrentVersion: 1}
		if err := tx.Create(&value).Error; err != nil {
			return mapError(err)
		}
		if err := tx.Create(&versionRecord{SecretID: id, Version: 1, WorkspaceID: space, ApplicationID: app, EncryptedValue: encrypted}).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "application.build-secret.create", "build-secret", id, &space, &app); err != nil {
			return err
		}
		result = view(value)
		return nil
	})
	return result, err
}
func (s *Service) Replace(ctx context.Context, p authorization.Principal, space, app, id string, input ReplaceInput) (View, error) {
	if input.ExpectedVersion < 1 {
		return View{}, authorization.ErrInvalid
	}
	if err := validateValue(input.Value); err != nil {
		return View{}, err
	}
	var result View
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorized(tx, current, space, app); err != nil {
			return err
		}
		var value secretRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "workspace_id = ? AND application_id = ? AND id = ?", space, app, id).Error; err != nil {
			return mapError(err)
		}
		if value.Status != "ACTIVE" || value.CurrentVersion != input.ExpectedVersion {
			return authorization.ErrConflict
		}
		version := value.CurrentVersion + 1
		encrypted, err := s.box.Encrypt([]byte(input.Value), aad(space, app, id, version))
		if err != nil {
			return fmt.Errorf("构建密钥加密失败")
		}
		if err := tx.Create(&versionRecord{SecretID: id, Version: version, WorkspaceID: space, ApplicationID: app, EncryptedValue: encrypted}).Error; err != nil {
			return err
		}
		updated := tx.Model(&secretRecord{}).Where("workspace_id = ? AND application_id = ? AND id = ? AND current_version = ? AND status = 'ACTIVE'", space, app, id, input.ExpectedVersion).Updates(map[string]any{"current_version": version})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "application.build-secret.replace", "build-secret", id, &space, &app); err != nil {
			return err
		}
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		result = view(value)
		return nil
	})
	return result, err
}
func (s *Service) Get(ctx context.Context, p authorization.Principal, space, app, id string) (View, error) {
	var result View
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorized(tx, current, space, app); err != nil {
			return err
		}
		var value secretRecord
		if err := tx.First(&value, "workspace_id = ? AND application_id = ? AND id = ?", space, app, id).Error; err != nil {
			return mapError(err)
		}
		result = view(value)
		return nil
	})
	return result, err
}
func (s *Service) List(ctx context.Context, p authorization.Principal, space, app string, limit int, cursor string) (Page, error) {
	result := Page{Items: make([]View, 0)}
	scope := "build-secrets:" + space + ":" + app + ":" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorized(tx, current, space, app); err != nil {
			return err
		}
		query := tx.Model(&secretRecord{}).Where("workspace_id = ? AND application_id = ?", space, app)
		if after != "" {
			query = query.Where("id > ?", after)
		}
		var values []secretRecord
		if err := query.Order("id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(scope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			result.Items = append(result.Items, view(value))
		}
		return nil
	})
	return result, err
}
func (s *Service) Deactivate(ctx context.Context, p authorization.Principal, space, app, id string, expectedVersion int64) error {
	if expectedVersion < 1 {
		return authorization.ErrInvalid
	}
	return s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.authorized(tx, current, space, app); err != nil {
			return err
		}
		var value secretRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "workspace_id = ? AND application_id = ? AND id = ?", space, app, id).Error; err != nil {
			return mapError(err)
		}
		if value.CurrentVersion != expectedVersion {
			return authorization.ErrConflict
		}
		if value.Status == "DISABLED" {
			return nil
		}
		updated := tx.Model(&secretRecord{}).Where("workspace_id = ? AND application_id = ? AND id = ? AND current_version = ? AND status = 'ACTIVE'", space, app, id, expectedVersion).Update("status", "DISABLED")
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		return audit.Record(tx, current.UserID, "application.build-secret.deactivate", "build-secret", id, &space, &app)
	})
}
