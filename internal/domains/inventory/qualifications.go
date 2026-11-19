package inventory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/lib/naming"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// qualificationIDPattern 约束worker上报的资格标识，避免把任意文本当作能力凭据。
var qualificationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func qualificationView(value qualificationRecord) QualificationView {
	return QualificationView{ID: value.ID, NodeID: value.NodeID, WorkerID: value.WorkerID, ClusterNodeUID: value.ClusterNodeUID, OS: value.OS, Arch: value.Arch, BuildReady: value.BuildReady, RuntimeReady: value.RuntimeReady, IngressReady: value.IngressReady, MetricsReady: value.MetricsReady, QualificationID: value.QualificationID, Status: value.Status, CreatedBy: value.CreatedBy, CreatedAt: value.CreatedAt, RevokedAt: value.RevokedAt}
}

func validateQualification(input QualificationInput) (QualificationInput, error) {
	if !naming.ValidWorkerID(input.WorkerID) {
		return input, invalid("worker身份无效")
	}
	input.ClusterNodeUID = strings.TrimSpace(input.ClusterNodeUID)
	if input.ClusterNodeUID == "" || len(input.ClusterNodeUID) > 256 || !utf8.ValidString(input.ClusterNodeUID) {
		return input, invalid("集群节点UID需要1～256个有效字符")
	}
	if input.OS == "" {
		input.OS = "linux"
	}
	if input.Arch == "" {
		input.Arch = "amd64"
	}
	// 网关只放行linux/amd64资格；登记其他平台必然无法就绪，提前拒绝。
	if input.OS != "linux" || input.Arch != "amd64" {
		return input, invalid("首期执行平台仅支持linux/amd64")
	}
	input.QualificationID = strings.TrimSpace(input.QualificationID)
	if !qualificationIDPattern.MatchString(input.QualificationID) {
		return input, invalid("资格标识必须是1～128位字母数字与._:-的组合")
	}
	if !input.Build && !input.Runtime && !input.Ingress && !input.Metrics {
		return input, invalid("至少登记一项已验收能力")
	}
	if len(input.Evidence) > 32 {
		return input, invalid("验收证据摘要最多32项")
	}
	for key, value := range input.Evidence {
		if key == "" || strings.TrimSpace(key) != key || len(key) > 64 || len(value) > 1024 || !utf8.ValidString(key) || !utf8.ValidString(value) {
			return input, invalid("验收证据摘要键值超长或包含无效字符")
		}
	}
	return input, nil
}

// RegisterQualification 登记安装者线下验收记录；不直接修改节点就绪状态，
// 就绪仍由worker连接后携带qualification_id通过网关核验产生。
func (s *Service) RegisterQualification(ctx context.Context, p authorization.Principal, input QualificationInput) (QualificationView, error) {
	input, err := validateQualification(input)
	if err != nil {
		return QualificationView{}, err
	}
	var result QualificationView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var node nodeRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&node, "id = ?", input.NodeID).Error; err != nil {
			return missing(err)
		}
		if node.WorkerID != input.WorkerID {
			return invalid("worker身份与登记节点不一致")
		}
		// 节点已在线核验过不同UID时拒绝登记，避免资格记录与真实节点事实冲突。
		if node.ClusterNodeUID != "" && node.ClusterNodeUID != input.ClusterNodeUID {
			return invalid("集群节点UID与节点已核验事实不一致")
		}
		// 同一节点同时只允许一条活动资格；换发必须先显式撤销旧记录。
		var active qualificationRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id = ? AND status = 'ACTIVE'", input.NodeID).Take(&active).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		} else {
			return fmt.Errorf("%w: 节点已存在活动资格，请先撤销 %s", authorization.ErrConflict, active.ID)
		}
		id, err := security.NewID("nqf")
		if err != nil {
			return err
		}
		evidence := make(map[string]any, len(input.Evidence))
		for key, value := range input.Evidence {
			evidence[key] = value
		}
		value := qualificationRecord{ID: id, NodeID: node.ID, WorkerID: node.WorkerID, ClusterNodeUID: input.ClusterNodeUID, OS: input.OS, Arch: input.Arch, BuildReady: input.Build, RuntimeReady: input.Runtime, IngressReady: input.Ingress, MetricsReady: input.Metrics, QualificationID: input.QualificationID, Evidence: evidence, Status: "ACTIVE", CreatedBy: current.UserID}
		if err := tx.Create(&value).Error; err != nil {
			return conflicting(err)
		}
		if err := audit.Record(tx, current.UserID, "instance.node.qualification.register", "node_qualification", id, nil, nil); err != nil {
			return err
		}
		result = qualificationView(value)
		return nil
	})
	return result, err
}

// RevokeQualification 按记录ID幂等撤销；不存在时失败关闭，不返回静默成功。
// 撤销立即清空节点就绪事实，已连接节点必须重新通过资格核验。
func (s *Service) RevokeQualification(ctx context.Context, p authorization.Principal, id string) (QualificationView, error) {
	var result QualificationView
	err := s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var value qualificationRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "id = ?", id).Error; err != nil {
			return missing(err)
		}
		if value.Status == "REVOKED" {
			result = qualificationView(value)
			return nil
		}
		updated := tx.Model(&qualificationRecord{}).Where("id = ? AND status = 'ACTIVE'", id).Updates(map[string]any{"status": "REVOKED", "revoked_at": time.Now().UTC()})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return authorization.ErrConflict
		}
		if err := tx.Model(&nodeRecord{}).Where("id = ?", value.NodeID).Updates(map[string]any{"build_ready": false, "runtime_ready": false, "ingress_ready": false, "metrics_ready": false, "readiness_reason": "登记资格已撤销，等待重新核验"}).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "instance.node.qualification.revoke", "node_qualification", id, nil, nil); err != nil {
			return err
		}
		if err := tx.First(&value, "id = ?", id).Error; err != nil {
			return err
		}
		result = qualificationView(value)
		return nil
	})
	return result, err
}

func (s *Service) ListQualifications(ctx context.Context, p authorization.Principal, limit int, cursor string) (QualificationPage, error) {
	result := QualificationPage{Items: make([]QualificationView, 0)}
	scope := "instance-node-qualifications:" + p.UserID
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		query := tx.Model(&qualificationRecord{})
		if after != "" {
			// 资格是验收历史，按创建顺序列出；随机 id 不能代表先后，
			// 用 (created_at, id) 复合 keyset 平局裁决，游标记录被删时无返回，不回退首页。
			query = query.Where("(created_at, id) > ((SELECT created_at FROM node_qualifications WHERE id = ?), ?)", after, after)
		}
		var values []qualificationRecord
		if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&values).Error; err != nil {
			return err
		}
		if len(values) > limit {
			result.NextCursor = store.NextCursor(scope, values[limit-1].ID)
			values = values[:limit]
		}
		for _, value := range values {
			result.Items = append(result.Items, qualificationView(value))
		}
		return nil
	})
	return result, err
}
