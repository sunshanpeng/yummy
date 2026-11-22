package workergateway

import (
	"context"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
)

func (g *Gateway) openPresence(ctx context.Context, s *session) error {
	// 【数据库专有】UPDATE ... RETURNING 为 PostgreSQL 专有语法，原子推进连接纪元并取回新值；
	// MySQL 切换时改为 UPDATE 后同事务 SELECT，或用 connection_epoch 作为 WHERE 条件做乐观校验。
	updated := g.config.DB.WithContext(ctx).Raw(`UPDATE execution_nodes SET connection_epoch = connection_epoch + 1, connection_owner = ?, connected_at = NOW(), last_seen = NOW(), build_ready = FALSE, runtime_ready = FALSE, ingress_ready = FALSE, metrics_ready = FALSE, readiness_reason = '新连接等待资格核验' WHERE id = ? AND worker_id = ? AND status IN ('ACTIVE','DRAINING') RETURNING connection_epoch`, s.owner, s.identity.NodeID, s.identity.WorkerID).Scan(&s.epoch)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 || s.epoch < 1 {
		return workerprotocol.ErrIdentity
	}
	return nil
}
func (g *Gateway) closePresence(ctx context.Context, s *session) error {
	return g.config.DB.WithContext(ctx).Table("execution_nodes").Where("id = ? AND connection_epoch = ? AND connection_owner = ?", s.identity.NodeID, s.epoch, s.owner).Updates(map[string]any{"connected_at": nil, "connection_owner": "", "build_ready": false, "runtime_ready": false, "ingress_ready": false, "metrics_ready": false, "readiness_reason": "连接已断开，等待恢复核验"}).Error
}
func (g *Gateway) heartbeat(ctx context.Context, s *session, message *platformv1.WorkerHeartbeat) error {
	if len(message.ClusterNodeUid) > 256 || len(message.Os) > 32 || len(message.Arch) > 32 {
		return workerprotocol.ErrProtocol
	}
	// 未经资格检查只刷新连接事实，不把自报UID/能力写成资产与就绪事实。
	var registered struct {
		ClusterNodeUID string
		OS             string
		Arch           string
	}
	if err := g.config.DB.WithContext(ctx).Table("execution_nodes").Select("cluster_node_uid,os,arch").Where("id = ?", s.identity.NodeID).Take(&registered).Error; err != nil {
		return err
	}
	if registered.ClusterNodeUID != "" && message.ClusterNodeUid != registered.ClusterNodeUID {
		return workerprotocol.ErrIdentity
	}
	if registered.OS != "" && message.Os != registered.OS {
		return workerprotocol.ErrIdentity
	}
	if registered.Arch != "" && message.Arch != registered.Arch {
		return workerprotocol.ErrIdentity
	}
	updated := g.config.DB.WithContext(ctx).Table("execution_nodes").Where("id = ? AND connection_epoch = ? AND connection_owner = ?", s.identity.NodeID, s.epoch, s.owner).Update("last_seen", time.Now().UTC())
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return workerprotocol.ErrIdentity
	}
	return nil
}
func (g *Gateway) qualification(ctx context.Context, s *session, message *platformv1.WorkerQualification) error {
	if g.config.Capabilities == nil || message.Nonce != s.nonce || message.QualificationId == "" {
		return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_Rejection{Rejection: &platformv1.WorkerRejection{Channel: "qualification", Code: "QUALIFICATION_REQUIRED"}}})
	}
	proof, err := g.config.Capabilities.Verify(ctx, s.identity, s.nonce, message)
	if err != nil {
		return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_Rejection{Rejection: &platformv1.WorkerRejection{Channel: "qualification", Code: "QUALIFICATION_REJECTED"}}})
	}
	if proof.ClusterNodeUID == "" || proof.OS != "linux" || proof.Arch != "amd64" || proof.ClusterNodeUID != message.ClusterNodeUid {
		return workerprotocol.ErrQualification
	}
	updated := g.config.DB.WithContext(ctx).Table("execution_nodes").Where("id = ? AND connection_epoch = ? AND connection_owner = ? AND status IN ('ACTIVE','DRAINING') AND (cluster_node_uid = '' OR cluster_node_uid = ?)", s.identity.NodeID, s.epoch, s.owner, proof.ClusterNodeUID).Updates(map[string]any{"cluster_node_uid": proof.ClusterNodeUID, "os": proof.OS, "arch": proof.Arch, "build_ready": proof.Build, "runtime_ready": proof.Runtime, "ingress_ready": proof.Ingress, "metrics_ready": proof.Metrics, "readiness_reason": "已核验登记资格证据", "last_seen": time.Now().UTC()})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return workerprotocol.ErrIdentity
	}
	return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_Ack{Ack: &platformv1.WorkerAck{Channel: "qualification", State: "VERIFIED"}}})
}
