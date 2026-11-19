package qualification

import (
	"context"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
)

// Qualification 是节点能力资格核对结论，worker 网关据此放行对应能力位。
type Qualification struct {
	ClusterNodeUID, OS, Arch         string
	Build, Runtime, Ingress, Metrics bool
}

// CapabilityVerifier 必须核对管理员登记qualification、nonce和真实探测证据，nil始终失败关闭。
// 契约归 qualification 域所有：worker 网关（传输层）消费，域侧实现，避免域反向依赖传输层。
type CapabilityVerifier interface {
	Verify(context.Context, enrollment.Identity, string, *platformv1.WorkerQualification) (Qualification, error)
}
