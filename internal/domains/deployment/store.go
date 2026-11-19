package deployment

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockActiveApplicationForDispatch 锁定并确认应用在目标空间下处于活跃状态。
// 加锁顺序与 ClaimPending、撤销证书及停止操作一致，提交前不可放开身份锁。
// 【方言兼容】单表锁定投影，标准 SQL。
func LockActiveApplicationForDispatch(tx *gorm.DB, workspaceID, applicationID string) error {
	var application struct{ ID string }
	return tx.Table("applications").
		Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
		Where("id = ? AND workspace_id = ? AND status = 'ACTIVE'", applicationID, workspaceID).
		Take(&application).Error
}

// FrozenDeployment 是部署凭据材料化所需的最小冻结事实（六表 JOIN 的结果）。
type FrozenDeployment struct {
	RegistryID, Repository, Digest, Platform, Action string
	CredentialVersion                                int64
}

// FrozenDeploymentForDispatch 在 worker 凭据材料化时冻结部署事实：运行状态、
// 活跃操作、release、placement、节点与已登记证书必须全部与上报身份一致，
// 即使派发命令过期或被伪造也只能读到关联 release 的冻结凭据。
// 【方言兼容】标准 JOIN 与 IN 谓词，PostgreSQL 与 MySQL 通用。六表关联按约定
// 收口在本文件，装配层只做授权与凭据校验编排。
func FrozenDeploymentForDispatch(tx *gorm.DB, dispatch Dispatch, worker WorkerIdentity, certificateSHA256 string) (FrozenDeployment, error) {
	var frozen FrozenDeployment
	err := tx.Table("application_runtime_states AS s").
		Select("r.registry_id,r.credential_version,r.repository,r.digest,r.platform,o.action").
		Joins("JOIN deployment_operations o ON o.id = s.active_operation_id AND o.workspace_id = s.workspace_id AND o.application_id = s.application_id").
		Joins("JOIN application_releases r ON r.id = o.release_id AND r.workspace_id = o.workspace_id AND r.application_id = o.application_id").
		Joins("JOIN application_placements p ON p.workspace_id = s.workspace_id AND p.application_id = s.application_id").
		Joins("JOIN execution_nodes n ON n.id = p.node_id AND n.worker_id = p.worker_id").
		Joins("JOIN worker_certificates c ON c.node_id = n.id AND c.worker_id = n.worker_id").
		Where("s.workspace_id = ? AND s.application_id = ? AND s.generation = ? AND s.desired_release_id = ? AND o.id = ? AND o.active = TRUE AND o.generation = s.generation AND o.action <> 'STOP' AND p.node_id = ? AND p.worker_id = ? AND p.cluster_node_uid = ? AND n.cluster_node_uid = p.cluster_node_uid AND n.status IN ('ACTIVE','DRAINING') AND c.serial = ? AND c.certificate_sha256 = ? AND c.revoked_at IS NULL AND c.not_before <= NOW() AND c.not_after > NOW()",
			dispatch.WorkspaceID, dispatch.ApplicationID, dispatch.Generation, dispatch.ReleaseID, dispatch.OperationID,
			worker.NodeID, worker.WorkerID, dispatch.Placement.ClusterNodeUID,
			worker.CertificateSerial, certificateSHA256).
		Take(&frozen).Error
	return frozen, err
}
