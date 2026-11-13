package enrollment

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db            *gorm.DB
	access        *authorization.Access
	ca            *x509.Certificate
	signer        crypto.Signer
	caPEM         []byte
	allowLoopback bool
	onRevoke      func(Identity)
	now           func() time.Time
}

func NewService(db *gorm.DB, access *authorization.Access, config Config) (*Service, error) {
	if db == nil || access == nil {
		return nil, authorization.ErrInvalid
	}
	ca, signer, caPEM, err := loadCA(config)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, access: access, ca: ca, signer: signer, caPEM: caPEM, allowLoopback: config.AllowInsecureLoopback, onRevoke: config.OnRevoke, now: func() time.Time { return time.Now().UTC() }}, nil
}

// ClientCAPool 为独立 worker 监听器提供只包含本实例签发 CA 的新池，不暴露签名私钥。
func (s *Service) ClientCAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.ca)
	return pool
}

func (s *Service) CreateRegistration(ctx context.Context, principal authorization.Principal, nodeID string) (result Registration, err error) {
	err = s.access.Do(ctx, principal, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		node, err := lockedNode(tx, nodeID)
		if err != nil {
			return err
		}
		if node.Status != "ACTIVE" {
			return ErrRegistration
		}
		var count int64
		if err := tx.Model(&certificateRecord{}).Where("node_id = ? AND revoked_at IS NULL", nodeID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return authorization.ErrConflict
		}
		now := s.now()
		if err := tx.Model(&registrationRecord{}).Where("node_id = ? AND consumed_at IS NULL AND revoked_at IS NULL", nodeID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		id, err := security.NewID("reg")
		if err != nil {
			return err
		}
		token, err := security.NewToken("wrk_")
		if err != nil {
			return err
		}
		value := registrationRecord{ID: id, NodeID: nodeID, TokenHash: security.Digest(token), CreatedBy: current.UserID, CreatedAt: now, ExpiresAt: now.Add(RegistrationLifetime)}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "instance.node.registration.create", "node", nodeID, nil, nil); err != nil {
			return err
		}
		result = Registration{ID: id, NodeID: nodeID, Token: token, ExpiresAt: value.ExpiresAt}
		return nil
	})
	return result, err
}

func (s *Service) Register(ctx context.Context, input RegisterInput) (result IssuedCertificate, err error) {
	if !input.Transport.TLS && !(s.allowLoopback && input.Transport.Loopback) {
		return result, ErrTLSRequired
	}
	if len(input.Token) != 47 || !strings.HasPrefix(input.Token, "wrk_") {
		return result, ErrRegistration
	}
	request, err := parseCSR(input.CSRPEM)
	if err != nil {
		return result, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先查定位信息再锁 node，所有登记/轮换/撤销使用 node→credential 的同一锁序。
		var token registrationRecord
		if err := tx.First(&token, "token_hash = ?", security.Digest(input.Token)).Error; err != nil {
			return registrationError(err)
		}
		node, err := lockedNode(tx, token.NodeID)
		if err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&token, "id = ?", token.ID).Error; err != nil {
			return registrationError(err)
		}
		now := s.now()
		if node.Status != "ACTIVE" || token.ConsumedAt != nil || token.RevokedAt != nil || !token.ExpiresAt.After(now) {
			return ErrRegistration
		}
		var count int64
		if err := tx.Model(&certificateRecord{}).Where("node_id = ? AND revoked_at IS NULL", node.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrRegistration
		}
		value, issued, err := s.issue(node, request, now, nil)
		if err != nil {
			return err
		}
		if err := tx.Create(&value).Error; err != nil {
			return err
		}
		if err := tx.Model(&token).Update("consumed_at", now).Error; err != nil {
			return err
		}
		if err := s.recordCredentialEvent(tx, value, "REGISTER"); err != nil {
			return err
		}
		result = issued
		return nil
	})
	return result, err
}

// VerifyPeer 只接受已经过 TLS 链校验/ClientAuth/持有证明的证书，不信任 Hello worker_id。
// 额外比较完整证书摘要，避免只凭可猜的序列号/CN 接受伪造或其他 CA 的叶证书。
func (s *Service) VerifyPeer(ctx context.Context, certificate *x509.Certificate) (Identity, error) {
	value, err := s.verify(s.db.WithContext(ctx), certificate)
	if err != nil {
		return Identity{}, err
	}
	return identity(value), nil
}

func (s *Service) verify(tx *gorm.DB, certificate *x509.Certificate) (certificateRecord, error) {
	var value certificateRecord
	if certificate == nil || certificate.SerialNumber == nil || certificate.SerialNumber.Sign() <= 0 || certificate.IsCA || len(certificate.Raw) == 0 {
		return value, ErrPeer
	}
	if err := tx.First(&value, "serial = ?", certificate.SerialNumber.Text(16)).Error; err != nil {
		return value, peerError(err)
	}
	var node nodeRecord
	if err := tx.First(&node, "id = ?", value.NodeID).Error; err != nil {
		return value, peerError(err)
	}
	now := s.now()
	digest := sha256.Sum256(certificate.Raw)
	if value.RevokedAt != nil || (node.Status != "ACTIVE" && node.Status != "DRAINING") || node.WorkerID != value.WorkerID || certificate.Subject.CommonName != value.WorkerID || value.CertificateSHA256 != hex.EncodeToString(digest[:]) || now.Before(value.NotBefore) || !value.NotAfter.After(now) || now.Before(certificate.NotBefore) || !certificate.NotAfter.After(now) || !value.NotBefore.Equal(certificate.NotBefore) || !value.NotAfter.Equal(certificate.NotAfter) {
		return value, ErrPeer
	}
	if certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 || !hasClientAuth(certificate) {
		return value, ErrPeer
	}
	return value, nil
}

func hasClientAuth(certificate *x509.Certificate) bool {
	for _, usage := range certificate.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

func lockedNode(tx *gorm.DB, id string) (nodeRecord, error) {
	var node nodeRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&node, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return node, authorization.ErrNotFound
	}
	return node, err
}
func (s *Service) recordCredentialEvent(tx *gorm.DB, certificate certificateRecord, action string) error {
	id, err := security.NewID("wce")
	if err != nil {
		return err
	}
	return tx.Create(&credentialEvent{ID: id, NodeID: certificate.NodeID, WorkerID: certificate.WorkerID, Serial: certificate.Serial, Action: action, RequestID: audit.RequestID(tx.Statement.Context), CreatedAt: s.now()}).Error
}

func registrationError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrRegistration
	}
	return err
}
func peerError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrPeer
	}
	return err
}
