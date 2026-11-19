package source

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BuildGrant 只允许交给已经验证身份的 worker，不属于普通用户 DTO。
type BuildGrant struct {
	ID                                  string
	Token                               string `json:"-"`
	SHA256                              string
	BuildID, WorkspaceID, ApplicationID string
	Size                                int64
	ExpiresAt                           time.Time
}

type buildGrantRecord struct {
	ID, WorkspaceID, ApplicationID, SnapshotID, BuildID, AttemptID string
	NodeID, WorkerID, CertificateSerial, TokenHash                 string
	EncryptedToken                                                 []byte `json:"-"`
	CreatedAt, ExpiresAt                                           time.Time
}

func (buildGrantRecord) TableName() string { return "source_build_grants" }

func buildGrantAAD(value buildGrantRecord) string {
	return fmt.Sprintf("source-grant/%s/workspace/%s/application/%s/build/%s/attempt/%s/node/%s/cert/%s", value.ID, value.WorkspaceID, value.ApplicationID, value.BuildID, value.AttemptID, value.NodeID, value.CertificateSerial)
}

// GrantForBuild 必须在调用方事务中执行；锁定构建、attempt、节点证书与快照后重新读取冻结事实。
// 同一 attempt 总是返回同一个令牌以保持 Kubernetes 临时 Secret 幂等，令牌最迟随任务 deadline 失效。
// 证书轮换恢复：若已有授权绑定旧证书而该 attempt 尚无确认 JobUID（旧令牌不可能被任何
// Secret 引用），原子换绑到当前证书重新发放；JobUID 已存在的执行绝不换发，下载侧按
// 节点当前有效证书放行（见 OpenBuildGrant），节点撤销全部证书才阻断在途构建。
func (s *Service) GrantForBuild(tx *gorm.DB, box *security.SecretBox, identity build.WorkerIdentity, attemptID string) (BuildGrant, error) {
	if tx == nil || box == nil || identity.NodeID == "" || identity.WorkerID == "" || identity.CertificateSerial == "" || attemptID == "" {
		return BuildGrant{}, authorization.ErrInvalid
	}
	var frozen struct {
		BuildID, WorkspaceID, ApplicationID, SnapshotID, SourceSHA256 string
		NodeID, WorkerID, State, DesiredState, JobUID                 string
		Occupied                                                      bool
		DeadlineAt                                                    time.Time
	}
	// 与构建状态机保持 build→attempt 的行锁顺序。来自 worker 的 attempt 仅用于定位。
	var lookup struct{ BuildID string }
	if err := tx.Table("build_attempts").Select("build_id").Where("id = ?", attemptID).Take(&lookup).Error; err != nil {
		return BuildGrant{}, grantError(err)
	}
	if err := tx.Table("builds").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id AS build_id, workspace_id, application_id, source_snapshot_id AS snapshot_id, source_sha256, node_id, state, desired_state, deadline_at").Where("id = ?", lookup.BuildID).Take(&frozen).Error; err != nil {
		return BuildGrant{}, grantError(err)
	}
	var attempt struct {
		NodeID, WorkerID, JobUID string
		Occupied                 bool
	}
	if err := tx.Table("build_attempts").Clauses(clause.Locking{Strength: "UPDATE"}).Select("node_id,worker_id,job_uid,occupied").Where("id = ? AND build_id = ?", attemptID, frozen.BuildID).Take(&attempt).Error; err != nil {
		return BuildGrant{}, grantError(err)
	}
	if frozen.NodeID != identity.NodeID || attempt.NodeID != identity.NodeID || attempt.WorkerID != identity.WorkerID || !attempt.Occupied || attempt.JobUID != "" || frozen.State != build.Starting || frozen.DesiredState != "RUN" || !frozen.DeadlineAt.After(time.Now()) {
		return BuildGrant{}, authorization.ErrDenied
	}
	// 与停用/轮换统一按 node→certificate 的顺序加共享锁，防止发放期间撤权提交。
	var node struct{ ID string }
	if err := tx.Table("execution_nodes").Clauses(clause.Locking{Strength: "SHARE"}).Select("id").Where("id = ? AND worker_id = ? AND status IN ('ACTIVE','DRAINING')", identity.NodeID, identity.WorkerID).Take(&node).Error; err != nil {
		return BuildGrant{}, grantError(err)
	}
	var certificate struct {
		Serial   string
		NotAfter time.Time
	}
	err := tx.Table("worker_certificates").Clauses(clause.Locking{Strength: "SHARE"}).Select("serial,not_after").Where("serial = ? AND node_id = ? AND worker_id = ? AND revoked_at IS NULL AND not_before <= NOW() AND not_after > NOW()", identity.CertificateSerial, identity.NodeID, identity.WorkerID).Take(&certificate).Error
	if err != nil {
		return BuildGrant{}, grantError(err)
	}
	var snapshot snapshotRecord
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&snapshot, "id = ? AND workspace_id = ? AND application_id = ?", frozen.SnapshotID, frozen.WorkspaceID, frozen.ApplicationID).Error; err != nil {
		return BuildGrant{}, grantError(err)
	}
	if snapshot.State != StateReady || snapshot.SHA256 != frozen.SourceSHA256 || snapshot.CompressedBytes < 1 || !storageKeyPattern.MatchString(snapshot.StorageKey) {
		return BuildGrant{}, ErrNotReady
	}
	var value buildGrantRecord
	err = tx.Where("attempt_id = ?", attemptID).Take(&value).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.issueBuildGrant(tx, box, identity, frozen, attempt, certificate, snapshot, attemptID)
	}
	if err != nil {
		return BuildGrant{}, err
	}
	if value.NodeID != identity.NodeID || value.WorkerID != identity.WorkerID || value.SnapshotID != snapshot.ID || !value.ExpiresAt.After(time.Now()) {
		return BuildGrant{}, authorization.ErrDenied
	}
	if value.CertificateSerial != identity.CertificateSerial {
		// 授权绑定签发时的证书序列号；轮换后 worker 携新证书重投且尚无确认 JobUID 时，
		// 删除旧行换绑到当前证书。JobUID 已存在说明旧令牌可能已进入 Secret，绝不换发。
		if attempt.JobUID != "" {
			return BuildGrant{}, authorization.ErrDenied
		}
		if err := tx.Where("attempt_id = ? AND certificate_serial = ?", attemptID, value.CertificateSerial).Delete(&buildGrantRecord{}).Error; err != nil {
			return BuildGrant{}, err
		}
		return s.issueBuildGrant(tx, box, identity, frozen, attempt, certificate, snapshot, attemptID)
	}
	plaintext, err := box.Decrypt(value.EncryptedToken, buildGrantAAD(value))
	if err != nil {
		return BuildGrant{}, ErrUnavailable
	}
	defer clear(plaintext)
	token := string(plaintext)
	if subtle.ConstantTimeCompare([]byte(security.Digest(token)), []byte(value.TokenHash)) != 1 {
		return BuildGrant{}, ErrUnavailable
	}
	return BuildGrant{ID: value.ID, Token: token, SHA256: snapshot.SHA256, BuildID: value.BuildID, WorkspaceID: value.WorkspaceID, ApplicationID: value.ApplicationID, Size: snapshot.CompressedBytes, ExpiresAt: value.ExpiresAt}, nil
}

// issueBuildGrant 在调用方事务内创建新授权；certificate 必须是 identity 当前序列号的
// 有效证书，令牌截止时间取构建 deadline 与证书到期较早者。
func (s *Service) issueBuildGrant(tx *gorm.DB, box *security.SecretBox, identity build.WorkerIdentity, frozen struct {
	BuildID, WorkspaceID, ApplicationID, SnapshotID, SourceSHA256 string
	NodeID, WorkerID, State, DesiredState, JobUID                 string
	Occupied                                                      bool
	DeadlineAt                                                    time.Time
}, attempt struct {
	NodeID, WorkerID, JobUID string
	Occupied                 bool
}, certificate struct {
	Serial   string
	NotAfter time.Time
}, snapshot snapshotRecord, attemptID string) (BuildGrant, error) {
	id, err := security.NewID("sgr")
	if err != nil {
		return BuildGrant{}, err
	}
	token, err := security.NewToken("ym_sg_")
	if err != nil {
		return BuildGrant{}, err
	}
	now := time.Now().UTC()
	expires := frozen.DeadlineAt
	if certificate.NotAfter.Before(expires) {
		expires = certificate.NotAfter
	}
	value := buildGrantRecord{ID: id, WorkspaceID: frozen.WorkspaceID, ApplicationID: frozen.ApplicationID, SnapshotID: frozen.SnapshotID, BuildID: frozen.BuildID, AttemptID: attemptID, NodeID: identity.NodeID, WorkerID: identity.WorkerID, CertificateSerial: identity.CertificateSerial, TokenHash: security.Digest(token), CreatedAt: now, ExpiresAt: expires}
	value.EncryptedToken, err = box.Encrypt([]byte(token), buildGrantAAD(value))
	if err != nil {
		return BuildGrant{}, err
	}
	if err := tx.Create(&value).Error; err != nil {
		return BuildGrant{}, err
	}
	return BuildGrant{ID: value.ID, Token: token, SHA256: snapshot.SHA256, BuildID: value.BuildID, WorkspaceID: value.WorkspaceID, ApplicationID: value.ApplicationID, Size: snapshot.CompressedBytes, ExpiresAt: value.ExpiresAt}, nil
}

// OpenBuildGrant 在打开文件时复核授权、任务、节点与证书，读取句柄由调用者关闭。
// 下载开始后的流由 HTTP 截止时间控制；令牌撤销不会回滚已经发送的字节。
// 证书按节点粒度放行：授权绑定签发序列号（审计/加密 AAD 语义不变），但下载有效性
// 取决于该节点当前是否存在任一有效证书——证书轮换不断在途构建，节点全部证书被
// 撤销才阻断；下载端是受限 Bearer，节点才是信任边界。
func (s *Service) OpenBuildGrant(ctx context.Context, id, token string) (reader io.ReadCloser, result BuildGrant, err error) {
	if !strings.HasPrefix(id, "sgr_") || len(id) != 36 || !strings.HasPrefix(token, "ym_sg_") || len(token) != 49 {
		return nil, result, authorization.ErrUnauthenticated
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var value buildGrantRecord
		if err := tx.Where("id = ? AND token_hash = ? AND expires_at > NOW()", id, security.Digest(token)).Take(&value).Error; err != nil {
			return grantError(err)
		}
		var count int64
		if err := tx.Table("build_attempts AS a").Joins("JOIN builds b ON b.id = a.build_id AND b.workspace_id = a.workspace_id AND b.application_id = a.application_id").Where("a.id = ? AND a.build_id = ? AND a.workspace_id = ? AND a.application_id = ? AND a.node_id = ? AND a.worker_id = ? AND a.occupied = TRUE AND b.source_snapshot_id = ? AND b.desired_state = 'RUN' AND b.state IN ('STARTING','RUNNING','RECONCILING') AND b.deadline_at > NOW()", value.AttemptID, value.BuildID, value.WorkspaceID, value.ApplicationID, value.NodeID, value.WorkerID, value.SnapshotID).
			Where("EXISTS (SELECT 1 FROM worker_certificates c WHERE c.node_id = a.node_id AND c.worker_id = a.worker_id AND c.revoked_at IS NULL AND c.not_before <= NOW() AND c.not_after > NOW())").
			Where("EXISTS (SELECT 1 FROM execution_nodes n WHERE n.id = a.node_id AND n.worker_id = a.worker_id AND n.status IN ('ACTIVE','DRAINING'))").
			Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return authorization.ErrUnauthenticated
		}
		var snapshot snapshotRecord
		if err := tx.First(&snapshot, "id = ? AND workspace_id = ? AND application_id = ? AND state = 'READY'", value.SnapshotID, value.WorkspaceID, value.ApplicationID).Error; err != nil {
			return grantError(err)
		}
		file, err := s.openStored(snapshot.StorageKey)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || info.Size() != snapshot.CompressedBytes {
			_ = file.Close()
			return ErrUnavailable
		}
		reader = file
		result = BuildGrant{ID: value.ID, SHA256: snapshot.SHA256, Size: snapshot.CompressedBytes, ExpiresAt: value.ExpiresAt}
		return nil
	})
	if err != nil && reader != nil {
		_ = reader.Close()
		reader = nil
	}
	return reader, result, err
}

func grantError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return authorization.ErrUnauthenticated
	}
	return err
}
