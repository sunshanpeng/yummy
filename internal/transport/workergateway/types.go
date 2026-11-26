// Package workergateway 只装配认证worker流和类型化业务适配，不创建集群或执行宿主Shell。
package workergateway

import (
	"context"
	"crypto/x509"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"github.com/sunshanpeng/yummy/internal/domains/qualification"
	"gorm.io/gorm"
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

type ApplicationCleanupBackend interface {
	ClaimCleanup(context.Context, application.CleanupWorker) (application.CleanupDispatch, error)
	CheckCleanup(context.Context, application.CleanupWorker, application.CleanupLease) error
	CompleteCleanup(context.Context, application.CleanupWorker, application.CleanupLease, application.CleanupResult) error
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
	ApplicationCleanup ApplicationCleanupBackend
	Capabilities       qualification.CapabilityVerifier
	BuildMaterial      BuildMaterializer
	DeploymentMaterial DeploymentMaterializer
	ServerID           string
	MaxSessions        int
	QueueSize          int
	ScheduleInterval   time.Duration
	OnError            func(error)
}
