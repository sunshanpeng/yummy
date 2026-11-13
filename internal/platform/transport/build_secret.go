package transport

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/buildsecret"
	"go.uber.org/zap"
)

type BuildSecretHandler struct {
	Service *buildsecret.Service
	Log     *zap.Logger
}

func NewBuildSecretHandler(service *buildsecret.Service, log *zap.Logger) (string, http.Handler) {
	return platformv1connect.NewBuildSecretServiceHandler(&BuildSecretHandler{service, log}, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))
}
func buildSecretMessage(v buildsecret.View) *platformv1.BuildSecret {
	return &platformv1.BuildSecret{Id: v.ID, Name: v.Name, Purpose: v.Purpose, Status: v.Status, CurrentVersion: v.CurrentVersion, Configured: v.Configured, CreatedAt: timestamp(v.CreatedAt), UpdatedAt: timestamp(v.UpdatedAt)}
}
func (h *BuildSecretHandler) ListBuildSecrets(ctx context.Context, r *connect.Request[platformv1.ListBuildSecretsRequest]) (*connect.Response[platformv1.ListBuildSecretsResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.List(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, int(r.Msg.Limit), r.Msg.Cursor)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	result := &platformv1.ListBuildSecretsResponse{NextCursor: v.NextCursor}
	for _, secret := range v.Items {
		result.Secrets = append(result.Secrets, buildSecretMessage(secret))
	}
	return connect.NewResponse(result), nil
}
func (h *BuildSecretHandler) CreateBuildSecret(ctx context.Context, r *connect.Request[platformv1.CreateBuildSecretRequest]) (*connect.Response[platformv1.CreateBuildSecretResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Create(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, buildsecret.CreateInput{Name: r.Msg.Name, Purpose: r.Msg.Purpose, Value: r.Msg.Value})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateBuildSecretResponse{Secret: buildSecretMessage(v)}), nil
}
func (h *BuildSecretHandler) ReplaceBuildSecret(ctx context.Context, r *connect.Request[platformv1.ReplaceBuildSecretRequest]) (*connect.Response[platformv1.ReplaceBuildSecretResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Replace(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.SecretId, buildsecret.ReplaceInput{ExpectedVersion: r.Msg.ExpectedVersion, Value: r.Msg.Value})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.ReplaceBuildSecretResponse{Secret: buildSecretMessage(v)}), nil
}
func (h *BuildSecretHandler) DeactivateBuildSecret(ctx context.Context, r *connect.Request[platformv1.DeactivateBuildSecretRequest]) (*connect.Response[platformv1.DeactivateBuildSecretResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.Deactivate(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.SecretId, r.Msg.ExpectedVersion); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.DeactivateBuildSecretResponse{}), nil
}
