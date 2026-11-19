// Package qualification 用全局管理员登记的节点验收记录核对worker上报的能力资格。
// 契约（CapabilityVerifier/Qualification）在本包定义，worker 网关消费；域不反向依赖传输层。
package qualification

import (
	"context"
	"errors"
	"fmt"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"gorm.io/gorm"
)

var (
	// ErrReport 表示上报本身不完整（缺标识、nonce 不一致），不是登记记录不匹配。
	ErrReport = errors.New("节点资格上报无效")
	// ErrMatch 表示上报与登记验收记录不一致；网关以 QUALIFICATION_REJECTED 拒绝，节点保持不就绪。
	ErrMatch = errors.New("节点资格与登记验收记录不一致")
)

// Verifier 实现本包 CapabilityVerifier；结论失败关闭，不做任何自报即信。
type Verifier struct {
	db *gorm.DB
}

func NewVerifier(db *gorm.DB) *Verifier { return &Verifier{db: db} }

// record 是 node_qualifications 的包内只读投影，不与 inventory 共享可写模型。
type record struct {
	ID              string `gorm:"column:id"`
	NodeID          string `gorm:"column:node_id"`
	WorkerID        string `gorm:"column:worker_id"`
	ClusterNodeUID  string `gorm:"column:cluster_node_uid"`
	OS              string `gorm:"column:os"`
	Arch            string `gorm:"column:arch"`
	BuildReady      bool   `gorm:"column:build_ready"`
	RuntimeReady    bool   `gorm:"column:runtime_ready"`
	IngressReady    bool   `gorm:"column:ingress_ready"`
	MetricsReady    bool   `gorm:"column:metrics_ready"`
	QualificationID string `gorm:"column:qualification_id"`
}

// Verify 按上报的 qualification_id 查活动登记记录，核对连接身份、平台事实与
// 四个能力位全部一致后返回登记值；任何缺失或不一致都返回错误。
func (v *Verifier) Verify(ctx context.Context, identity enrollment.Identity, nonce string, report *platformv1.WorkerQualification) (Qualification, error) {
	// presence.go 已把上报nonce与会话nonce比对；这里仍防御性校验非空与一致，防止旁路调用。
	if report == nil || report.QualificationId == "" || nonce == "" || report.Nonce != nonce {
		return Qualification{}, fmt.Errorf("%w: 缺少资格标识或nonce不一致", ErrReport)
	}
	if identity.NodeID == "" || identity.WorkerID == "" {
		return Qualification{}, fmt.Errorf("%w: 连接身份不完整", ErrReport)
	}
	// 只认可活动记录，并核对记录归属的节点仍处于可连接状态；
	// node_id/worker_id 同时作为查询条件，跨节点或跨worker复用资格直接失败。
	value, err := findActiveQualification(v.db, ctx, report.QualificationId, identity.NodeID, identity.WorkerID)
	if err != nil {
		return Qualification{}, err
	}
	if value.ID == "" {
		return Qualification{}, fmt.Errorf("%w: 未找到匹配的活动验收记录 %s", ErrMatch, report.QualificationId)
	}
	mismatch := ""
	switch {
	case value.ClusterNodeUID != report.ClusterNodeUid:
		mismatch = "cluster_node_uid"
	case value.OS != report.Os:
		mismatch = "os"
	case value.Arch != report.Arch:
		mismatch = "arch"
	case value.BuildReady != report.Build:
		mismatch = "build"
	case value.RuntimeReady != report.Runtime:
		mismatch = "runtime"
	case value.IngressReady != report.Ingress:
		mismatch = "ingress"
	case value.MetricsReady != report.Metrics:
		mismatch = "metrics"
	}
	if mismatch != "" {
		return Qualification{}, fmt.Errorf("%w: %s 与登记记录不一致", ErrMatch, mismatch)
	}
	// 返回值取自登记记录而非上报值；worker 自报布尔不构成资格。
	return Qualification{ClusterNodeUID: value.ClusterNodeUID, OS: value.OS, Arch: value.Arch, Build: value.BuildReady, Runtime: value.RuntimeReady, Ingress: value.IngressReady, Metrics: value.MetricsReady}, nil
}
