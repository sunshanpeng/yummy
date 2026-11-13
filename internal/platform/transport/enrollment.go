package transport

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/enrollment"
	"go.uber.org/zap"
)

// EnrollmentHandler 仅管理登记凭证；证书签发/轮换从独立TLS适配器的真实连接身份取得。
type EnrollmentHandler struct {
	Service *enrollment.Service
	Log     *zap.Logger
}

func NewEnrollmentHandler(service *enrollment.Service, log *zap.Logger) (string, http.Handler) {
	return platformv1connect.NewEnrollmentServiceHandler(&EnrollmentHandler{service, log}, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))
}
func (h *EnrollmentHandler) CreateNodeRegistration(ctx context.Context, r *connect.Request[platformv1.CreateNodeRegistrationRequest]) (*connect.Response[platformv1.CreateNodeRegistrationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if h.Service == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("节点签发CA尚未配置"))
	}
	v, err := h.Service.CreateRegistration(ctx, p, r.Msg.NodeId)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateNodeRegistrationResponse{Id: v.ID, NodeId: v.NodeID, Token: v.Token, ExpiresAt: timestamp(v.ExpiresAt)}), nil
}
func (h *EnrollmentHandler) RevokeWorkerCertificate(ctx context.Context, r *connect.Request[platformv1.RevokeWorkerCertificateRequest]) (*connect.Response[platformv1.RevokeWorkerCertificateResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if h.Service == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("节点签发CA尚未配置"))
	}
	if err := h.Service.RevokeCertificate(ctx, p, r.Msg.Serial); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.RevokeWorkerCertificateResponse{}), nil
}
func (h *EnrollmentHandler) DisableExecutionNode(ctx context.Context, r *connect.Request[platformv1.DisableExecutionNodeRequest]) (*connect.Response[platformv1.DisableExecutionNodeResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if h.Service == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("节点签发CA尚未配置"))
	}
	if err := h.Service.DisableNode(ctx, p, r.Msg.NodeId); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.DisableExecutionNodeResponse{}), nil
}
func (h *EnrollmentHandler) EnableExecutionNode(ctx context.Context, r *connect.Request[platformv1.EnableExecutionNodeRequest]) (*connect.Response[platformv1.EnableExecutionNodeResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if h.Service == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("节点签发CA尚未配置"))
	}
	if err := h.Service.EnableNode(ctx, p, r.Msg.NodeId); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.EnableExecutionNodeResponse{}), nil
}
