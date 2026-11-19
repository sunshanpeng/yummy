package transport

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/runtimeconfig"
	"go.uber.org/zap"
)

type RuntimeConfigHandler struct {
	Service *runtimeconfig.Service
	Log     *zap.Logger
}

func NewRuntimeConfigHandler(service *runtimeconfig.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewRuntimeConfigServiceHandler(&RuntimeConfigHandler{service, log}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(4<<20), connect.WithInterceptors(ValidationInterceptor()))...)
}
func runtimeMessage(v runtimeconfig.View) *platformv1.RuntimeConfig {
	result := &platformv1.RuntimeConfig{WorkspaceId: v.WorkspaceID, ApplicationId: v.ApplicationID, Revision: v.Revision, Confirmed: v.Confirmed, Type: v.Type, Port: v.Port, Command: v.Command, Args: v.Args, Domains: v.Domains, CreatedAt: timestamp(v.CreatedAt),
		Probe:     &platformv1.RuntimeProbe{Path: v.Probe.Path, InitialDelaySeconds: v.Probe.InitialDelaySeconds, PeriodSeconds: v.Probe.PeriodSeconds, TimeoutSeconds: v.Probe.TimeoutSeconds, FailureThreshold: v.Probe.FailureThreshold},
		Resources: &platformv1.RuntimeResources{CpuRequestMilli: v.Resources.CPURequestMilli, CpuLimitMilli: v.Resources.CPULimitMilli, MemoryRequestMib: v.Resources.MemoryRequestMiB, MemoryLimitMib: v.Resources.MemoryLimitMiB}}
	for _, variable := range v.Variables {
		result.Variables = append(result.Variables, &platformv1.RuntimeVariable{Name: variable.Name, Value: variable.Value, Sensitive: variable.Sensitive, Configured: variable.Configured})
	}
	for _, file := range v.Files {
		result.Files = append(result.Files, &platformv1.RuntimeFile{Path: file.Path, Content: file.Content, Sensitive: file.Sensitive, Configured: file.Configured})
	}
	for _, volume := range v.Volumes {
		result.Volumes = append(result.Volumes, &platformv1.RuntimeVolume{Name: volume.Name, MountPath: volume.MountPath, SizeGib: volume.SizeGiB})
	}
	return result
}
func (h *RuntimeConfigHandler) GetRuntimeConfig(ctx context.Context, req *connect.Request[platformv1.GetRuntimeConfigRequest]) (*connect.Response[platformv1.GetRuntimeConfigResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	var value runtimeconfig.View
	if req.Msg.Revision == 0 {
		value, err = h.Service.Get(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId)
	} else {
		value, err = h.Service.GetRevision(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.Revision)
	}
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.GetRuntimeConfigResponse{Config: runtimeMessage(value)}), nil
}
func (h *RuntimeConfigHandler) SaveRuntimeConfig(ctx context.Context, req *connect.Request[platformv1.SaveRuntimeConfigRequest]) (*connect.Response[platformv1.SaveRuntimeConfigResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	msg := req.Msg
	input := runtimeconfig.Input{Type: msg.Type, Port: msg.Port, ExpectedRevision: msg.ExpectedRevision, Confirmed: msg.Confirmed, IdempotencyKey: msg.IdempotencyKey, Command: msg.Command, Args: msg.Args, Domains: msg.Domains}
	if msg.Probe != nil {
		input.Probe = runtimeconfig.Probe{Path: msg.Probe.Path, InitialDelaySeconds: msg.Probe.InitialDelaySeconds, PeriodSeconds: msg.Probe.PeriodSeconds, TimeoutSeconds: msg.Probe.TimeoutSeconds, FailureThreshold: msg.Probe.FailureThreshold}
	}
	if msg.Resources != nil {
		input.Resources = runtimeconfig.Resources{CPURequestMilli: msg.Resources.CpuRequestMilli, CPULimitMilli: msg.Resources.CpuLimitMilli, MemoryRequestMiB: msg.Resources.MemoryRequestMib, MemoryLimitMiB: msg.Resources.MemoryLimitMib}
	}
	for _, v := range msg.Variables {
		input.Variables = append(input.Variables, runtimeconfig.VariableInput{Name: v.Name, Value: v.Value, Sensitive: v.Sensitive, KeepExisting: v.KeepExisting})
	}
	for _, f := range msg.Files {
		input.Files = append(input.Files, runtimeconfig.FileInput{Path: f.Path, Content: f.Content, Sensitive: f.Sensitive, KeepExisting: f.KeepExisting})
	}
	for _, v := range msg.Volumes {
		input.Volumes = append(input.Volumes, runtimeconfig.Volume{Name: v.Name, MountPath: v.MountPath, SizeGiB: v.SizeGib})
	}
	value, err := h.Service.Save(ctx, p, msg.WorkspaceId, msg.ApplicationId, input)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.SaveRuntimeConfigResponse{Config: runtimeMessage(value)}), nil
}
