package build

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockIdempotencyKey 对“工作空间+actor+幂等键”的组合键取事务级咨询锁，保证同一
// 幂等键的并发创建串行化后做指纹比对。【数据库专有】pg_advisory_xact_lock 与
// hashtextextended（任意长度键映射到 64 位锁空间）均为 PostgreSQL 专有；MySQL
// 切换时改用幂等键唯一索引 + 唯一冲突重读的方案替代。调用方必须持有进行中的事务。
func lockIdempotencyKey(tx *gorm.DB, key string) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", key).Error
}

// lockDispatchableBuildsForNode 锁定节点上未终态且 attempt 租约已到期的构建，
// 供 PendingDispatch 重投（SKIP LOCKED 防止多 worker 抢同一行，两方言 8.0+/PG 通用）。
// 到期比较用数据库时钟，与租约写入时钟一致。
// 【方言兼容】EXISTS 相关子查询为标准 SQL；子查询引用外层别名 builds.id 做行级
// 关联，gorm gen 类型安全字段无法自然表达，按约定收口在本文件。
func lockDispatchableBuildsForNode(tx *gorm.DB, nodeID string, limit int) ([]buildRecord, error) {
	var records []buildRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("node_id = ? AND state NOT IN ('SUCCEEDED','FAILED','CANCELED','TIMED_OUT') AND EXISTS (SELECT 1 FROM build_attempts a WHERE a.build_id = builds.id AND a.occupied AND a.next_dispatch_at <= NOW())", nodeID).
		Order("created_at, id").Limit(limit).Find(&records).Error
	return records, err
}

// FrozenBuildMaterial 是构建凭据材料化所需的最小冻结事实（调用前已完成授权核验）。
type FrozenBuildMaterial struct {
	WorkspaceID, ApplicationID, RegistryID, Repository string
	CredentialVersion                                  int64
}

// FrozenBuildMaterialForGrant 读取构建行的冻结投影，供装配层核对派发命令未被伪造。
// 【方言兼容】单表锁定投影，标准 SQL。
func FrozenBuildMaterialForGrant(tx *gorm.DB, workspaceID, applicationID, buildID string) (FrozenBuildMaterial, error) {
	var frozen FrozenBuildMaterial
	err := tx.Table("builds").
		Select("workspace_id, application_id, registry_id, credential_version, repository").
		Where("id = ? AND workspace_id = ? AND application_id = ?", buildID, workspaceID, applicationID).
		Take(&frozen).Error
	return frozen, err
}
