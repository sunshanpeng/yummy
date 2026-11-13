// Package workergateway 只装配认证worker流和类型化业务适配，不创建集群或执行宿主Shell。
package workergateway

import (
	"context"
	"crypto/x509"
	"errors"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/enrollment"
	"gorm.io/gorm"
)

var (
	ErrIdentity      = errors.New("worker连接身份无效或已被替代")
	ErrQualification = errors.New("缺少可验证的节点资格证据")
	ErrSpoolFull     = errors.New("worker持久发送日志已达到容量上限")
	ErrProtocol      = errors.New("worker协议无效")
)

type PeerVerifier interface {
	VerifyPeer(context.Context, *x509.Certificate) (enrollment.Identity, error)
}
type BuildBackend interface {
	Claim(context.Context, build.WorkerIdentity) (build.Dispatch, error)
	PendingDispatch(context.Context, build.WorkerIdentity, int) ([]build.Dispatch, error)
	ApplyEvent(context.Context, build.WorkerIdentity, build.Event) (build.Ack, error)
	AppendLogs(context.Context, build.WorkerIdentity, build.LogInput) (build.Ack, error)
}
type DeploymentBackend interface {
	ClaimPending(context.Context, deployment.WorkerIdentity) (deployment.Dispatch, error)
	ApplyObservation(context.Context, deployment.WorkerIdentity, deployment.Observation) (deployment.Ack, error)
	ApplyRuntimeLog(context.Context, deployment.WorkerIdentity, deployment.RuntimeLogInput) (deployment.Ack, error)
	ApplyRuntimeMetric(context.Context, deployment.WorkerIdentity, deployment.RuntimeMetricInput) (deployment.Ack, error)
}
type GenerationCheck struct {
	WorkspaceID, ApplicationID, OperationID, ReleaseID, ClusterNodeUID string
	Generation                                                         int64
}
type GenerationChecker interface {
	Check(context.Context, deployment.WorkerIdentity, GenerationCheck) error
}
type Qualification struct {
	ClusterNodeUID, OS, Arch         string
	Build, Runtime, Ingress, Metrics bool
}

// CapabilityVerifier 必须核对管理员登记qualification、nonce和真实探测证据，nil始终失败关闭。
type CapabilityVerifier interface {
	Verify(context.Context, enrollment.Identity, string, *platformv1.WorkerQualification) (Qualification, error)
}
type BuildMaterial struct {
	Source   *platformv1.WorkerSourceGrant
	Registry *platformv1.WorkerRegistryCredential
	Secrets  []*platformv1.WorkerBuildSecret
}
type BuildMaterializer interface {
	MaterializeBuild(context.Context, enrollment.Identity, build.Dispatch) (BuildMaterial, error)
}
type DeploymentMaterializer interface {
	MaterializeDeployment(context.Context, enrollment.Identity, *x509.Certificate, deployment.Dispatch) (*platformv1.WorkerRegistryCredential, error)
}

type Config struct {
	DB                 *gorm.DB
	Peers              PeerVerifier
	Builds             BuildBackend
	Deployments        DeploymentBackend
	Guard              GenerationChecker
	Capabilities       CapabilityVerifier
	BuildMaterial      BuildMaterializer
	DeploymentMaterial DeploymentMaterializer
	ServerID           string
	MaxSessions        int
	QueueSize          int
	ScheduleInterval   time.Duration
	OnError            func(error)
}

// Executor由worker装配层注入Kubernetes适配器，生命周期必须独立于某次流请求。
// Reconcile=true表示此执行已见过，必须核对原稳定Job身份，不能另起第二个任务。
type Executor interface {
	Build(context.Context, *platformv1.WorkerBuildCommand, bool, Emitter) error
	Deploy(context.Context, *platformv1.WorkerDeploymentCommand, bool, Emitter) error
}
type Emitter interface {
	BuildEvent(*platformv1.WorkerBuildEvent) error
	BuildLog(*platformv1.WorkerBuildLog) error
	Observation(*platformv1.WorkerDeploymentObservation) error
	// 应用运行日志与 CPU/内存采样；实现必须先持久化再发送，重传路径才能保持幂等。
	RuntimeLog(*platformv1.WorkerRuntimeLog) error
	RuntimeMetric(*platformv1.WorkerRuntimeMetric) error
}
