package transport

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/oauth"
	"go.uber.org/zap"
)

// OAuthAdminHandler 暴露实例级 OAuth 客户端管理 RPC；server.go 负责路由接线。
type OAuthAdminHandler struct {
	Service *oauth.Service
	Log     *zap.Logger
}

func NewOAuthAdminHandler(service *oauth.Service, log *zap.Logger) (string, http.Handler) {
	return platformv1connect.NewOAuthAdminServiceHandler(&OAuthAdminHandler{service, log}, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(256<<10), connect.WithInterceptors(ValidationInterceptor()))
}

// oauthAdminError 在通用映射之上补充 oauth.ErrClient 语义：管理入口中该错误
// 只表示提交的客户端元数据校验失败（回调地址、范围、资源或字段超限），归 InvalidArgument。
func oauthAdminError(log *zap.Logger, err error) error {
	if errors.Is(err, oauth.ErrClient) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return RPCError(log, err)
}

func oauthClientMessage(v oauth.ClientView) *platformv1.OAuthClient {
	return &platformv1.OAuthClient{ClientId: v.ID, Name: v.Name, Source: v.Source, RedirectUris: v.RedirectURIs, Scopes: v.Scopes, Resources: v.Resources, Active: v.Active, CreatedAt: timestamp(v.CreatedAt)}
}

func (h *OAuthAdminHandler) RegisterOAuthClient(ctx context.Context, r *connect.Request[platformv1.RegisterOAuthClientRequest]) (*connect.Response[platformv1.RegisterOAuthClientResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, oauthAdminError(h.Log, e)
	}
	v, e := h.Service.RegisterClientView(ctx, p, oauth.ClientInput{ID: r.Msg.ClientId, Name: r.Msg.Name, RedirectURIs: r.Msg.RedirectUris, Scopes: r.Msg.Scopes, Resources: r.Msg.Resources})
	if e != nil {
		return nil, oauthAdminError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.RegisterOAuthClientResponse{Client: oauthClientMessage(v)}), nil
}

func (h *OAuthAdminHandler) ListOAuthClients(ctx context.Context, r *connect.Request[platformv1.ListOAuthClientsRequest]) (*connect.Response[platformv1.ListOAuthClientsResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, oauthAdminError(h.Log, e)
	}
	v, e := h.Service.ListClients(ctx, p, int(r.Msg.Limit), r.Msg.Cursor)
	if e != nil {
		return nil, oauthAdminError(h.Log, e)
	}
	o := &platformv1.ListOAuthClientsResponse{NextCursor: v.NextCursor}
	for _, x := range v.Items {
		o.Clients = append(o.Clients, oauthClientMessage(x))
	}
	return connect.NewResponse(o), nil
}

func (h *OAuthAdminHandler) SetOAuthClientActive(ctx context.Context, r *connect.Request[platformv1.SetOAuthClientActiveRequest]) (*connect.Response[platformv1.SetOAuthClientActiveResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, oauthAdminError(h.Log, e)
	}
	v, e := h.Service.SetClientActiveView(ctx, p, r.Msg.ClientId, r.Msg.Active)
	if e != nil {
		return nil, oauthAdminError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.SetOAuthClientActiveResponse{Client: oauthClientMessage(v)}), nil
}
