package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func registryView(value registryRecord) RegistryView {
	return RegistryView{ID: value.ID, Name: value.Name, URL: value.URL, RepositoryPrefix: value.RepositoryPrefix, Status: value.Status, CredentialConfigured: value.Username != "", CredentialVersion: value.CredentialVersion, Revision: value.Revision, VerifiedAt: value.VerifiedAt}
}
func (s *Service) CreateRegistry(ctx context.Context, p authorization.Principal, input RegistryInput) (RegistryView, error) {
	input, err := validateRegistry(input)
	if err != nil {
		return RegistryView{}, err
	}
	var result RegistryView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		settings, err := instance(tx)
		if err != nil {
			return err
		}
		id, err := security.NewID("reg")
		if err != nil {
			return err
		}
		encrypted, err := s.box.Encrypt([]byte(input.Password), credentialIdentity(settings.ID, id, 1))
		if err != nil {
			return err
		}
		value := registryRecord{ID: id, InstanceID: settings.ID, Name: input.Name, URL: input.URL, RepositoryPrefix: input.RepositoryPrefix, Username: input.Username, EncryptedPassword: encrypted, CredentialVersion: 1, Status: "ACTIVE", Revision: 1}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		if err := tx.Create(&credentialRecord{RegistryID: id, Version: 1, Username: input.Username, EncryptedPassword: encrypted, RegistryURL: input.URL, RepositoryPrefix: input.RepositoryPrefix}).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "instance.registry.create", "registry", id, nil, nil); err != nil {
			return err
		}
		result = registryView(value)
		return nil
	})
	return result, err
}
func (s *Service) UpdateRegistry(ctx context.Context, p authorization.Principal, id string, input UpdateRegistryInput) (RegistryView, error) {
	if input.ExpectedRevision < 1 || (input.Status != "ACTIVE" && input.Status != "DISABLED") || (input.Username == nil) != (input.Password == nil) {
		return RegistryView{}, authorization.ErrInvalid
	}
	base := RegistryInput{Name: input.Name, URL: input.URL, RepositoryPrefix: input.RepositoryPrefix}
	if input.Password != nil {
		base.Username = *input.Username
		base.Password = *input.Password
	}
	base, err := validateRegistry(base)
	if err != nil {
		return RegistryView{}, err
	}
	var result RegistryView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value registryRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		if value.Revision != input.ExpectedRevision {
			return authorization.ErrConflict
		}
		if input.Password == nil && (base.URL != value.URL || base.RepositoryPrefix != value.RepositoryPrefix) {
			return invalid("修改仓库目标时必须显式替换凭据，不能将旧凭据自动发送到新目标")
		}
		updates := map[string]any{"name": base.Name, "url": base.URL, "repository_prefix": base.RepositoryPrefix, "status": input.Status, "revision": gorm.Expr("revision + 1"), "verified_at": nil, "verified_revision": nil}
		if input.Password != nil {
			version := value.CredentialVersion + 1
			encrypted, err := s.box.Encrypt([]byte(base.Password), credentialIdentity(value.InstanceID, id, version))
			if err != nil {
				return err
			}
			if err := tx.Create(&credentialRecord{RegistryID: id, Version: version, Username: base.Username, EncryptedPassword: encrypted, RegistryURL: base.URL, RepositoryPrefix: base.RepositoryPrefix}).Error; err != nil {
				return err
			}
			updates["username"] = base.Username
			updates["encrypted_password"] = encrypted
			updates["credential_version"] = version
		}
		updated := tx.Model(&registryRecord{}).Where("id = ? AND revision = ?", id, input.ExpectedRevision).Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := audit.Record(tx, current.UserID, "instance.registry.update", "registry", id, nil, nil); err != nil {
			return err
		}
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		result = registryView(value)
		return nil
	})
	return result, err
}
func (s *Service) GetRegistry(ctx context.Context, p authorization.Principal, id string) (RegistryView, error) {
	var result RegistryView
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value registryRecord
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		result = registryView(value)
		return nil
	})
	return result, err
}
func (s *Service) ListRegistries(ctx context.Context, p authorization.Principal, limit int, cursor string) (RegistryPage, error) {
	result := RegistryPage{Items: make([]RegistryView, 0)}
	scope := "instance-registries:" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		query := tx.Model(&registryRecord{})
		if after != "" {
			query = query.Where("id > ?", after)
		}
		var values []registryRecord
		if err := query.Order("id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(scope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			result.Items = append(result.Items, registryView(value))
		}
		return nil
	})
	return result, err
}

// GetInstanceSettings 读取实例设置（当前仅含"自由建空间"开关）。登录身份均可读：
// 该开关只决定控制台是否展示创建工作空间入口，不含敏感配置；授权仍由
// CreateWorkspace 服务端判定。
func (s *Service) GetInstanceSettings(ctx context.Context, p authorization.Principal) (bool, error) {
	var value bool
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, "any", "any", authorization.BuildRead); err != nil {
			return err
		}
		var row instanceRecord
		if err := tx.First(&row, "singleton = TRUE").Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		value = row.WorkspaceSelfCreate
		return nil
	})
	return value, err
}

// SetWorkspaceSelfCreate 设置"自由建空间"开关：仅限实例管理员。开启后所有登录
// 身份（会话）可在控制台自助创建工作空间并成为空间管理员；关闭恢复为仅实例
// 管理员可创建。
func (s *Service) SetWorkspaceSelfCreate(ctx context.Context, p authorization.Principal, enabled bool) (bool, error) {
	var value bool
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		settings, err := instance(tx)
		if err != nil {
			return err
		}
		if err := tx.Model(&instanceRecord{}).Where("singleton = TRUE").Update("workspace_self_create", enabled).Error; err != nil {
			return err
		}
		value = enabled
		returned := fmt.Sprintf("%t", enabled)
		return audit.Record(tx, current.UserID, "instance.settings.self_create", "instance", settings.ID, nil, &returned)
	})
	return value, err
}
