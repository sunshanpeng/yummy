package qualification

import (
	"context"

	"gorm.io/gorm"
)

// findActiveQualification 核对验收记录：只认可活动记录，并要求记录归属的节点
// 仍处于可连接状态；node_id/worker_id 同时作为查询条件，跨节点或跨 worker
// 复用资格直接失败。
// 【方言兼容】标准 JOIN 与 IN 谓词，PostgreSQL 与 MySQL 通用。
func findActiveQualification(db *gorm.DB, ctx context.Context, qualificationID, nodeID, workerID string) (record, error) {
	var value record
	err := db.WithContext(ctx).Raw(`
		SELECT q.id, q.node_id, q.worker_id, q.cluster_node_uid, q.os, q.arch,
		       q.build_ready, q.runtime_ready, q.ingress_ready, q.metrics_ready, q.qualification_id
		FROM node_qualifications q
		JOIN execution_nodes n
		  ON n.id = q.node_id AND n.worker_id = q.worker_id AND n.status IN ('ACTIVE','DRAINING')
		WHERE q.qualification_id = ? AND q.status = 'ACTIVE'
		  AND q.node_id = ? AND q.worker_id = ?`,
		qualificationID, nodeID, workerID).Scan(&value).Error
	return value, err
}
