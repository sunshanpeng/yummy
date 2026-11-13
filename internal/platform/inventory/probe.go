package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/registryclient"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RegistryProbeView struct {
	RegistryID        string    `json:"registryId"`
	Revision          int64     `json:"revision"`
	CredentialVersion int64     `json:"credentialVersion"`
	CheckedAt         time.Time `json:"checkedAt"`
	registryclient.ProbeResult
}

// ProbeRegistry 只检查控制面HTTPS/认证与可选digest读取，不写 verified_at。
// worker侧真实推送并拉取同一探测产物仍是构建资格的必要条件。
func (s *Service) ProbeRegistry(ctx context.Context, p authorization.Principal, id string, expectedRevision int64, readReference string) (RegistryProbeView, error) {
	return s.probeRegistry(ctx, p, id, expectedRevision, readReference, registryclient.Config{AllowPrivate: true})
}
func (s *Service) probeRegistry(ctx context.Context, p authorization.Principal, id string, expectedRevision int64, readReference string, config registryclient.Config) (RegistryProbeView, error) {
	var snapshot registryRecord
	if expectedRevision < 1 {
		return RegistryProbeView{}, authorization.ErrInvalid
	}
	if err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		if err := tx.First(&snapshot, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		if snapshot.Revision != expectedRevision {
			return authorization.ErrConflict
		}
		return nil
	}); err != nil {
		return RegistryProbeView{}, err
	}
	password, err := s.box.Decrypt(snapshot.EncryptedPassword, credentialIdentity(snapshot.InstanceID, id, snapshot.CredentialVersion))
	if err != nil {
		return RegistryProbeView{}, fmt.Errorf("仓库凭据解密失败")
	}
	defer clear(password)
	if readReference != "" {
		prefix := strings.TrimPrefix(snapshot.URL, "https://") + "/"
		if snapshot.RepositoryPrefix != "" {
			prefix += snapshot.RepositoryPrefix + "/"
		}
		if !strings.HasPrefix(readReference, prefix) {
			return RegistryProbeView{}, authorization.ErrInvalid
		}
	}
	config.URL = snapshot.URL
	config.Username = snapshot.Username
	config.Password = string(password)
	client, err := registryclient.New(config)
	if err != nil {
		return RegistryProbeView{}, err
	}
	defer client.Close()
	// 网络与TLS均在事务外完成；结果提交前重验权限与配置版本。
	probe, probeErr := client.Probe(ctx, readReference)
	result := RegistryProbeView{RegistryID: id, Revision: snapshot.Revision, CredentialVersion: snapshot.CredentialVersion, CheckedAt: time.Now().UTC(), ProbeResult: probe}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var latest registryRecord
		if err := tx.First(&latest, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		if latest.Revision != snapshot.Revision || latest.CredentialVersion != snapshot.CredentialVersion {
			return authorization.ErrConflict
		}
		action := "instance.registry.probe"
		if probeErr != nil {
			action = "instance.registry.probe.failed"
		}
		return audit.Record(tx, current.UserID, action, "registry", id, nil, nil)
	})
	if err != nil {
		return RegistryProbeView{}, err
	}
	return result, probeErr
}

// Credential 只交给内部worker/构建适配器，不能作为公共API的返回结构。
type Credential struct {
	URL      string
	Username string
	Password []byte
}

func (c *Credential) Clear() { clear(c.Password); c.Password = nil }

// GetCredential 使用调用者事务读取不可变凭据版本；调用者必须先完成系统任务归属校验。
func (s *Service) GetCredential(tx *gorm.DB, registryID string, version int64) (Credential, error) {
	if version < 1 {
		return Credential{}, authorization.ErrInvalid
	}
	var registry registryRecord
	if err := tx.First(&registry, "id = ?", registryID).Error; err != nil {
		return Credential{}, missing(err)
	}
	var value credentialRecord
	if err := tx.First(&value, "registry_id = ? AND version = ?", registryID, version).Error; err != nil {
		return Credential{}, missing(err)
	}
	password, err := s.box.Decrypt(value.EncryptedPassword, credentialIdentity(registry.InstanceID, registryID, version))
	if err != nil {
		return Credential{}, fmt.Errorf("仓库凭据解密失败")
	}
	return Credential{URL: value.RegistryURL, Username: value.Username, Password: password}, nil
}

// VerifyRegistry 验证输出仓库当前凭据版本并标记 verified：真实执行网络/认证探测
// （可选校验指定引用的读取），全部通过才在事务内写 verified_at/verified_revision；
// 任一不满足即失败关闭不标记，构建与部署以该标记为准。凭据变更（UpdateRegistry）
// 会重置标记，需重新验证。
func (s *Service) VerifyRegistry(ctx context.Context, p authorization.Principal, id string, expectedRevision int64, readReference string) (RegistryView, RegistryProbeView, bool, error) {
	return s.verifyRegistry(ctx, p, id, expectedRevision, readReference, registryclient.Config{AllowPrivate: true})
}

func (s *Service) verifyRegistry(ctx context.Context, p authorization.Principal, id string, expectedRevision int64, readReference string, config registryclient.Config) (RegistryView, RegistryProbeView, bool, error) {
	view, probeErr := s.probeRegistry(ctx, p, id, expectedRevision, readReference, config)
	if probeErr != nil {
		// 请求类错误（冲突/无效/不存在/拒绝）原样返回；探测失败（网络/认证/读取）
		// 属于验证结果本身：失败关闭返回 ok=false，不作为传输层错误。
		if errors.Is(probeErr, authorization.ErrConflict) || errors.Is(probeErr, authorization.ErrInvalid) ||
			errors.Is(probeErr, authorization.ErrNotFound) || errors.Is(probeErr, authorization.ErrDenied) {
			return RegistryView{}, view, false, probeErr
		}
		return RegistryView{}, view, false, nil
	}
	// 失败关闭：网络/认证必须通过；给出读取引用时必须真实读到。
	ok := view.NetworkReachable && view.AuthenticationAccepted && (readReference == "" || view.ReadVerified)
	if !ok {
		return RegistryView{}, view, false, nil
	}
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var latest registryRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&latest, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		if latest.Revision != view.Revision || latest.CredentialVersion != view.CredentialVersion {
			return authorization.ErrConflict
		}
		now := time.Now().UTC()
		if err := tx.Model(&latest).Updates(map[string]any{"verified_at": now, "verified_revision": latest.Revision}).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "instance.registry.verify", "registry", id, nil, nil)
	})
	if err != nil {
		return RegistryView{}, view, false, err
	}
	registry, err := s.GetRegistry(ctx, p, id)
	if err != nil {
		return RegistryView{}, view, true, err
	}
	return registry, view, true, nil
}
