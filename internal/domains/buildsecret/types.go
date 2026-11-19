// Package buildsecret 管理应用内的构建密钥；公开接口只返回元数据。
package buildsecret

import "time"

type CreateInput struct{ Name, Purpose, Value string }
type ReplaceInput struct {
	ExpectedVersion int64
	Value           string
}
type View struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Purpose        string    `json:"purpose"`
	Status         string    `json:"status"`
	CurrentVersion int64     `json:"currentVersion"`
	Configured     bool      `json:"configured"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}
type Page struct {
	Items      []View `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Ref 是不可变构建输入，Name由服务端从SecretID解析，不允许客户端覆盖挂载名称。
type Ref struct {
	SecretID string `json:"secretId"`
	Version  int64  `json:"version"`
	Name     string `json:"name"`
}

// Material 仅用于已授权构建的受限worker执行传输，不能序列化到公共API或日志。
type Material struct {
	Name  string `json:"-"`
	Value []byte `json:"-"`
}

func (m *Material) Clear() { clear(m.Value); m.Value = nil }

type secretRecord struct {
	ID             string    `gorm:"column:id;primaryKey"`
	WorkspaceID    string    `gorm:"column:workspace_id"`
	ApplicationID  string    `gorm:"column:application_id"`
	Name           string    `gorm:"column:name"`
	Purpose        string    `gorm:"column:purpose"`
	Status         string    `gorm:"column:status"`
	CurrentVersion int64     `gorm:"column:current_version"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
	CreatedBy      string    `gorm:"column:created_by"`
	UpdatedBy      string    `gorm:"column:updated_by"`
}

func (secretRecord) TableName() string { return "application_build_secrets" }

type versionRecord struct {
	ID             string    `gorm:"column:id;primaryKey"`
	SecretID       string    `gorm:"column:secret_id"`
	Version        int64     `gorm:"column:version"`
	WorkspaceID    string    `gorm:"column:workspace_id"`
	ApplicationID  string    `gorm:"column:application_id"`
	EncryptedValue []byte    `gorm:"column:encrypted_value" json:"-"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
	CreatedBy      string    `gorm:"column:created_by"`
	UpdatedBy      string    `gorm:"column:updated_by"`
}

func (versionRecord) IDPrefix() string  { return "bsv" }
func (versionRecord) TableName() string { return "application_build_secret_versions" }
