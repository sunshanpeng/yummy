package transport

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"go.uber.org/zap"
)

type DeploymentHandler struct {
	Service *deployment.Service
	Log     *zap.Logger
}

func NewDeploymentHandler(service *deployment.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewDeploymentServiceHandler(&DeploymentHandler{service, log}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}

func deploymentError(log *zap.Logger, err error) error {
	if errors.Is(err, deployment.ErrNotReady) || errors.Is(err, deployment.ErrVerification) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return RPCError(log, err)
}

func deploymentMessage(v deployment.Result) *platformv1.Deployment {
	r, o := v.Release, v.Operation
	return &platformv1.Deployment{
		Release: &platformv1.Release{
			Id: r.ID, WorkspaceId: r.WorkspaceID, ApplicationId: r.ApplicationID,
			Artifact:              &platformv1.ReleaseArtifact{Id: r.Artifact.ID, RegistryId: r.Artifact.RegistryID, CredentialVersion: r.Artifact.CredentialVersion, Repository: r.Artifact.Repository, Digest: r.Artifact.Digest, Platform: r.Artifact.Platform},
			RuntimeConfigRevision: r.RuntimeConfigRevision,
			Placement:             &platformv1.Placement{NodeId: r.Placement.NodeID, PolicyRevision: r.Placement.PolicyRevision, RuntimeType: r.Placement.RuntimeType, DefaultDomain: r.Placement.DefaultDomain, CreatedAt: timestamp(r.Placement.CreatedAt)},
			Domains:               r.Domains, PreviousServingId: r.PreviousServingID, RollbackOf: r.RollbackOf, Source: r.Source, ActorId: r.ActorID, CreatedAt: timestamp(r.CreatedAt),
		},
		Operation: &platformv1.DeploymentOperation{Id: o.ID, WorkspaceId: o.WorkspaceID, ApplicationId: o.ApplicationID, ReleaseId: o.ReleaseID, Generation: o.Generation, Action: o.Action, State: o.State, Active: o.Active, ActorId: o.ActorID, ErrorCode: o.ErrorCode, RecoveryOf: o.RecoveryOf, CreatedAt: timestamp(o.CreatedAt), FinishedAt: optionalTimestamp(o.FinishedAt)},
	}
}

func expectedRuntime(v *platformv1.ExpectedRuntime) (deployment.Expected, error) {
	if v == nil {
		return deployment.Expected{}, authorization.ErrInvalid
	}
	return deployment.Expected{Generation: v.Generation, ServingReleaseID: v.ServingReleaseId, ConfigRevision: v.ConfigRevision}, nil
}

func (h *DeploymentHandler) CreateDeployment(ctx context.Context, r *connect.Request[platformv1.CreateDeploymentRequest]) (*connect.Response[platformv1.CreateDeploymentResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	expected, err := expectedRuntime(r.Msg.Expected)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.CreateDeployment(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, deployment.CreateInput{ArtifactID: r.Msg.ArtifactId, RuntimeConfigRevision: r.Msg.RuntimeConfigRevision, ExpectedPolicyRevision: r.Msg.ExpectedPolicyRevision, Expected: expected, IdempotencyKey: r.Msg.IdempotencyKey})
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateDeploymentResponse{Deployment: deploymentMessage(v)}), nil
}

func (h *DeploymentHandler) GetDeployment(ctx context.Context, r *connect.Request[platformv1.GetDeploymentRequest]) (*connect.Response[platformv1.GetDeploymentResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.Get(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.OperationId)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.GetDeploymentResponse{Deployment: deploymentMessage(v)}), nil
}

func (h *DeploymentHandler) ListDeployments(ctx context.Context, r *connect.Request[platformv1.ListDeploymentsRequest]) (*connect.Response[platformv1.ListDeploymentsResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.List(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, int(r.Msg.Limit), r.Msg.Cursor)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	result := &platformv1.ListDeploymentsResponse{NextCursor: v.NextCursor}
	for _, item := range v.Items {
		result.Deployments = append(result.Deployments, deploymentMessage(item))
	}
	return connect.NewResponse(result), nil
}

func (h *DeploymentHandler) GetRuntimeStatus(ctx context.Context, r *connect.Request[platformv1.GetRuntimeStatusRequest]) (*connect.Response[platformv1.GetRuntimeStatusResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.GetStatus(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.GetRuntimeStatusResponse{Status: &platformv1.RuntimeStatus{WorkspaceId: v.WorkspaceID, ApplicationId: v.ApplicationID, Generation: v.Generation, DesiredState: v.DesiredState, DesiredReleaseId: v.DesiredReleaseID, ServingReleaseId: v.ServingReleaseID, ServingGeneration: v.ServingGeneration, ObservedGeneration: v.ObservedGeneration, ObservedState: v.ObservedState, ActiveOperationId: v.ActiveOperationID, LastObservedAt: optionalTimestamp(v.LastObservedAt)}}), nil
}

func (h *DeploymentHandler) RollbackDeployment(ctx context.Context, r *connect.Request[platformv1.RollbackDeploymentRequest]) (*connect.Response[platformv1.RollbackDeploymentResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	expected, err := expectedRuntime(r.Msg.Expected)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.Rollback(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, deployment.RollbackInput{TargetReleaseID: r.Msg.TargetReleaseId, ExpectedPolicyRevision: r.Msg.ExpectedPolicyRevision, Expected: expected, IdempotencyKey: r.Msg.IdempotencyKey})
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.RollbackDeploymentResponse{Deployment: deploymentMessage(v)}), nil
}

func (h *DeploymentHandler) ChangeRuntime(ctx context.Context, r *connect.Request[platformv1.ChangeRuntimeRequest]) (*connect.Response[platformv1.ChangeRuntimeResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	expected, err := expectedRuntime(r.Msg.Expected)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	input := deployment.ChangeInput{Expected: expected, IdempotencyKey: r.Msg.IdempotencyKey}
	var result deployment.Result
	switch r.Msg.Action {
	case deployment.Start:
		result, err = h.Service.Start(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, input)
	case deployment.Stop:
		result, err = h.Service.Stop(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, input)
	case deployment.Restart:
		result, err = h.Service.Restart(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, input)
	default:
		err = authorization.ErrInvalid
	}
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.ChangeRuntimeResponse{Deployment: deploymentMessage(result)}), nil
}

// runtimeObservationTime 解析可选的 RFC3339 边界；空字符串表示不设界。
func runtimeObservationTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, authorization.ErrInvalid
	}
	return parsed, nil
}

func (h *DeploymentHandler) GetRuntimeLogs(ctx context.Context, r *connect.Request[platformv1.GetRuntimeLogsRequest]) (*connect.Response[platformv1.GetRuntimeLogsResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.ListRuntimeLogs(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, int(r.Msg.Limit), r.Msg.Cursor, r.Msg.PodUid, r.Msg.Container)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	response := &platformv1.GetRuntimeLogsResponse{NextCursor: v.NextCursor}
	for _, item := range v.Items {
		response.Chunks = append(response.Chunks, &platformv1.RuntimeLogChunk{Id: item.ID, WorkspaceId: item.WorkspaceID, ApplicationId: item.ApplicationID, PodUid: item.PodUID, Container: item.Container, RestartCount: item.RestartCount, Seq: item.Seq, Content: item.Content, Truncated: item.Truncated, CreatedAt: timestamp(item.CreatedAt)})
	}
	return connect.NewResponse(response), nil
}

func (h *DeploymentHandler) GetRuntimeMetrics(ctx context.Context, r *connect.Request[platformv1.GetRuntimeMetricsRequest]) (*connect.Response[platformv1.GetRuntimeMetricsResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	since, err := runtimeObservationTime(r.Msg.Since)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	until, err := runtimeObservationTime(r.Msg.Until)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	v, err := h.Service.ListRuntimeMetrics(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, since, until)
	if err != nil {
		return nil, deploymentError(h.Log, err)
	}
	response := &platformv1.GetRuntimeMetricsResponse{}
	for _, item := range v {
		response.Samples = append(response.Samples, &platformv1.RuntimeMetricSample{Id: item.ID, WorkspaceId: item.WorkspaceID, ApplicationId: item.ApplicationID, ReleaseId: item.ReleaseID, Generation: item.Generation, PodUid: item.PodUID, CpuNanoCores: item.CPUNanoCores, CpuWindowMillis: item.CPUWindowMillis, MemoryWorkingSetBytes: item.MemoryWorkingSetBytes, CollectedAt: timestamp(item.CollectedAt)})
	}
	return connect.NewResponse(response), nil
}
