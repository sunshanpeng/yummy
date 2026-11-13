// Package build 实现控制面内的持久化构建内核，不启动 worker 或网络服务。
package build

import (
	"context"
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
)

const (
	Queued      = "QUEUED"
	Starting    = "STARTING"
	Running     = "RUNNING"
	Canceling   = "CANCELING"
	Reconciling = "RECONCILING"
	Succeeded   = "SUCCEEDED"
	Failed      = "FAILED"
	Canceled    = "CANCELED"
	TimedOut    = "TIMED_OUT"
	Lost        = "LOST"
	Platform    = "linux/amd64"
	MaxLogChunk = 16 << 10
	MaxLogBytes = 20 << 20
)

var (
	ErrSourceExpired = errors.New("源码快照已过期")
	ErrSourceInvalid = errors.New("源码快照不可构建")
	ErrSequence      = errors.New("事件或日志序号不连续")
	ErrNoCapacity    = errors.New("没有可领取的构建或节点容量不足")
	ErrVerification  = errors.New("产物独立验证失败")
)

// BuildResolver 可由 inventory.Service 实现；此预览不能替代受理事务内的重新读取。
type BuildResolver interface {
	ResolveBuild(context.Context, authorization.Principal, string, string, int64) (inventory.ExecutionSummary, error)
}
type SecretRef struct {
	SecretID string `json:"secretId"`
	Name     string `json:"name"`
	Version  int64  `json:"version"`
}
type Config struct {
	Dockerfile string            `json:"dockerfile"`
	Context    string            `json:"context"`
	Target     string            `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	SecretRefs []SecretRef       `json:"secretRefs,omitempty"`
}
type Overrides struct {
	Dockerfile *string           `json:"dockerfile,omitempty"`
	Context    *string           `json:"context,omitempty"`
	Target     *string           `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	SecretRefs []SecretRef       `json:"secretRefs,omitempty"`
}
type CreateInput struct {
	SourceSnapshotID       string    `json:"sourceSnapshotId"`
	ConfigRevision         int64     `json:"configRevision"`
	ExpectedPolicyRevision int64     `json:"expectedPolicyRevision"`
	Overrides              Overrides `json:"overrides"`
	IdempotencyKey         string    `json:"idempotencyKey"`
	AutoDeploy             bool      `json:"autoDeploy"`
	RuntimeConfigRevision  int64     `json:"runtimeConfigRevision"`
	ExpectedServingRelease string    `json:"expectedServingRelease"`
}
type Preview struct {
	SourceSnapshotID      string                     `json:"sourceSnapshotId"`
	SourceSHA256          string                     `json:"sourceSha256"`
	ConfigRevision        int64                      `json:"configRevision"`
	Config                Config                     `json:"config"`
	Execution             inventory.ExecutionSummary `json:"execution"`
	AutoDeploy            bool                       `json:"autoDeploy"`
	RuntimeConfigRevision *int64                     `json:"runtimeConfigRevision,omitempty"`
}
type Artifact struct {
	ID                string     `json:"id"`
	RegistryID        string     `json:"registryId"`
	CredentialVersion int64      `json:"credentialVersion"`
	Repository        string     `json:"repository"`
	Tag               string     `json:"tag"`
	Digest            string     `json:"digest"`
	Platform          string     `json:"platform"`
	State             string     `json:"state"`
	VerifiedAt        *time.Time `json:"verifiedAt,omitempty"`
}
type View struct {
	ID                     string     `json:"id"`
	WorkspaceID            string     `json:"workspaceId"`
	ApplicationID          string     `json:"applicationId"`
	ActorID                string     `json:"actorId"`
	SourceSnapshotID       string     `json:"sourceSnapshotId"`
	ConfigRevision         int64      `json:"configRevision"`
	Config                 Config     `json:"config"`
	PolicyRevision         int64      `json:"policyRevision"`
	NodeID                 string     `json:"nodeId"`
	Repository             string     `json:"repository"`
	Platform               string     `json:"platform"`
	State                  string     `json:"state"`
	Stage                  string     `json:"stage"`
	DesiredState           string     `json:"desiredState"`
	ErrorCode              string     `json:"errorCode,omitempty"`
	QueueReason            string     `json:"queueReason,omitempty"`
	Version                int64      `json:"version"`
	AutoDeploy             bool       `json:"autoDeploy"`
	RuntimeConfigRevision  *int64     `json:"runtimeConfigRevision,omitempty"`
	ExpectedServingRelease string     `json:"expectedServingRelease"`
	RetryOf                *string    `json:"retryOf,omitempty"`
	LogsComplete           bool       `json:"logsComplete"`
	LogsTruncated          bool       `json:"logsTruncated"`
	CreatedAt              time.Time  `json:"createdAt"`
	FinishedAt             *time.Time `json:"finishedAt,omitempty"`
	Artifact               *Artifact  `json:"artifact,omitempty"`
}
type Page struct {
	Items      []View `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// WorkerIdentity 只能由已验证 mTLS 的内部网关构造，不能从业务请求正文获得。
// 每次调用仍复核登记证书和数据库分配；不保存长连接内存授权缓存。
type WorkerIdentity struct{ NodeID, WorkerID, CertificateSerial string }
type Dispatch struct {
	BuildID, AttemptID, WorkspaceID, ApplicationID string
	NodeID, WorkerID, JobName, JobUID              string
	Command                                        string
	SourceSnapshotID, SourceSHA256                 string
	Config                                         Config
	RegistryID                                     string
	CredentialVersion                              int64
	Repository, Tag, Platform                      string
	Execution                                      inventory.ExecutionSummary
	DeadlineAt                                     time.Time
}
type Event struct {
	AttemptID    string `json:"attemptId"`
	Seq          int64  `json:"seq"`
	Kind         string `json:"kind"`
	JobUID       string `json:"jobUid"`
	Stage        string `json:"stage,omitempty"`
	Digest       string `json:"digest,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	LogsComplete bool   `json:"logsComplete"`
}
type Ack struct {
	Seq       int64
	Duplicate bool
	State     string
}
type LogInput struct {
	AttemptID string
	Seq       int64
	Content   []byte
}
type LogChunk struct {
	Seq       int64     `json:"seq"`
	Content   string    `json:"content"`
	Truncated bool      `json:"truncated"`
	CreatedAt time.Time `json:"createdAt"`
}
type Logs struct {
	Items     []LogChunk `json:"items"`
	NextSeq   int64      `json:"nextSeq"`
	Complete  bool       `json:"complete"`
	Truncated bool       `json:"truncated"`
}

type VerificationRequest struct {
	BuildID, ArtifactID, RegistryID   string
	CredentialVersion                 int64
	Repository, Tag, Digest, Platform string
}
type VerificationResult struct{ Repository, Digest, Platform string }

// ArtifactVerifier 在事务外独立读取 registry manifest/config，不信任 worker 的成功结论。
// 网络暂时不可用应返回普通 error，明确不合法返回 ErrVerification，允许扫描器安全重试。
type ArtifactVerifier interface {
	Verify(context.Context, VerificationRequest) (VerificationResult, error)
}
type Dispatcher interface {
	Dispatch(context.Context, Dispatch) error
}

func terminal(state string) bool {
	switch state {
	case Succeeded, Failed, Canceled, TimedOut, Lost:
		return true
	}
	return false
}
