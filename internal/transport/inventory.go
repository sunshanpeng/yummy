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
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
	"go.uber.org/zap"
)

type InventoryHandler struct {
	Service             *inventory.Service
	Log                 *zap.Logger
	ReadClusterMachines func(string) (*platformv1.ClusterMachinesReport, time.Time)
}

func NewInventoryHandler(service *inventory.Service, log *zap.Logger, readClusterMachines func(string) (*platformv1.ClusterMachinesReport, time.Time), opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewInventoryServiceHandler(&InventoryHandler{Service: service, Log: log, ReadClusterMachines: readClusterMachines}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(256<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}
func inventoryError(log *zap.Logger, err error) error {
	if errors.Is(err, inventory.ErrNotReady) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return RPCError(log, err)
}
func nodeMessage(v inventory.NodeView) *platformv1.ExecutionNode {
	return &platformv1.ExecutionNode{Id: v.ID, Name: v.Name, WorkerId: v.WorkerID, Status: v.Status, ClusterNodeUid: v.ClusterNodeUID, Os: v.OS, Arch: v.Arch, ConnectedAt: optionalTimestamp(v.ConnectedAt), LastSeen: optionalTimestamp(v.LastSeen), BuildReady: v.BuildReady, RuntimeReady: v.RuntimeReady, IngressReady: v.IngressReady, MetricsReady: v.MetricsReady, ReadinessReason: v.ReadinessReason, Capacity: int32(v.Capacity), Revision: v.Revision}
}
func registryMessage(v inventory.RegistryView) *platformv1.RegistryConnection {
	return &platformv1.RegistryConnection{Id: v.ID, Name: v.Name, Url: v.URL, RepositoryPrefix: v.RepositoryPrefix, Status: v.Status, CredentialConfigured: v.CredentialConfigured, CredentialVersion: v.CredentialVersion, Revision: v.Revision, VerifiedAt: optionalTimestamp(v.VerifiedAt)}
}
func executionResources(v inventory.Resources) *platformv1.ExecutionResources {
	return &platformv1.ExecutionResources{CpuMillis: v.CPUMillis, MemoryMib: v.MemoryMiB}
}
func policyMessage(v inventory.PolicyView) *platformv1.ExecutionPolicy {
	return &platformv1.ExecutionPolicy{InstanceId: v.InstanceID, Revision: v.Revision, BuildNodeId: v.BuildNodeID, RuntimeNodeId: v.RuntimeNodeID, RegistryId: v.RegistryID, RepositoryPrefix: v.RepositoryPrefix, BaseDomain: v.BaseDomain, ResourceDefault: executionResources(v.ResourceDefault), ResourceLimits: executionResources(v.ResourceLimits), AutoRecoveryEnabled: v.AutoRecoveryEnabled, CreatedAt: timestamp(v.CreatedAt)}
}
func allocationMessage(v inventory.AllocationView) *platformv1.WorkspaceAllocation {
	return &platformv1.WorkspaceAllocation{WorkspaceId: v.WorkspaceID, AllowedNodeIds: v.AllowedNodeIDs, MaxApplications: int32(v.MaxApplications), BuildConcurrency: int32(v.BuildConcurrency), CpuMillis: v.CPUMillis, MemoryMib: v.MemoryMiB, Revision: v.Revision}
}
func summaryMessage(v inventory.ExecutionSummary) *platformv1.ExecutionSummary {
	return &platformv1.ExecutionSummary{PolicyRevision: v.PolicyRevision, Platform: v.Platform, NodeId: v.NodeID, NodeName: v.NodeName, ImageRepository: v.ImageRepository, DefaultDomain: v.DefaultDomain, ResourceDefault: executionResources(v.ResourceDefault), ResourceLimits: executionResources(v.ResourceLimits), AutoRecoveryEnabled: v.AutoRecoveryEnabled, Ready: v.Ready, Reason: v.Reason}
}
func nodeQualificationMessage(v inventory.QualificationView) *platformv1.NodeQualification {
	return &platformv1.NodeQualification{Id: v.ID, NodeId: v.NodeID, WorkerId: v.WorkerID, ClusterNodeUid: v.ClusterNodeUID, Os: v.OS, Arch: v.Arch, BuildReady: v.BuildReady, RuntimeReady: v.RuntimeReady, IngressReady: v.IngressReady, MetricsReady: v.MetricsReady, QualificationId: v.QualificationID, Status: v.Status, CreatedBy: v.CreatedBy, CreatedAt: timestamp(v.CreatedAt), RevokedAt: optionalTimestamp(v.RevokedAt)}
}
func domainTLSSecretMessage(v inventory.DomainTLSView) *platformv1.DomainTLSSecret {
	return &platformv1.DomainTLSSecret{Domain: v.Domain, SecretName: v.SecretName}
}
func (h *InventoryHandler) ListClusterMachines(ctx context.Context, _ *connect.Request[platformv1.ListClusterMachinesRequest]) (*connect.Response[platformv1.ListClusterMachinesResponse], error) {
	principal, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, inventoryError(h.Log, err)
	}
	policy, err := h.Service.GetPolicy(ctx, principal, 0)
	result := &platformv1.ListClusterMachinesResponse{}
	if errors.Is(err, authorization.ErrNotFound) {
		result.Reason = "请先配置平台运行节点。"
		return connect.NewResponse(result), nil
	}
	if err != nil {
		return nil, inventoryError(h.Log, err)
	}
	if h.ReadClusterMachines == nil {
		result.Reason = "控制面尚未启用 worker 网关。"
		return connect.NewResponse(result), nil
	}
	report, observed := h.ReadClusterMachines(policy.RuntimeNodeID)
	switch {
	case report == nil:
		result.Reason = "等待策略运行节点的 worker 上报集群机器。"
	case report.ErrorCode == "FORBIDDEN":
		result.Reason = "worker 缺少 Kubernetes 节点列表的只读权限。"
	case report.ErrorCode == "UNAVAILABLE":
		result.Reason = "worker 暂时无法读取 Kubernetes 节点列表。"
	default:
		result.Machines, result.Truncated = report.Machines, report.Truncated
		result.ObservedAt = timestamp(observed)
	}
	return connect.NewResponse(result), nil
}
func (h *InventoryHandler) ListNodes(ctx context.Context, r *connect.Request[platformv1.ListNodesRequest]) (*connect.Response[platformv1.ListNodesResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.ListNodes(ctx, p, int(r.Msg.Limit), r.Msg.Cursor)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	o := &platformv1.ListNodesResponse{NextCursor: v.NextCursor}
	for _, x := range v.Items {
		o.Nodes = append(o.Nodes, nodeMessage(x))
	}
	return connect.NewResponse(o), nil
}
func (h *InventoryHandler) GetNode(ctx context.Context, r *connect.Request[platformv1.GetNodeRequest]) (*connect.Response[platformv1.GetNodeResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.GetNode(ctx, p, r.Msg.NodeId)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetNodeResponse{Node: nodeMessage(v)}), nil
}
func (h *InventoryHandler) CreateNode(ctx context.Context, r *connect.Request[platformv1.CreateNodeRequest]) (*connect.Response[platformv1.CreateNodeResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.CreateNode(ctx, p, inventory.NodeInput{Name: r.Msg.Name, WorkerID: r.Msg.WorkerId, Capacity: int(r.Msg.Capacity)})
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.CreateNodeResponse{Node: nodeMessage(v)}), nil
}
func (h *InventoryHandler) UpdateNode(ctx context.Context, r *connect.Request[platformv1.UpdateNodeRequest]) (*connect.Response[platformv1.UpdateNodeResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.UpdateNode(ctx, p, r.Msg.NodeId, inventory.UpdateNodeInput{Name: r.Msg.Name, Status: r.Msg.Status, Capacity: int(r.Msg.Capacity), ExpectedRevision: r.Msg.ExpectedRevision})
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.UpdateNodeResponse{Node: nodeMessage(v)}), nil
}
func (h *InventoryHandler) ListRegistries(ctx context.Context, r *connect.Request[platformv1.ListRegistriesRequest]) (*connect.Response[platformv1.ListRegistriesResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.ListRegistries(ctx, p, int(r.Msg.Limit), r.Msg.Cursor)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	o := &platformv1.ListRegistriesResponse{NextCursor: v.NextCursor}
	for _, x := range v.Items {
		o.Registries = append(o.Registries, registryMessage(x))
	}
	return connect.NewResponse(o), nil
}
func (h *InventoryHandler) GetRegistry(ctx context.Context, r *connect.Request[platformv1.GetRegistryRequest]) (*connect.Response[platformv1.GetRegistryResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.GetRegistry(ctx, p, r.Msg.RegistryId)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetRegistryResponse{Registry: registryMessage(v)}), nil
}
func (h *InventoryHandler) CreateRegistry(ctx context.Context, r *connect.Request[platformv1.CreateRegistryRequest]) (*connect.Response[platformv1.CreateRegistryResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.CreateRegistry(ctx, p, inventory.RegistryInput{Name: r.Msg.Name, URL: r.Msg.Url, RepositoryPrefix: r.Msg.RepositoryPrefix, Username: r.Msg.Username, Password: r.Msg.Password})
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.CreateRegistryResponse{Registry: registryMessage(v)}), nil
}
func (h *InventoryHandler) UpdateRegistry(ctx context.Context, r *connect.Request[platformv1.UpdateRegistryRequest]) (*connect.Response[platformv1.UpdateRegistryResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.UpdateRegistry(ctx, p, r.Msg.RegistryId, inventory.UpdateRegistryInput{Name: r.Msg.Name, URL: r.Msg.Url, RepositoryPrefix: r.Msg.RepositoryPrefix, Status: r.Msg.Status, ExpectedRevision: r.Msg.ExpectedRevision, Username: r.Msg.Username, Password: r.Msg.Password})
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.UpdateRegistryResponse{Registry: registryMessage(v)}), nil
}
func (h *InventoryHandler) GetExecutionPolicy(ctx context.Context, r *connect.Request[platformv1.GetExecutionPolicyRequest]) (*connect.Response[platformv1.GetExecutionPolicyResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.GetPolicy(ctx, p, r.Msg.Revision)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetExecutionPolicyResponse{Policy: policyMessage(v)}), nil
}
func (h *InventoryHandler) SaveExecutionPolicy(ctx context.Context, r *connect.Request[platformv1.SaveExecutionPolicyRequest]) (*connect.Response[platformv1.SaveExecutionPolicyResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	m := r.Msg
	i := inventory.PolicyInput{BuildNodeID: m.BuildNodeId, RuntimeNodeID: m.RuntimeNodeId, RegistryID: m.RegistryId, RepositoryPrefix: m.RepositoryPrefix, BaseDomain: m.BaseDomain, ExpectedRevision: m.ExpectedRevision, AutoRecoveryEnabled: m.AutoRecoveryEnabled}
	if m.ResourceDefault != nil {
		i.ResourceDefault = inventory.Resources{CPUMillis: m.ResourceDefault.CpuMillis, MemoryMiB: m.ResourceDefault.MemoryMib}
	}
	if m.ResourceLimits != nil {
		i.ResourceLimits = inventory.Resources{CPUMillis: m.ResourceLimits.CpuMillis, MemoryMiB: m.ResourceLimits.MemoryMib}
	}
	v, e := h.Service.SavePolicy(ctx, p, i)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.SaveExecutionPolicyResponse{Policy: policyMessage(v)}), nil
}
func (h *InventoryHandler) GetWorkspaceAllocation(ctx context.Context, r *connect.Request[platformv1.GetWorkspaceAllocationRequest]) (*connect.Response[platformv1.GetWorkspaceAllocationResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.GetAllocation(ctx, p, r.Msg.WorkspaceId)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetWorkspaceAllocationResponse{Allocation: allocationMessage(v)}), nil
}
func (h *InventoryHandler) SetWorkspaceAllocation(ctx context.Context, r *connect.Request[platformv1.SetWorkspaceAllocationRequest]) (*connect.Response[platformv1.SetWorkspaceAllocationResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	m := r.Msg
	v, e := h.Service.SetAllocation(ctx, p, m.WorkspaceId, inventory.AllocationInput{AllowedNodeIDs: m.AllowedNodeIds, MaxApplications: int(m.MaxApplications), BuildConcurrency: int(m.BuildConcurrency), CPUMillis: m.CpuMillis, MemoryMiB: m.MemoryMib, ExpectedRevision: m.ExpectedRevision})
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.SetWorkspaceAllocationResponse{Allocation: allocationMessage(v)}), nil
}
func (h *InventoryHandler) ResolveExecution(ctx context.Context, r *connect.Request[platformv1.ResolveExecutionRequest]) (*connect.Response[platformv1.ResolveExecutionResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	var v inventory.ExecutionSummary
	if r.Msg.Runtime {
		v, e = h.Service.ResolveRuntime(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.ExpectedPolicyRevision)
	} else {
		v, e = h.Service.ResolveBuild(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.ExpectedPolicyRevision)
	}
	if errors.Is(e, inventory.ErrNotReady) {
		if v.Reason == "" {
			v.Reason = e.Error()
		}
		return connect.NewResponse(&platformv1.ResolveExecutionResponse{Summary: summaryMessage(v)}), nil
	}
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.ResolveExecutionResponse{Summary: summaryMessage(v)}), nil
}
func (h *InventoryHandler) RegisterNodeQualification(ctx context.Context, r *connect.Request[platformv1.RegisterNodeQualificationRequest]) (*connect.Response[platformv1.RegisterNodeQualificationResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	evidence := make(map[string]string, len(r.Msg.Evidence))
	for key, value := range r.Msg.Evidence {
		evidence[key] = value
	}
	v, e := h.Service.RegisterQualification(ctx, p, inventory.QualificationInput{NodeID: r.Msg.NodeId, WorkerID: r.Msg.WorkerId, ClusterNodeUID: r.Msg.ClusterNodeUid, OS: r.Msg.Os, Arch: r.Msg.Arch, QualificationID: r.Msg.QualificationId, Build: r.Msg.BuildReady, Runtime: r.Msg.RuntimeReady, Ingress: r.Msg.IngressReady, Metrics: r.Msg.MetricsReady, Evidence: evidence})
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.RegisterNodeQualificationResponse{Qualification: nodeQualificationMessage(v)}), nil
}
func (h *InventoryHandler) RevokeNodeQualification(ctx context.Context, r *connect.Request[platformv1.RevokeNodeQualificationRequest]) (*connect.Response[platformv1.RevokeNodeQualificationResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.RevokeQualification(ctx, p, r.Msg.Id)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.RevokeNodeQualificationResponse{Qualification: nodeQualificationMessage(v)}), nil
}
func (h *InventoryHandler) ListNodeQualifications(ctx context.Context, r *connect.Request[platformv1.ListNodeQualificationsRequest]) (*connect.Response[platformv1.ListNodeQualificationsResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.ListQualifications(ctx, p, int(r.Msg.Limit), r.Msg.Cursor)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	o := &platformv1.ListNodeQualificationsResponse{NextCursor: v.NextCursor}
	for _, x := range v.Items {
		o.Qualifications = append(o.Qualifications, nodeQualificationMessage(x))
	}
	return connect.NewResponse(o), nil
}

func (h *InventoryHandler) SetDomainTLSSecret(ctx context.Context, r *connect.Request[platformv1.SetDomainTLSSecretRequest]) (*connect.Response[platformv1.SetDomainTLSSecretResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.SetDomainTLSSecret(ctx, p, r.Msg.Domain, r.Msg.SecretName)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.SetDomainTLSSecretResponse{Binding: domainTLSSecretMessage(v)}), nil
}

func (h *InventoryHandler) GetDomainTLSSecret(ctx context.Context, r *connect.Request[platformv1.GetDomainTLSSecretRequest]) (*connect.Response[platformv1.GetDomainTLSSecretResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	v, e := h.Service.GetDomainTLSSecret(ctx, p, r.Msg.Domain)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetDomainTLSSecretResponse{Binding: domainTLSSecretMessage(v)}), nil
}

func (h *InventoryHandler) VerifyRegistry(ctx context.Context, r *connect.Request[platformv1.VerifyRegistryRequest]) (*connect.Response[platformv1.VerifyRegistryResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	registry, probe, ok, e := h.Service.VerifyRegistry(ctx, p, r.Msg.RegistryId, r.Msg.ExpectedRevision, r.Msg.ReadReference)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	response := &platformv1.VerifyRegistryResponse{Ok: ok, Message: probe.Message, NetworkReachable: probe.NetworkReachable, AuthenticationAccepted: probe.AuthenticationAccepted, ReadVerified: probe.ReadVerified, CheckedAt: timestamp(probe.CheckedAt)}
	if ok {
		response.Registry = registryMessage(registry)
	}
	return connect.NewResponse(response), nil
}

func (h *InventoryHandler) GetInstanceSettings(ctx context.Context, _ *connect.Request[platformv1.GetInstanceSettingsRequest]) (*connect.Response[platformv1.GetInstanceSettingsResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	enabled, e := h.Service.GetInstanceSettings(ctx, p)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetInstanceSettingsResponse{WorkspaceSelfCreate: enabled}), nil
}

func (h *InventoryHandler) SetWorkspaceSelfCreate(ctx context.Context, r *connect.Request[platformv1.SetWorkspaceSelfCreateRequest]) (*connect.Response[platformv1.SetWorkspaceSelfCreateResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	enabled, e := h.Service.SetWorkspaceSelfCreate(ctx, p, r.Msg.Enabled)
	if e != nil {
		return nil, inventoryError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.SetWorkspaceSelfCreateResponse{WorkspaceSelfCreate: enabled}), nil
}
