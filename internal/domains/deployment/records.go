package deployment

import (
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
)

type releaseRecord struct {
	ID                                     string `gorm:"column:id"`
	WorkspaceID, ApplicationID, ArtifactID string
	RuntimeConfigRevision                  int64
	RegistryID                             string
	CredentialVersion                      int64
	Repository, Digest, Platform           string
	PlacementSnapshot                      Placement `gorm:"serializer:json;type:jsonb"`
	Domains                                []string  `gorm:"serializer:json;type:jsonb"`
	PreviousServingID, RollbackOf          *string
	Source                                 string
	AutoRecoveryEnabled                    bool
	ActorID                                string
	CreatedAt                              time.Time
}

func (releaseRecord) TableName() string { return "application_releases" }

type operationRecord struct {
	ID                                              string `gorm:"column:id"`
	WorkspaceID, ApplicationID, ReleaseID           string
	Generation                                      int64
	Action, State                                   string
	Active                                          bool
	ActorID, IdempotencyKey, Fingerprint, ErrorCode string
	RecoveryOf                                      *string
	EventSeq                                        int64
	DeploymentUID, PodUID                           string
	KubernetesReady                                 bool
	LastObservedAt                                  *time.Time
	NextDispatchAt                                  time.Time
	NextObservationAt                               time.Time
	DispatchCount                                   int
	CreatedAt                                       time.Time
	FinishedAt                                      *time.Time
}

func (operationRecord) TableName() string { return "deployment_operations" }

type stateRecord struct {
	ID                                                    string `gorm:"column:id;primaryKey"`
	ApplicationID                                         string `gorm:"column:application_id"`
	WorkspaceID                                           string
	Generation                                            int64
	DesiredState                                          string
	DesiredReleaseID, ServingReleaseID, ActiveOperationID *string
	ServingGeneration, ObservedGeneration                 int64
	ObservedState, DeploymentUID, PodUID                  string
	LastObservedAt                                        *time.Time
	UpdatedAt                                             time.Time
	CreatedAt                                             time.Time `gorm:"column:created_at"`
	CreatedBy                                             string    `gorm:"column:created_by"`
	UpdatedBy                                             string    `gorm:"column:updated_by"`
}

func (stateRecord) IDPrefix() string  { return "ars" }
func (stateRecord) TableName() string { return "application_runtime_states" }

type domainRecord struct {
	ID            string `gorm:"column:id;primaryKey"`
	Domain        string `gorm:"column:domain"`
	WorkspaceID   string
	ApplicationID string
	IsDefault     bool
	// TLSSecretName 是全局管理员预置的入口证书 Secret，空串表示未绑定；
	// 每次发布领取读取当前值汇聚，不写入发布快照。
	TLSSecretName string
	CreatedAt     time.Time
}

func (domainRecord) IDPrefix() string  { return "dcl" }
func (domainRecord) TableName() string { return "application_domain_claims" }

type observationRecord struct {
	ID                                               string `gorm:"column:id;primaryKey"`
	WorkspaceID, ApplicationID, OperationID          string
	Seq                                              int64
	NodeID, WorkerID, CertificateSerial, PayloadHash string
	Payload                                          Observation `gorm:"serializer:json;type:jsonb"`
	CollectedAt, CreatedAt                           time.Time
}

func (observationRecord) IDPrefix() string  { return "rob" }
func (observationRecord) TableName() string { return "runtime_observations" }

type probeRecord struct {
	ID                                      string `gorm:"column:id"`
	WorkspaceID, ApplicationID, OperationID string
	Generation                              int64
	Domain, Path, PodUID                    string
	TLSVerified, Healthy                    bool
	ObservedAt                              time.Time
}

func (probeRecord) TableName() string { return "deployment_probe_evidence" }

// runtimeLogChunkRecord 是应用运行日志块的持久化记录；内容上限由数据库 CHECK 约束兜底。
type runtimeLogChunkRecord struct {
	ReleaseID                                     *string `gorm:"column:release_id"`
	ID                                            string  `gorm:"column:id"`
	WorkspaceID, ApplicationID, PodUID, Container string
	RestartCount                                  int32
	Seq                                           int64
	PayloadHash                                   string
	Content                                       []byte
	Truncated                                     bool
	CreatedAt                                     time.Time `gorm:"->"` // 落库时间由数据库时钟默认值写入，不从宿主机注入
}

func (runtimeLogChunkRecord) TableName() string { return "runtime_log_chunks" }

// runtimeMetricSampleRecord 是 CPU/内存采样的持久化记录；collected_at 来自 worker 上报。
type runtimeMetricSampleRecord struct {
	ID                                                   string `gorm:"column:id"`
	WorkspaceID, ApplicationID, ReleaseID, PodUID        string
	Generation                                           int64
	CPUNanoCores, CPUWindowMillis, MemoryWorkingSetBytes int64
	PayloadHash                                          string
	CollectedAt                                          time.Time
	CreatedAt                                            time.Time `gorm:"->"` // 同上，数据库时钟默认值
}

func (runtimeMetricSampleRecord) TableName() string { return "runtime_metric_samples" }

type settingsRecord struct {
	ID             string
	PolicyRevision int64
}

func (settingsRecord) TableName() string { return "instance_settings" }

type policyRecord struct {
	ID                        string `gorm:"column:id;primaryKey"`
	InstanceID                string
	Revision                  int64
	RuntimeNodeID, BaseDomain string
	ResourceLimits            inventory.Resources `gorm:"serializer:json;type:jsonb"`
	AutoRecoveryEnabled       bool
	CreatedAt                 time.Time `gorm:"column:created_at"`
	UpdatedAt                 time.Time `gorm:"column:updated_at"`
	CreatedBy                 string    `gorm:"column:created_by"`
	UpdatedBy                 string    `gorm:"column:updated_by"`
}

func (policyRecord) IDPrefix() string  { return "iep" }
func (policyRecord) TableName() string { return "instance_execution_policies" }

type nodeRecord struct {
	ID, InstanceID, WorkerID, Status, ClusterNodeUID, OS, Arch string
	RuntimeReady, IngressReady, MetricsReady                   bool
	LastSeen                                                   *time.Time
}

func (nodeRecord) TableName() string { return "execution_nodes" }

type allocationRecord struct {
	ID          string    `gorm:"column:id;primaryKey"`
	WorkspaceID string    `gorm:"column:workspace_id"`
	InstanceID  string    `gorm:"column:instance_id"`
	CPUMillis   int64     `gorm:"column:cpu_millis"`
	MemoryMiB   int64     `gorm:"column:memory_mib"`
	Revision    int64     `gorm:"column:revision"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	CreatedBy   string    `gorm:"column:created_by"`
	UpdatedBy   string    `gorm:"column:updated_by"`
}

func (allocationRecord) IDPrefix() string  { return "wal" }
func (allocationRecord) TableName() string { return "workspace_allocations" }

type certificateRecord struct {
	ID                                          string `gorm:"column:id;primaryKey"`
	Serial, NodeID, WorkerID, CertificateSHA256 string
	NotBefore, NotAfter                         time.Time
	RevokedAt                                   *time.Time
	CreatedAt                                   time.Time `gorm:"column:created_at"`
	UpdatedAt                                   time.Time `gorm:"column:updated_at"`
	CreatedBy                                   string    `gorm:"column:created_by"`
	UpdatedBy                                   string    `gorm:"column:updated_by"`
}

func (certificateRecord) IDPrefix() string  { return "wct" }
func (certificateRecord) TableName() string { return "worker_certificates" }

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// verifiedArtifactSnapshot 把构建域产物事实映射为发布域的产物快照（可比结构，
// 用于受理时冻结值与当前值的一致性比对）。
func verifiedArtifactSnapshot(v build.VerifiedArtifact) ArtifactSnapshot {
	return ArtifactSnapshot{ID: v.ID, WorkspaceID: v.WorkspaceID, ApplicationID: v.ApplicationID, RegistryID: v.RegistryID, CredentialVersion: v.CredentialVersion, Repository: v.Repository, Digest: v.Digest, Platform: v.Platform}
}
func releaseView(v releaseRecord) ReleaseView {
	// 持久快照和内部 Dispatch 保留 worker/集群身份，公共 DTO 只暴露业务可见节点摘要。
	placement := v.PlacementSnapshot
	placement.WorkerID, placement.ClusterNodeUID = "", ""
	return ReleaseView{ID: v.ID, WorkspaceID: v.WorkspaceID, ApplicationID: v.ApplicationID, Artifact: ArtifactSnapshot{ID: v.ArtifactID, WorkspaceID: v.WorkspaceID, ApplicationID: v.ApplicationID, RegistryID: v.RegistryID, CredentialVersion: v.CredentialVersion, Repository: v.Repository, Digest: v.Digest, Platform: v.Platform}, RuntimeConfigRevision: v.RuntimeConfigRevision, Placement: placement, Domains: v.Domains, PreviousServingID: text(v.PreviousServingID), RollbackOf: text(v.RollbackOf), Source: v.Source, ActorID: v.ActorID, CreatedAt: v.CreatedAt}
}
func operationView(v operationRecord) Operation {
	return Operation{ID: v.ID, WorkspaceID: v.WorkspaceID, ApplicationID: v.ApplicationID, ReleaseID: v.ReleaseID, Generation: v.Generation, Action: v.Action, State: v.State, Active: v.Active, ActorID: v.ActorID, ErrorCode: v.ErrorCode, RecoveryOf: text(v.RecoveryOf), CreatedAt: v.CreatedAt, FinishedAt: v.FinishedAt}
}
