// Package deployment 实现控制面持久化发布状态机；逻辑测试不构成真实 k3s/HTTPS 验收。
package deployment

import (
	"context"
	"errors"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	"gorm.io/gorm"
)

const (
	Deploy   = "DEPLOY"
	Rollback = "ROLLBACK"
	Recovery = "RECOVERY"
	Restart  = "RESTART"
	Stop     = "STOP"
	Start    = "START"
	// Reconcile 是控制面合成的观察恢复命令：worker 重启丢失内存中的运行采集器
	// （运行日志/指标）后，按观察停更的 serving release 下发；worker 不动集群
	// 资源，只重建观察循环与采集器（见 workerexec.Deploy 与 Service.claimResume）。
	Reconcile        = "RECONCILE"
	Queued           = "QUEUED"
	Applying         = "APPLYING"
	WaitingReady     = "WAITING_READY"
	VerifyingIngress = "VERIFYING_INGRESS"
	Reconciling      = "RECONCILING"
	Succeeded        = "SUCCEEDED"
	Failed           = "FAILED"
	Superseded       = "SUPERSEDED"
	Run              = "RUN"
	Stopped          = "STOPPED"
	Unknown          = "UNKNOWN"
	Observe          = "OBSERVE"
)

var (
	ErrNotReady         = errors.New("部署所需平台资源尚未就绪")
	ErrVerification     = errors.New("发布镜像或历史凭据不可用")
	ErrStaleObservation = errors.New("运行观察已被新操作取代")
	ErrSequence         = errors.New("运行观察序号不连续或内容冲突")
	ErrNoPending        = errors.New("没有可领取的运行操作")
)

// Expected 同时保护运行意图、当前服务版本和最新草稿，空 serving 表示从未成功发布。
type Expected struct {
	Generation       int64  `json:"generation"`
	ServingReleaseID string `json:"servingReleaseId"`
	ConfigRevision   int64  `json:"configRevision"`
}

type CreateInput struct {
	ArtifactID             string   `json:"artifactId"`
	RuntimeConfigRevision  int64    `json:"runtimeConfigRevision"`
	ExpectedPolicyRevision int64    `json:"expectedPolicyRevision"`
	Expected               Expected `json:"expected"`
	IdempotencyKey         string   `json:"idempotencyKey"`
}

type ChangeInput struct {
	Expected       Expected `json:"expected"`
	IdempotencyKey string   `json:"idempotencyKey"`
}

type RollbackInput struct {
	TargetReleaseID        string   `json:"targetReleaseId"`
	ExpectedPolicyRevision int64    `json:"expectedPolicyRevision"`
	Expected               Expected `json:"expected"`
	IdempotencyKey         string   `json:"idempotencyKey"`
}

type Placement struct {
	WorkspaceID    string    `json:"workspaceId"`
	ApplicationID  string    `json:"applicationId"`
	InstanceID     string    `json:"instanceId"`
	NodeID         string    `json:"nodeId"`
	WorkerID       string    `json:"workerId,omitempty"`
	ClusterNodeUID string    `json:"clusterNodeUid,omitempty"`
	PolicyRevision int64     `json:"policyRevision"`
	RuntimeType    string    `json:"runtimeType"`
	DefaultDomain  string    `json:"defaultDomain"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (Placement) TableName() string { return "application_placements" }

type ArtifactSnapshot struct {
	ID                string `json:"id"`
	WorkspaceID       string `json:"workspaceId"`
	ApplicationID     string `json:"applicationId"`
	RegistryID        string `json:"registryId"`
	CredentialVersion int64  `json:"credentialVersion"`
	Repository        string `json:"repository"`
	Digest            string `json:"digest"`
	Platform          string `json:"platform"`
}

// Verifier 独立检查仓库 manifest/config 和冻结凭据可读性，调用始终在业务事务外。
// nil 校验器失败关闭；fake 仅用于状态机测试，不证明真实镜像存在。
type Verifier interface {
	Verify(context.Context, ArtifactSnapshot) error
}
type RuntimeConfigs interface {
	MaterializeForDeployment(*gorm.DB, string, string, int64) (runtimeconfig.Materialized, error)
}

type ReleaseView struct {
	ID                    string           `json:"id"`
	WorkspaceID           string           `json:"workspaceId"`
	ApplicationID         string           `json:"applicationId"`
	Artifact              ArtifactSnapshot `json:"artifact"`
	RuntimeConfigRevision int64            `json:"runtimeConfigRevision"`
	Placement             Placement        `json:"placement"`
	Domains               []string         `json:"domains"`
	PreviousServingID     string           `json:"previousServingId"`
	RollbackOf            string           `json:"rollbackOf"`
	Source                string           `json:"source"`
	ActorID               string           `json:"actorId"`
	CreatedAt             time.Time        `json:"createdAt"`
}

type Operation struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspaceId"`
	ApplicationID string     `json:"applicationId"`
	ReleaseID     string     `json:"releaseId"`
	Generation    int64      `json:"generation"`
	Action        string     `json:"action"`
	State         string     `json:"state"`
	Active        bool       `json:"active"`
	ActorID       string     `json:"actorId"`
	ErrorCode     string     `json:"errorCode"`
	RecoveryOf    string     `json:"recoveryOf"`
	CreatedAt     time.Time  `json:"createdAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

type Result struct {
	Release   ReleaseView `json:"release"`
	Operation Operation   `json:"operation"`
}

type Status struct {
	WorkspaceID        string     `json:"workspaceId"`
	ApplicationID      string     `json:"applicationId"`
	Generation         int64      `json:"generation"`
	DesiredState       string     `json:"desiredState"`
	DesiredReleaseID   string     `json:"desiredReleaseId"`
	ServingReleaseID   string     `json:"servingReleaseId"`
	ServingGeneration  int64      `json:"servingGeneration"`
	ObservedGeneration int64      `json:"observedGeneration"`
	ObservedState      string     `json:"observedState"`
	ActiveOperationID  string     `json:"activeOperationId"`
	LastObservedAt     *time.Time `json:"lastObservedAt,omitempty"`
}

type Page struct {
	Items      []Result `json:"items"`
	NextCursor string   `json:"nextCursor"`
}

// WorkerIdentity 只由已验证 mTLS 的内部网关构造，事件正文不得自行指定。
type WorkerIdentity struct{ NodeID, WorkerID, CertificateSerial, CertificateSHA256 string }

// DispatchTLS 是发布领取时刻汇聚的入口 TLS 事实：绑定属实例级运行设施，
// 只在下发命令时取 application_domain_claims 的当前值，不冻结进 release。
// HTTPS=true 且 SecretName 非空仅当全部域名绑定同一非空 Secret；存在未绑定
// 或绑定不一致时为 nil，入口探测将因缺少 TLS 证据无法收敛（失败关闭，不猜证书）。
type DispatchTLS struct {
	HTTPS      bool
	SecretName string
}

// Dispatch 含解密运行配置，只允许发送给冻结节点的认证 worker，不能作为公共 DTO。
type Dispatch struct {
	WorkspaceID, ApplicationID, OperationID, ReleaseID string
	// SpaceSlug/AppName 空间标识与应用名（统一命名规范）：worker 据此派生
	// 运行命名空间 prod-<space_slug> 与 Deployment/Service 名称 <应用名>-default。
	SpaceSlug, AppName string
	Action             string
	Generation         int64
	NextEventSeq       int64
	RestartMarker      string
	Placement          Placement
	Artifact           ArtifactSnapshot
	Config             runtimeconfig.Materialized
	Domains            []string
	TLS                *DispatchTLS
}

// Observation 只记录 Kubernetes 证据，不接收任意日志、错误详情或探测 URL。
// PodUID/DeploymentUID 为空时不能证明成功；LOST 只是 UNKNOWN，绝非 STOPPED。
type Observation struct {
	WorkspaceID       string    `json:"workspaceId"`
	ApplicationID     string    `json:"applicationId"`
	OperationID       string    `json:"operationId"`
	ReleaseID         string    `json:"releaseId"`
	Generation        int64     `json:"generation"`
	Seq               int64     `json:"seq"`
	ClusterNodeUID    string    `json:"clusterNodeUid"`
	DeploymentUID     string    `json:"deploymentUid"`
	PodUID            string    `json:"podUid"`
	Digest            string    `json:"digest"`
	CorrectGeneration bool      `json:"correctGeneration"`
	ReadyReplicas     int32     `json:"readyReplicas"`
	Replicas          int32     `json:"replicas"`
	OldPodsGone       bool      `json:"oldPodsGone"`
	ServiceEndpoints  bool      `json:"serviceEndpoints"`
	Kind              string    `json:"kind"`
	ErrorCode         string    `json:"errorCode"`
	CollectedAt       time.Time `json:"collectedAt"`
}

type Ack struct {
	Seq       int64
	Duplicate bool
	State     string
}

// ProbeEvidence 仅供控制面探测器调用：目标必须先从 ProbeTargets 取得，外部请求无此入口。
type ProbeEvidence struct {
	WorkspaceID, ApplicationID, OperationID string
	Generation                              int64
	Domain, Path, PodUID                    string
	TLSVerified, Healthy                    bool
	ObservedAt                              time.Time
}

type ProbeTarget struct {
	WorkspaceID, ApplicationID, OperationID, ReleaseID, Domain, Path, PodUID string
	Generation                                                               int64
}

// RuntimeLogInput 是一个容器运行日志块；Content 为 worker 上报的原始字节分片。
// 时间不由 worker 自报，落库时间取控制面数据库时钟。
type RuntimeLogInput struct {
	WorkspaceID, ApplicationID, OperationID, ReleaseID string
	Generation                                         int64
	PodUID, Container                                  string
	RestartCount                                       int32
	Seq                                                int64
	Content                                            []byte
	Truncated                                          bool
}

// RuntimeMetricInput 是一次 CPU/内存采样；CollectedAt 由 worker 上报，
// 窗口校验只与数据库时钟比较，幂等键为 (pod_uid, collected_at)。
type RuntimeMetricInput struct {
	WorkspaceID, ApplicationID, ReleaseID string
	Generation                            int64
	PodUID                                string
	CPUNanoCores, CPUWindowMillis         int64
	MemoryWorkingSetBytes                 int64
	CollectedAt                           time.Time
}

// RuntimeLog 是运行日志块的对外只读投影。
type RuntimeLog struct {
	ID, WorkspaceID, ApplicationID, PodUID, Container string
	RestartCount                                      int32
	Seq                                               int64
	Content                                           []byte
	Truncated                                         bool
	CreatedAt                                         time.Time
}

// RuntimeMetricSample 是基础监控采样点的对外只读投影。
type RuntimeMetricSample struct {
	ID, WorkspaceID, ApplicationID, ReleaseID, PodUID    string
	Generation                                           int64
	CPUNanoCores, CPUWindowMillis, MemoryWorkingSetBytes int64
	CollectedAt                                          time.Time
}

// RuntimeLogPage 按 (created_at, id) 时间正序分页返回运行日志块。
type RuntimeLogPage struct {
	Items      []RuntimeLog `json:"items"`
	NextCursor string       `json:"nextCursor"`
}
