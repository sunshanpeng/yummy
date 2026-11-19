package buildsecret

import "gorm.io/gorm"

// SecretReference 是构建引用的密钥版本（构建材料化时按 secret_id 稳定排序）。
type SecretReference struct {
	SecretID, Name string
	Version        int64
}

// SecretReferencesForBuild 读取一次构建引用的全部密钥版本。
// 【方言兼容】单表投影查询，标准 SQL。
func SecretReferencesForBuild(tx *gorm.DB, workspaceID, applicationID, buildID string) ([]SecretReference, error) {
	var refs []SecretReference
	err := tx.Table("build_secret_references").
		Select("secret_id,name,version").
		Where("workspace_id = ? AND application_id = ? AND build_id = ?", workspaceID, applicationID, buildID).
		Order("secret_id").Find(&refs).Error
	return refs, err
}
