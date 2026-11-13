package inventory

import "time"

// 持久化结构留在模块内部，避免实例凭据变成公共 DTO 或跨模块可写对象。
type instanceRecord struct {
	Singleton      bool      `gorm:"column:singleton;primaryKey"`
	ID             string    `gorm:"column:id"`
	PolicyRevision int64     `gorm:"column:policy_revision"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (instanceRecord) TableName() string { return "instance_settings" }

type nodeRecord struct {
	ID              string     `gorm:"column:id;primaryKey"`
	InstanceID      string     `gorm:"column:instance_id"`
	Name            string     `gorm:"column:name"`
	WorkerID        string     `gorm:"column:worker_id"`
	Status          string     `gorm:"column:status"`
	ClusterNodeUID  string     `gorm:"column:cluster_node_uid"`
	OS              string     `gorm:"column:os"`
	Arch            string     `gorm:"column:arch"`
	ConnectedAt     *time.Time `gorm:"column:connected_at"`
	LastSeen        *time.Time `gorm:"column:last_seen"`
	BuildReady      bool       `gorm:"column:build_ready"`
	RuntimeReady    bool       `gorm:"column:runtime_ready"`
	IngressReady    bool       `gorm:"column:ingress_ready"`
	MetricsReady    bool       `gorm:"column:metrics_ready"`
	ReadinessReason string     `gorm:"column:readiness_reason"`
	Capacity        int        `gorm:"column:capacity"`
	Revision        int64      `gorm:"column:revision"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (nodeRecord) TableName() string { return "execution_nodes" }

// qualificationRecord 对应 node_qualifications；验收证据只存摘要，不承载凭据。
type qualificationRecord struct {
	ID              string         `gorm:"column:id;primaryKey"`
	NodeID          string         `gorm:"column:node_id"`
	WorkerID        string         `gorm:"column:worker_id"`
	ClusterNodeUID  string         `gorm:"column:cluster_node_uid"`
	OS              string         `gorm:"column:os"`
	Arch            string         `gorm:"column:arch"`
	BuildReady      bool           `gorm:"column:build_ready"`
	RuntimeReady    bool           `gorm:"column:runtime_ready"`
	IngressReady    bool           `gorm:"column:ingress_ready"`
	MetricsReady    bool           `gorm:"column:metrics_ready"`
	QualificationID string         `gorm:"column:qualification_id"`
	Evidence        map[string]any `gorm:"column:evidence;type:jsonb;serializer:json"`
	Status          string         `gorm:"column:status"`
	CreatedBy       string         `gorm:"column:created_by"`
	CreatedAt       time.Time      `gorm:"column:created_at"`
	RevokedAt       *time.Time     `gorm:"column:revoked_at"`
}

func (qualificationRecord) TableName() string { return "node_qualifications" }

type registryRecord struct {
	ID                string     `gorm:"column:id;primaryKey"`
	InstanceID        string     `gorm:"column:instance_id"`
	Name              string     `gorm:"column:name"`
	URL               string     `gorm:"column:url"`
	RepositoryPrefix  string     `gorm:"column:repository_prefix"`
	Username          string     `gorm:"column:username"`
	EncryptedPassword []byte     `gorm:"column:encrypted_password" json:"-"`
	CredentialVersion int64      `gorm:"column:credential_version"`
	Status            string     `gorm:"column:status"`
	Revision          int64      `gorm:"column:revision"`
	VerifiedAt        *time.Time `gorm:"column:verified_at"`
	VerifiedRevision  *int64     `gorm:"column:verified_revision"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (registryRecord) TableName() string { return "registries" }

type credentialRecord struct {
	RegistryURL       string    `gorm:"column:registry_url"`
	RepositoryPrefix  string    `gorm:"column:repository_prefix"`
	RegistryID        string    `gorm:"column:registry_id;primaryKey"`
	Version           int64     `gorm:"column:version;primaryKey"`
	Username          string    `gorm:"column:username"`
	EncryptedPassword []byte    `gorm:"column:encrypted_password" json:"-"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (credentialRecord) TableName() string { return "registry_credential_versions" }

type policyRecord struct {
	InstanceID          string    `gorm:"column:instance_id;primaryKey"`
	Revision            int64     `gorm:"column:revision;primaryKey"`
	BuildNodeID         string    `gorm:"column:build_node_id"`
	RuntimeNodeID       string    `gorm:"column:runtime_node_id"`
	RegistryID          string    `gorm:"column:registry_id"`
	RepositoryPrefix    string    `gorm:"column:repository_prefix"`
	BaseDomain          string    `gorm:"column:base_domain"`
	ResourceDefault     Resources `gorm:"column:resource_default;type:jsonb;serializer:json"`
	ResourceLimits      Resources `gorm:"column:resource_limits;type:jsonb;serializer:json"`
	AutoRecoveryEnabled bool      `gorm:"column:auto_recovery_enabled"`
	CreatedAt           time.Time `gorm:"column:created_at"`
}

func (policyRecord) TableName() string { return "instance_execution_policies" }

type allocationRecord struct {
	WorkspaceID      string    `gorm:"column:workspace_id;primaryKey"`
	InstanceID       string    `gorm:"column:instance_id"`
	MaxApplications  int       `gorm:"column:max_applications"`
	BuildConcurrency int       `gorm:"column:build_concurrency"`
	CPUMillis        int64     `gorm:"column:cpu_millis"`
	MemoryMiB        int64     `gorm:"column:memory_mib"`
	Revision         int64     `gorm:"column:revision"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

func (allocationRecord) TableName() string { return "workspace_allocations" }

type allocationNodeRecord struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	InstanceID  string `gorm:"column:instance_id"`
	NodeID      string `gorm:"column:node_id;primaryKey"`
}

func (allocationNodeRecord) TableName() string { return "workspace_allocation_nodes" }
