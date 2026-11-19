// Package model 只定义数据库持久化结构；业务服务和公共接口不得直接返回这些对象。
package model

import "time"

const (
	UserActive   = "ACTIVE"
	UserDisabled = "DISABLED"

	ResourceActive   = "ACTIVE"
	ResourceArchived = "ARCHIVED"

	WorkspaceAdmin      = "admin"
	WorkspaceMemberRole = "member"

	ApplicationMaintainer = "maintainer"
	ApplicationDeveloper  = "developer"
	ApplicationViewer     = "viewer"
)

type User struct {
	ID            string    `gorm:"column:id;primaryKey"`
	Username      string    `gorm:"column:username"`
	PasswordHash  string    `gorm:"column:password_hash" json:"-"`
	Status        string    `gorm:"column:status;default:ACTIVE"`
	AuthVersion   int64     `gorm:"column:auth_version;default:1"`
	InstanceAdmin bool      `gorm:"column:instance_admin"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (User) TableName() string { return "users" }

type Workspace struct {
	ID          string    `gorm:"column:id;primaryKey"`
	Name        string    `gorm:"column:name"`
	Slug        string    `gorm:"column:slug"`
	Description string    `gorm:"column:description"`
	Status      string    `gorm:"column:status;default:ACTIVE"`
	Revision    int64     `gorm:"column:revision;default:1"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (Workspace) TableName() string { return "workspaces" }

type WorkspaceMember struct {
	WorkspaceID string    `gorm:"column:workspace_id;primaryKey"`
	UserID      string    `gorm:"column:user_id;primaryKey"`
	Role        string    `gorm:"column:role"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (WorkspaceMember) TableName() string { return "workspace_members" }

type Application struct {
	ID          string `gorm:"column:id;primaryKey"`
	WorkspaceID string `gorm:"column:workspace_id"`
	Name        string `gorm:"column:name"`
	// AddressSuffix 默认地址后缀（创建时生成且不变）：默认地址为
	// <应用名>-<后缀>.<应用根域名>（2026-10-26 设计）。
	AddressSuffix string    `gorm:"column:address_suffix"`
	Description   string    `gorm:"column:description"`
	Status        string    `gorm:"column:status;default:ACTIVE"`
	Revision      int64     `gorm:"column:revision;default:1"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (Application) TableName() string { return "applications" }

type ApplicationMember struct {
	WorkspaceID   string    `gorm:"column:workspace_id"`
	ApplicationID string    `gorm:"column:application_id;primaryKey"`
	UserID        string    `gorm:"column:user_id;primaryKey"`
	Role          string    `gorm:"column:role"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (ApplicationMember) TableName() string { return "application_members" }

type Invitation struct {
	ID          string     `gorm:"column:id;primaryKey"`
	WorkspaceID string     `gorm:"column:workspace_id"`
	Username    string     `gorm:"column:username"`
	Role        string     `gorm:"column:role"`
	TokenHash   string     `gorm:"column:token_hash" json:"-"`
	CreatedBy   string     `gorm:"column:created_by"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	ConsumedAt  *time.Time `gorm:"column:consumed_at"`
	ConsumedBy  *string    `gorm:"column:consumed_by"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

func (Invitation) TableName() string { return "invitations" }

type AccessToken struct {
	ID          string     `gorm:"column:id;primaryKey"`
	UserID      string     `gorm:"column:user_id"`
	Name        string     `gorm:"column:name"`
	TokenHash   string     `gorm:"column:token_hash" json:"-"`
	Scopes      []string   `gorm:"column:scopes;type:jsonb;serializer:json"`
	AuthVersion int64      `gorm:"column:auth_version"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	LastUsedAt  *time.Time `gorm:"column:last_used_at"`
}

func (AccessToken) TableName() string { return "access_tokens" }

// AccessTokenApplication 是授权上限，不代替每次调用时的用户角色和状态校验。
type AccessTokenApplication struct {
	TokenID       string `gorm:"column:token_id;primaryKey"`
	WorkspaceID   string `gorm:"column:workspace_id;primaryKey"`
	ApplicationID string `gorm:"column:application_id;primaryKey"`
}

func (AccessTokenApplication) TableName() string { return "access_token_applications" }

// AuditEvent 只记录可公开的操作事实，禁止放入密码、凭证明文或请求正文。
type AuditEvent struct {
	ID            string    `gorm:"column:id;primaryKey"`
	WorkspaceID   *string   `gorm:"column:workspace_id"`
	ApplicationID *string   `gorm:"column:application_id"`
	ActorID       string    `gorm:"column:actor_id"`
	Action        string    `gorm:"column:action"`
	ObjectType    string    `gorm:"column:object_type"`
	ObjectID      string    `gorm:"column:object_id"`
	Result        string    `gorm:"column:result"`
	TraceID       string    `gorm:"column:trace_id"`
	CreatedAt     time.Time `gorm:"column:created_at"`
}

func (AuditEvent) TableName() string { return "audit_events" }

type PasswordReset struct {
	ID         string     `gorm:"column:id;primaryKey"`
	UserID     string     `gorm:"column:user_id"`
	TokenHash  string     `gorm:"column:token_hash" json:"-"`
	ExpiresAt  time.Time  `gorm:"column:expires_at"`
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

func (PasswordReset) TableName() string { return "password_resets" }

// IdempotencyKey 与创建的业务资源在同一事务提交，禁止登记没有对应结果的占位行。
type IdempotencyKey struct {
	WorkspaceID string    `gorm:"column:workspace_id;primaryKey"`
	ActorID     string    `gorm:"column:actor_id;primaryKey"`
	Action      string    `gorm:"column:action;primaryKey"`
	Key         string    `gorm:"column:key;primaryKey"`
	Fingerprint string    `gorm:"column:fingerprint"`
	ResultID    string    `gorm:"column:result_id"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (IdempotencyKey) TableName() string { return "idempotency_keys" }
