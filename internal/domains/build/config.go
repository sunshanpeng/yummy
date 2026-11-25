package build

import (
	"context"
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/buildsecret"
	"gorm.io/gorm"
)

// SecretResolver 只解析归属及不可变版本，不从公共构建服务读取明文。
type SecretResolver interface {
	Resolve(*gorm.DB, string, string, []buildsecret.Ref) ([]buildsecret.Ref, error)
}
type secretReferenceRecord struct {
	ID                                            string `gorm:"column:id;primaryKey"`
	WorkspaceID, ApplicationID, BuildID, SecretID string
	Version                                       int64
	Name                                          string
	CreatedAt                                     time.Time `gorm:"column:created_at"`
	UpdatedAt                                     time.Time `gorm:"column:updated_at"`
	CreatedBy                                     string    `gorm:"column:created_by"`
	UpdatedBy                                     string    `gorm:"column:updated_by"`
}

func (secretReferenceRecord) IDPrefix() string  { return "bsr" }
func (secretReferenceRecord) TableName() string { return "build_secret_references" }
func (s *Service) resolveSecrets(tx *gorm.DB, ws, app string, refs []SecretRef) ([]SecretRef, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	if s.secrets == nil {
		return nil, invalid("构建密钥解析器尚未接入")
	}
	values := make([]buildsecret.Ref, 0, len(refs))
	for _, r := range refs {
		values = append(values, buildsecret.Ref{SecretID: r.SecretID, Version: r.Version, Name: r.Name})
	}
	frozen, err := s.secrets.Resolve(tx, ws, app, values)
	if err != nil {
		return nil, err
	}
	out := make([]SecretRef, 0, len(frozen))
	for _, r := range frozen {
		out = append(out, SecretRef{SecretID: r.SecretID, Version: r.Version, Name: r.Name})
	}
	return out, nil
}

type ConfigView struct {
	Revision int64  `json:"revision"`
	Config   Config `json:"config"`
}

func (s *Service) GetConfig(ctx context.Context, p authorization.Principal, ws, app string) (ConfigView, error) {
	var out ConfigView
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.ConfigRead); err != nil {
			return err
		}
		var r configRecord
		err := tx.Where("workspace_id = ? AND application_id = ?", ws, app).Order("revision DESC").Take(&r).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			out = ConfigView{Config: Config{Context: "."}}
			return nil
		}
		if err != nil {
			return err
		}
		out = ConfigView{Revision: r.Revision, Config: r.Spec}
		return nil
	})
	return out, err
}

// GetConfigRevision 返回已保存的不可变版本，避免保存后再读最新版本的响应竞态。
func (s *Service) GetConfigRevision(ctx context.Context, p authorization.Principal, ws, app string, revision int64) (ConfigView, error) {
	var out ConfigView
	if revision < 1 {
		return out, authorization.ErrInvalid
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.ConfigRead); err != nil {
			return err
		}
		var r configRecord
		if err := tx.Where("workspace_id = ? AND application_id = ? AND revision = ?", ws, app, revision).Take(&r).Error; err != nil {
			return missing(err)
		}
		out = ConfigView{Revision: r.Revision, Config: r.Spec}
		return nil
	})
	return out, err
}

// GetAttempt 供已认证 worker 重启时恢复同 attempt/JobUID，无网络副作用。
func (s *Service) GetAttempt(ctx context.Context, w WorkerIdentity, id string) (Dispatch, error) {
	var out Dispatch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		b, a, err := lockAttempt(tx, w, id)
		if err != nil {
			return err
		}
		slug, slugErr := workspaceSlug(tx, b.WorkspaceID)
		if slugErr != nil {
			return slugErr
		}
		out = dispatch(b, a, slug)
		if terminal(b.State) && !a.Occupied {
			out.Command = "FINISHED"
		}
		return nil
	})
	return out, err
}
