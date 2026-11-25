package source

import (
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

var (
	ErrExpired     = errors.New("源码或上传会话已过期")
	ErrNotReady    = errors.New("源码尚未验证就绪")
	ErrContent     = errors.New("上传内容大小或摘要不匹配")
	ErrUnavailable = errors.New("源码存储不可用，请重新上传")
)

const (
	UploadLifetime    = 10 * time.Minute
	SnapshotRetention = 7 * 24 * time.Hour
)

// Config.Root 必须是显式创建的持久目录，服务不在节点根盘上选择隐式默认值。
// Limits 可收紧默认限制，但不可扩大首期认证边界。
type Config struct {
	Root   string
	Limits sourcearchive.Limits
}

type CreateInput struct {
	Size   int64
	SHA256 string
	// Dockerfile 相对上传归档的根，不接受宿主路径；空值表示不显式指定，不要求归档含
	// Dockerfile（纯源码由构建受理时交给 Railpack）。显式路径必须在归档中存在。
	Dockerfile string
}

type Upload struct {
	ID             string
	WorkspaceID    string
	ApplicationID  string
	State          string
	ExpectedSize   int64
	ExpectedSHA256 string
	Dockerfile     string
	ExpiresAt      time.Time
	SnapshotID     string
}

type Snapshot struct {
	ID              string
	WorkspaceID     string
	ApplicationID   string
	State           string
	SHA256          string
	CompressedBytes int64
	ExpandedBytes   int64
	FileBytes       int64
	EntryCount      int64
	Dockerfile      string
	Entries         []sourcearchive.Entry
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

func uploadDTO(value uploadRecord) Upload {
	id := ""
	if value.SnapshotID != nil {
		id = *value.SnapshotID
	}
	return Upload{ID: value.ID, WorkspaceID: value.WorkspaceID, ApplicationID: value.ApplicationID, State: value.State, ExpectedSize: value.ExpectedSize, ExpectedSHA256: value.ExpectedSHA256, Dockerfile: value.Dockerfile, ExpiresAt: value.ExpiresAt, SnapshotID: id}
}

func snapshotDTO(value snapshotRecord, now time.Time) Snapshot {
	state := value.State
	if !value.ExpiresAt.After(now) {
		state = StateExpired
	}
	return Snapshot{ID: value.ID, WorkspaceID: value.WorkspaceID, ApplicationID: value.ApplicationID, State: state, SHA256: value.SHA256, CompressedBytes: value.CompressedBytes, ExpandedBytes: value.ExpandedBytes, FileBytes: value.FileBytes, EntryCount: value.EntryCount, Dockerfile: value.Dockerfile, Entries: append([]sourcearchive.Entry(nil), value.Entries...), CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt}
}
