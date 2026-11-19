package enrollment

import (
	"context"
	"crypto/x509"
	"errors"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"gorm.io/gorm"
)

// Rotate 的 current 必须来自同一条已验证 mTLS 连接，不接受请求正文内自报旧证书。
// 旧证书在同一事务内撤销，重放旧证书不能再次轮换；新证书只继承数据库 worker 身份。
func (s *Service) Rotate(ctx context.Context, current *x509.Certificate, csrPEM []byte) (result IssuedCertificate, err error) {
	request, err := parseCSR(csrPEM)
	if err != nil {
		return result, err
	}
	var revoked Identity
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if current == nil || current.SerialNumber == nil {
			return ErrPeer
		}
		var lookup certificateRecord
		if err := tx.First(&lookup, "serial = ?", current.SerialNumber.Text(16)).Error; err != nil {
			return peerError(err)
		}
		node, err := lockedNode(tx, lookup.NodeID)
		if err != nil {
			return err
		}
		old, err := s.verify(tx, current)
		if err != nil {
			return err
		}
		now := s.now()
		if old.NotAfter.Sub(now) > RotationWindow {
			return ErrRotationEarly
		}
		serial := old.Serial
		value, issued, err := s.issue(node, request, now, &serial)
		if err != nil {
			return err
		}
		if err := tx.Model(&certificateRecord{}).Where("serial = ? AND revoked_at IS NULL", old.Serial).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		if err := s.recordCredentialEvent(tx, value, "ROTATE"); err != nil {
			return err
		}
		revoked, result = identity(old), issued
		return nil
	})
	if err == nil && s.onRevoke != nil {
		s.onRevoke(revoked)
	}
	return result, err
}

func (s *Service) RevokeCertificate(ctx context.Context, principal authorization.Principal, serial string) error {
	var revoked Identity
	err := s.access.Do(ctx, principal, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value certificateRecord
		if err := tx.First(&value, "serial = ?", serial).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if _, err := lockedNode(tx, value.NodeID); err != nil {
			return err
		}
		if err := tx.First(&value, "serial = ?", serial).Error; err != nil {
			return err
		}
		revoked = identity(value)
		if value.RevokedAt != nil {
			return nil
		}
		if err := tx.Model(&value).Update("revoked_at", s.now()).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "instance.node.certificate.revoke", "node", value.NodeID, nil, nil)
	})
	if err == nil && s.onRevoke != nil {
		s.onRevoke(revoked)
	}
	return err
}

// EnableNode 不复活旧凭证，也不重置 cluster_node_uid；换主机必须建立新节点身份。
// 状态恢复后必须重新创建登记凭证并完成真实资格检查。
func (s *Service) EnableNode(ctx context.Context, principal authorization.Principal, nodeID string) error {
	return s.access.Do(ctx, principal, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		node, err := lockedNode(tx, nodeID)
		if err != nil {
			return err
		}
		if node.Status != "DISABLED" {
			return authorization.ErrConflict
		}
		now := s.now()
		// 对任何旧状态保持失败关闭；即使历史停用路径未吊证也不允许启用后复活。
		if err := tx.Model(&certificateRecord{}).Where("node_id = ? AND revoked_at IS NULL", nodeID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&registrationRecord{}).Where("node_id = ? AND consumed_at IS NULL AND revoked_at IS NULL", nodeID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&nodeRecord{}).Where("id = ?", nodeID).Updates(map[string]any{"status": "ACTIVE", "revision": gorm.Expr("revision + 1"), "updated_at": now, "connected_at": nil, "build_ready": false, "runtime_ready": false, "ingress_ready": false, "metrics_ready": false, "readiness_reason": "节点已启用，需要重新登记并验证原节点身份"}).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "instance.node.enable", "node", nodeID, nil, nil)
	})
}

// DisableNode 先在数据库撤销所有凭证和资格，再在提交后通知连接层关闭流。
// 仅将节点改回 ACTIVE 不会恢复已吊销证书，需要重新生成登记凭证。
func (s *Service) DisableNode(ctx context.Context, principal authorization.Principal, nodeID string) error {
	var revoked []Identity
	err := s.access.Do(ctx, principal, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		if _, err := lockedNode(tx, nodeID); err != nil {
			return err
		}
		now := s.now()
		var certificates []certificateRecord
		if err := tx.Where("node_id = ? AND revoked_at IS NULL", nodeID).Find(&certificates).Error; err != nil {
			return err
		}
		for _, value := range certificates {
			revoked = append(revoked, identity(value))
		}
		if err := tx.Model(&certificateRecord{}).Where("node_id = ? AND revoked_at IS NULL", nodeID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&registrationRecord{}).Where("node_id = ? AND consumed_at IS NULL AND revoked_at IS NULL", nodeID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&nodeRecord{}).Where("id = ?", nodeID).Updates(map[string]any{"status": "DISABLED", "revision": gorm.Expr("revision + 1"), "updated_at": now, "connected_at": nil, "build_ready": false, "runtime_ready": false, "ingress_ready": false, "metrics_ready": false, "readiness_reason": "节点已停用，登记证书已撤销"}).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "instance.node.disable", "node", nodeID, nil, nil)
	})
	if err == nil && s.onRevoke != nil {
		for _, value := range revoked {
			s.onRevoke(value)
		}
	}
	return err
}
