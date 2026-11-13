package inventory

import (
	"errors"
	"time"
)

var ErrNotReady = errors.New("平台执行资源尚未通过资格检查")

const Platform = "linux/amd64"

type Resources struct {
	CPUMillis int64 `json:"cpuMillis"`
	MemoryMiB int64 `json:"memoryMiB"`
}

type NodeInput struct {
	Name, WorkerID string
	Capacity       int
}
type UpdateNodeInput struct {
	Name, Status     string
	Capacity         int
	ExpectedRevision int64
}
type NodeView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	WorkerID        string     `json:"workerId"`
	Status          string     `json:"status"`
	ClusterNodeUID  string     `json:"clusterNodeUid"`
	OS              string     `json:"os"`
	Arch            string     `json:"arch"`
	ConnectedAt     *time.Time `json:"connectedAt,omitempty"`
	LastSeen        *time.Time `json:"lastSeen,omitempty"`
	BuildReady      bool       `json:"buildReady"`
	RuntimeReady    bool       `json:"runtimeReady"`
	IngressReady    bool       `json:"ingressReady"`
	MetricsReady    bool       `json:"metricsReady"`
	ReadinessReason string     `json:"readinessReason"`
	Capacity        int        `json:"capacity"`
	Revision        int64      `json:"revision"`
}
type NodePage struct {
	Items      []NodeView `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// QualificationInput 登记安装者线下验收的节点资格；登记本身不把节点置为就绪。
type QualificationInput struct {
	NodeID, WorkerID, ClusterNodeUID, OS, Arch, QualificationID string
	Build, Runtime, Ingress, Metrics                            bool
	Evidence                                                    map[string]string
}
type QualificationView struct {
	ID              string     `json:"id"`
	NodeID          string     `json:"nodeId"`
	WorkerID        string     `json:"workerId"`
	ClusterNodeUID  string     `json:"clusterNodeUid"`
	OS              string     `json:"os"`
	Arch            string     `json:"arch"`
	BuildReady      bool       `json:"buildReady"`
	RuntimeReady    bool       `json:"runtimeReady"`
	IngressReady    bool       `json:"ingressReady"`
	MetricsReady    bool       `json:"metricsReady"`
	QualificationID string     `json:"qualificationId"`
	Status          string     `json:"status"`
	CreatedBy       string     `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	RevokedAt       *time.Time `json:"revokedAt,omitempty"`
}
type QualificationPage struct {
	Items      []QualificationView `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

type RegistryInput struct{ Name, URL, RepositoryPrefix, Username, Password string }
type UpdateRegistryInput struct {
	Name, URL, RepositoryPrefix, Status string
	ExpectedRevision                    int64
	Username                            *string
	Password                            *string
}
type RegistryView struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	URL                  string     `json:"url"`
	RepositoryPrefix     string     `json:"repositoryPrefix"`
	Status               string     `json:"status"`
	CredentialConfigured bool       `json:"credentialConfigured"`
	CredentialVersion    int64      `json:"credentialVersion"`
	Revision             int64      `json:"revision"`
	VerifiedAt           *time.Time `json:"verifiedAt,omitempty"`
}
type RegistryPage struct {
	Items      []RegistryView `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

type PolicyInput struct {
	BuildNodeID, RuntimeNodeID, RegistryID, RepositoryPrefix, BaseDomain string
	ResourceDefault, ResourceLimits                                      Resources
	AutoRecoveryEnabled                                                  bool
	ExpectedRevision                                                     int64
}
type PolicyView struct {
	InstanceID          string    `json:"instanceId"`
	Revision            int64     `json:"revision"`
	BuildNodeID         string    `json:"buildNodeId"`
	RuntimeNodeID       string    `json:"runtimeNodeId"`
	RegistryID          string    `json:"registryId"`
	RepositoryPrefix    string    `json:"repositoryPrefix"`
	BaseDomain          string    `json:"baseDomain"`
	ResourceDefault     Resources `json:"resourceDefault"`
	ResourceLimits      Resources `json:"resourceLimits"`
	AutoRecoveryEnabled bool      `json:"autoRecoveryEnabled"`
	CreatedAt           time.Time `json:"createdAt"`
}
type AllocationInput struct {
	AllowedNodeIDs                         []string
	MaxApplications, BuildConcurrency      int
	CPUMillis, MemoryMiB, ExpectedRevision int64
}
type AllocationView struct {
	WorkspaceID      string   `json:"workspaceId"`
	AllowedNodeIDs   []string `json:"allowedNodeIds"`
	MaxApplications  int      `json:"maxApplications"`
	BuildConcurrency int      `json:"buildConcurrency"`
	CPUMillis        int64    `json:"cpuMillis"`
	MemoryMiB        int64    `json:"memoryMiB"`
	Revision         int64    `json:"revision"`
}

// ExecutionSummary 是业务可读摘要，不含凭据、worker 标识、主机地址或集群 UID。
// 后续构建/部署服务必须在受理事务中再次核对 revision 并冻结内部执行记录。
type ExecutionSummary struct {
	PolicyRevision      int64     `json:"policyRevision"`
	Platform            string    `json:"platform"`
	NodeID              string    `json:"nodeId"`
	NodeName            string    `json:"nodeName"`
	ImageRepository     string    `json:"imageRepository"`
	DefaultDomain       string    `json:"defaultDomain,omitempty"`
	ResourceDefault     Resources `json:"resourceDefault"`
	ResourceLimits      Resources `json:"resourceLimits"`
	AutoRecoveryEnabled bool      `json:"autoRecoveryEnabled"`
	Ready               bool      `json:"ready"`
	Reason              string    `json:"reason,omitempty"`
}
