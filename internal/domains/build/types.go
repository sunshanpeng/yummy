// Package build 实现控制面内的持久化构建内核，不启动 worker 或网络服务。
package build

import (
	"context"
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
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
	// 与 worker 执行内核 internal/infra/workerexec 的 maxLogChunk 保持一致（同一协议上限的两端）。
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

// 构建方式（2026-11-23 自动选择）：由受理时验证过的源码快照决定并随快照冻结。
const (
	// MethodDockerfile 使用源码中的 Dockerfile 交给 BuildKit。
	MethodDockerfile = "DOCKERFILE"
	// MethodRailpack 源码没有 Dockerfile 时由 Railpack 生成计划，再交给同一套 BuildKit 执行。
	MethodRailpack = "RAILPACK"
)

// Config 的 Dockerfile 为空表示不显式指定：保存的应用配置与单次构建覆盖都允许为空，
// 受理时按上下文根目录自动选择。Method 只在受理冻结后的配置快照中出现，保存配置时忽略。
// 冻结后 Dockerfile 为实际使用的文件路径（相对源码归档根），Railpack 构建为空。
type Config struct {
	Dockerfile string            `json:"dockerfile"`
	Context    string            `json:"context"`
	Method     string            `json:"method,omitempty"`
	Target     string            `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	SecretRefs []SecretRef       `json:"secretRefs,omitempty"`
}

// EffectiveMethod 返回快照的构建方式；方式字段出现之前冻结的快照只支持 Dockerfile。
func (c Config) EffectiveMethod() string {
	if c.Method == "" {
		return MethodDockerfile
	}
	return c.Method
}

type Overrides struct {
	Dockerfile *string           `json:"dockerfile,omitempty"`
	Context    *string           `json:"context,omitempty"`
	Target     *string           `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	SecretRefs []SecretRef       `json:"secretRefs,omitempty"`
}
type CreateInput struct {
	SourceSnapshotID          string    `json:"sourceSnapshotId"`
	ConfigRevision            int64     `json:"configRevision"`
	ExpectedPolicyRevision    int64     `json:"expectedPolicyRevision"`
	Overrides                 Overrides `json:"overrides"`
	IdempotencyKey            string    `json:"idempotencyKey"`
	AutoDeploy                bool      `json:"autoDeploy"`
	RuntimeConfigRevision     int64     `json:"runtimeConfigRevision"`
	ExpectedServingRelease    string    `json:"expectedServingRelease"`
	ExpectedRuntimeGeneration *int64    `json:"expectedRuntimeGeneration,omitempty"`
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
	// SpaceSlug 空间标识：worker 据此派生按空间的构建/运行命名空间（2026-10-26 设计）。
	SpaceSlug                         string
	NodeID, WorkerID, JobName, JobUID string
	Command                           string
	SourceSnapshotID, SourceSHA256    string
	Config                            Config
	RegistryID                        string
	CredentialVersion                 int64
	Repository, Tag, Platform         string
	Execution                         inventory.ExecutionSummary
	DeadlineAt                        time.Time
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
