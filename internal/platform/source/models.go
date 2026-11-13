// Package source 提供应用范围的源码上传、校验和不可变快照服务。
// 持久化模型仅在本模块内部使用，公共 DTO 不包含宿主存储路径。
package source

import (
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/sourcearchive"
)

const (
	StatePending  = "PENDING"
	StateUploaded = "UPLOADED"
	StateComplete = "COMPLETE"
	StateReady    = "READY"
	StateExpired  = "EXPIRED"
)

type uploadRecord struct {
	ID             string `gorm:"primaryKey"`
	WorkspaceID    string
	ApplicationID  string
	CreatedBy      string
	ExpectedSize   int64
	ExpectedSHA256 string
	Dockerfile     string
	State          string
	StorageKey     string
	SnapshotID     *string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	UploadedAt     *time.Time
	CompletedAt    *time.Time
}

func (uploadRecord) TableName() string { return "source_uploads" }

type snapshotRecord struct {
	ID              string `gorm:"primaryKey"`
	WorkspaceID     string
	ApplicationID   string
	UploadID        string
	CreatedBy       string
	State           string
	StorageKey      string
	SHA256          string
	CompressedBytes int64
	ExpandedBytes   int64
	FileBytes       int64
	EntryCount      int64
	Dockerfile      string
	Entries         []sourcearchive.Entry `gorm:"serializer:json;type:jsonb"`
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

func (snapshotRecord) TableName() string { return "source_snapshots" }
