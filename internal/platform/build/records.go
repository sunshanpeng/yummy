package build

import (
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/sourcearchive"
	"time"
)

type buildRecord struct {
	ID                                                             string `gorm:"primaryKey"`
	WorkspaceID, ApplicationID, ActorID                            string
	ActorAuthVersion                                               int64
	TokenID                                                        *string
	IdempotencyKey, Fingerprint, SourceSnapshotID, SourceSHA256    string
	ConfigRevision                                                 int64
	ConfigSnapshot                                                 Config `gorm:"serializer:json;type:jsonb"`
	InstanceID                                                     string
	PolicyRevision                                                 int64
	NodeID, RegistryID                                             string
	CredentialVersion                                              int64
	Repository, Tag, Platform                                      string
	ExecutionSnapshot                                              inventory.ExecutionSummary `gorm:"serializer:json;type:jsonb"`
	AutoDeploy                                                     bool
	RuntimeConfigRevision                                          *int64
	ExpectedServingRelease                                         string
	RetryOf                                                        *string
	State, Stage, DesiredState, StopReason, ErrorCode, QueueReason string
	Version, LogsBytes                                             int64
	LogsComplete, LogsTruncated                                    bool
	CreatedAt, UpdatedAt, DeadlineAt                               time.Time
	StartedAt, FinishedAt                                          *time.Time
}

func (buildRecord) TableName() string { return "builds" }

type attemptRecord struct {
	ID                                                                     string `gorm:"primaryKey"`
	WorkspaceID, ApplicationID, BuildID, NodeID, WorkerID, JobName, JobUID string
	Occupied                                                               bool
	EventSeq, LogSeq                                                       int64
	DispatchCount                                                          int
	NextDispatchAt                                                         time.Time
	LastEventAt, ReconcilingAt, StoppedAt                                  *time.Time
	CreatedAt                                                              time.Time
}

func (attemptRecord) TableName() string { return "build_attempts" }

type eventRecord struct {
	WorkspaceID, ApplicationID, BuildID, AttemptID string
	Seq                                            int64
	PayloadHash                                    string
	Payload                                        Event `gorm:"serializer:json;type:jsonb"`
	CreatedAt                                      time.Time
}

func (eventRecord) TableName() string { return "build_events" }

type logRecord struct {
	WorkspaceID, ApplicationID, BuildID, AttemptID string
	Seq                                            int64
	PayloadHash                                    string
	Content                                        []byte
	Truncated                                      bool
	CreatedAt                                      time.Time
}

func (logRecord) TableName() string { return "build_log_chunks" }

type artifactRecord struct {
	ID                                                         string `gorm:"primaryKey"`
	WorkspaceID, ApplicationID, BuildID, AttemptID, RegistryID string
	CredentialVersion                                          int64
	Repository, Tag, Digest, Platform, State                   string
	VerifiedAt                                                 *time.Time
	CreatedAt                                                  time.Time
}

func (artifactRecord) TableName() string { return "build_artifacts" }

type configRecord struct {
	WorkspaceID, ApplicationID string
	Revision                   int64
	Spec                       Config `gorm:"serializer:json;type:jsonb"`
	CreatedBy                  string
	CreatedAt                  time.Time
}

func (configRecord) TableName() string { return "build_config_revisions" }

// 以下是其他模块表的只读投影，不扩展 central model 或生成查询。
type sourceRecord struct {
	ID, WorkspaceID, ApplicationID, State, SHA256, Dockerfile string
	Entries                                                   []sourcearchive.Entry `gorm:"serializer:json;type:jsonb"`
	ExpiresAt                                                 time.Time
}

func (sourceRecord) TableName() string { return "source_snapshots" }

type settingsRecord struct {
	ID             string
	PolicyRevision int64
}

func (settingsRecord) TableName() string { return "instance_settings" }

type policyRecord struct {
	InstanceID                                string
	Revision                                  int64
	BuildNodeID, RegistryID, RepositoryPrefix string
	ResourceDefault, ResourceLimits           inventory.Resources `gorm:"serializer:json;type:jsonb"`
}

func (policyRecord) TableName() string { return "instance_execution_policies" }

type nodeRecord struct {
	ID, InstanceID, WorkerID, Name, Status, OS, Arch, ClusterNodeUID string
	Capacity                                                         int
	BuildReady                                                       bool
	LastSeen                                                         *time.Time
}

func (nodeRecord) TableName() string { return "execution_nodes" }

type registryRecord struct {
	ID, InstanceID, URL, RepositoryPrefix, Status string
	Revision, CredentialVersion                   int64
	VerifiedAt                                    *time.Time
	VerifiedRevision                              *int64
}

func (registryRecord) TableName() string { return "registries" }

type allocationRecord struct {
	WorkspaceID, InstanceID string
	BuildConcurrency        int
	CPUMillis               int64 `gorm:"column:cpu_millis"`
	MemoryMiB               int64 `gorm:"column:memory_mib"`
}

func (allocationRecord) TableName() string { return "workspace_allocations" }
