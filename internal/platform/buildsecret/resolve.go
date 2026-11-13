package buildsecret

import (
	"fmt"
	"sort"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Resolve 由新构建受理事务调用，先完成用户BuildWrite与所有资源归属授权。
// SHARE 行锁与停用/轮换的UPDATE锁串行，禁用先提交时不得冻结为新构建输入。
// Version=0表示冻结当前版本；显式旧版本必须仍存在且密钥整体未禁用。
func (s *Service) Resolve(tx *gorm.DB, space, app string, requested []Ref) ([]Ref, error) {
	if len(requested) > 32 {
		return nil, authorization.ErrInvalid
	}
	ordered := append([]Ref(nil), requested...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SecretID < ordered[j].SecretID })
	result := make([]Ref, 0, len(ordered))
	seen := make(map[string]bool, len(ordered))
	for _, ref := range ordered {
		if ref.SecretID == "" || ref.Version < 0 || seen[ref.SecretID] {
			return nil, authorization.ErrInvalid
		}
		seen[ref.SecretID] = true
		var secret secretRecord
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&secret, "workspace_id = ? AND application_id = ? AND id = ? AND status = 'ACTIVE'", space, app, ref.SecretID).Error; err != nil {
			return nil, mapError(err)
		}
		version := ref.Version
		if version == 0 {
			version = secret.CurrentVersion
		}
		var exists int64
		if err := tx.Model(&versionRecord{}).Where("workspace_id = ? AND application_id = ? AND secret_id = ? AND version = ?", space, app, secret.ID, version).Count(&exists).Error; err != nil {
			return nil, err
		}
		if exists != 1 {
			return nil, authorization.ErrNotFound
		}
		if ref.Name != "" && ref.Name != secret.Name {
			return nil, authorization.ErrInvalid
		}
		result = append(result, Ref{SecretID: secret.ID, Version: version, Name: secret.Name})
	}
	return result, nil
}

// ReadVersion 只用于已授权且已冻结的任务输入，不是普通用户读取接口。
// 不检查当前ACTIVE：轮换或禁用不抹除已开始构建的历史引用；取消由构建状态机决定。
func (s *Service) ReadVersion(tx *gorm.DB, space, app string, ref Ref) (Material, error) {
	if ref.SecretID == "" || ref.Version < 1 {
		return Material{}, authorization.ErrInvalid
	}
	var secret secretRecord
	if err := tx.First(&secret, "workspace_id = ? AND application_id = ? AND id = ?", space, app, ref.SecretID).Error; err != nil {
		return Material{}, mapError(err)
	}
	if ref.Name != "" && ref.Name != secret.Name {
		return Material{}, authorization.ErrInvalid
	}
	var value versionRecord
	if err := tx.First(&value, "workspace_id = ? AND application_id = ? AND secret_id = ? AND version = ?", space, app, ref.SecretID, ref.Version).Error; err != nil {
		return Material{}, mapError(err)
	}
	plaintext, err := s.box.Decrypt(value.EncryptedValue, aad(space, app, ref.SecretID, ref.Version))
	if err != nil {
		return Material{}, fmt.Errorf("构建密钥解密失败")
	}
	return Material{Name: secret.Name, Value: plaintext}, nil
}
