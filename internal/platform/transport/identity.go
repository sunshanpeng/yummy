package transport

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/identity"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/session"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type IdentityHandler struct {
	Service  *identity.Service
	Sessions *session.Manager
	Log      *zap.Logger
}

func NewIdentityHandler(service *identity.Service, sessions *session.Manager, log *zap.Logger) (string, http.Handler) {
	h := &IdentityHandler{Service: service, Sessions: sessions, Log: log}
	return platformv1connect.NewIdentityServiceHandler(h, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))
}

func ValidationInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if message, ok := req.Any().(proto.Message); ok && hasUnknown(message.ProtoReflect()) {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("请求包含不允许的字段"))
			}
			return next(ctx, req)
		}
	}
}
func hasUnknown(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() {
			if field.MapValue().Message() != nil {
				value.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					unknown = hasUnknown(v.Message())
					return !unknown
				})
			}
		} else if field.IsList() {
			if field.Message() != nil {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					if hasUnknown(list.Get(i).Message()) {
						unknown = true
						return false
					}
				}
			}
		} else if field.Message() != nil {
			unknown = hasUnknown(value.Message())
		}
		return !unknown
	})
	return unknown
}

func userMessage(value identity.UserView) *platformv1.User {
	return &platformv1.User{Id: value.ID, Username: value.Username, Status: value.Status, InstanceAdmin: value.InstanceAdmin, AuthVersion: value.AuthVersion}
}
func timestamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func optionalTimestamp(value *time.Time) string {
	if value == nil {
		return ""
	}
	return timestamp(*value)
}
func tokenMessage(value identity.TokenView) *platformv1.AccessToken {
	return &platformv1.AccessToken{Id: value.ID, Name: value.Name, Scopes: value.Scopes, ApplicationIds: value.ApplicationIDs, ExpiresAt: timestamp(value.ExpiresAt), CreatedAt: timestamp(value.CreatedAt), RevokedAt: optionalTimestamp(value.RevokedAt), LastUsedAt: optionalTimestamp(value.LastUsedAt)}
}
func invitationMessage(value identity.InvitationView) *platformv1.Invitation {
	return &platformv1.Invitation{Id: value.ID, WorkspaceId: value.WorkspaceID, Username: value.Username, Role: value.Role, ExpiresAt: timestamp(value.ExpiresAt), ConsumedAt: optionalTimestamp(value.ConsumedAt), RevokedAt: optionalTimestamp(value.RevokedAt)}
}

func RPCError(log *zap.Logger, err error) error {
	if err == nil {
		return nil
	}
	var pgError *pgconn.PgError
	switch {
	case errors.Is(err, authorization.ErrUnauthenticated), errors.Is(err, identity.ErrCredentials):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("账号、密码或登录凭证无效"))
	case errors.Is(err, authorization.ErrDenied):
		return connect.NewError(connect.CodePermissionDenied, authorization.ErrDenied)
	case errors.Is(err, authorization.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, authorization.ErrNotFound)
	case errors.Is(err, authorization.ErrConflict):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, authorization.ErrInvalid), errors.Is(err, security.ErrPassword):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, session.ErrRateLimited):
		return connect.NewError(connect.CodeResourceExhausted, session.ErrRateLimited)
	case errors.Is(err, session.ErrUnavailable):
		return connect.NewError(connect.CodeUnavailable, session.ErrUnavailable)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	case errors.As(err, &pgError):
		// PostgreSQL detail可能带用户输入或敏感值，不返回或记录其原始文本。
		if pgError.Code == "23505" {
			return connect.NewError(connect.CodeAlreadyExists, errors.New("名称或记录已存在"))
		}
		if pgError.Code == "23503" || pgError.Code == "23514" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("输入不满足资源归属或格式要求"))
		}
		if log != nil {
			log.Error("数据库操作失败", zap.String("sqlstate", pgError.Code))
		}
	default:
		if log != nil {
			log.Error("产品接口内部操作失败", zap.String("type", "internal"))
		}
	}
	return connect.NewError(connect.CodeInternal, errors.New("控制面内部错误，请提供请求编号联系管理员"))
}

func (h *IdentityHandler) GetSession(ctx context.Context, _ *connect.Request[platformv1.GetSessionRequest]) (*connect.Response[platformv1.GetSessionResponse], error) {
	response := &platformv1.GetSessionResponse{CsrfToken: session.CSRFToken(ctx)}
	p, err := authorization.FromContext(ctx)
	if errors.Is(err, authorization.ErrUnauthenticated) {
		return connect.NewResponse(response), nil
	}
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	// 自由建空间能力位：实例管理员恒真；非实例管理员取决于实例开关（前端据此
	// 展示入口，授权仍由 CreateWorkspace 服务端判定）。未认证用户不返回该位。
	response.WorkspaceSelfCreate = p.InstanceAdmin || h.Service.WorkspaceSelfCreateEnabled(ctx)
	user, err := h.Service.Current(ctx, p)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response.User = userMessage(user)
	return connect.NewResponse(response), nil
}
func (h *IdentityHandler) Login(ctx context.Context, req *connect.Request[platformv1.LoginRequest]) (*connect.Response[platformv1.LoginResponse], error) {
	if err := h.Sessions.AllowLogin(ctx, req.Msg.Username); err != nil {
		return nil, RPCError(h.Log, err)
	}
	user, p, err := h.Service.Authenticate(ctx, req.Msg.Username, req.Msg.Password)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Sessions.Establish(ctx, p); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.LoginResponse{User: userMessage(user)}), nil
}
func (h *IdentityHandler) Logout(ctx context.Context, _ *connect.Request[platformv1.LogoutRequest]) (*connect.Response[platformv1.LogoutResponse], error) {
	if err := h.Sessions.Logout(ctx); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.LogoutResponse{}), nil
}
func (h *IdentityHandler) ChangePassword(ctx context.Context, req *connect.Request[platformv1.ChangePasswordRequest]) (*connect.Response[platformv1.ChangePasswordResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.ChangePassword(ctx, p, req.Msg.OldPassword, req.Msg.NewPassword); err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Sessions.Logout(ctx); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.ChangePasswordResponse{}), nil
}
func (h *IdentityHandler) AcceptInvitation(ctx context.Context, req *connect.Request[platformv1.AcceptInvitationRequest]) (*connect.Response[platformv1.AcceptInvitationResponse], error) {
	if err := h.Sessions.AllowLogin(ctx, "invitation:"+security.Digest(req.Msg.Token)); err != nil {
		return nil, RPCError(h.Log, err)
	}
	user, err := h.Service.AcceptInvitation(ctx, req.Msg.Token, req.Msg.Password)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.AcceptInvitationResponse{User: userMessage(user)}), nil
}
func (h *IdentityHandler) CompletePasswordReset(ctx context.Context, req *connect.Request[platformv1.CompletePasswordResetRequest]) (*connect.Response[platformv1.CompletePasswordResetResponse], error) {
	if err := h.Sessions.AllowLogin(ctx, "password-reset:"+security.Digest(req.Msg.Token)); err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.CompletePasswordReset(ctx, req.Msg.Token, req.Msg.NewPassword); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CompletePasswordResetResponse{}), nil
}
func (h *IdentityHandler) ListAccessTokens(ctx context.Context, _ *connect.Request[platformv1.ListAccessTokensRequest]) (*connect.Response[platformv1.ListAccessTokensResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	values, err := h.Service.ListTokens(ctx, p)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListAccessTokensResponse{}
	for _, value := range values {
		response.Tokens = append(response.Tokens, tokenMessage(value))
	}
	return connect.NewResponse(response), nil
}
func (h *IdentityHandler) CreateAccessToken(ctx context.Context, req *connect.Request[platformv1.CreateAccessTokenRequest]) (*connect.Response[platformv1.CreateAccessTokenResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	var expires time.Time
	if req.Msg.ExpiresAt != "" {
		expires, err = time.Parse(time.RFC3339, req.Msg.ExpiresAt)
		if err != nil {
			return nil, RPCError(h.Log, authorization.ErrInvalid)
		}
	}
	value, err := h.Service.CreateToken(ctx, p, identity.CreateTokenInput{Name: req.Msg.Name, Scopes: req.Msg.Scopes, ApplicationIDs: req.Msg.ApplicationIds, ExpiresAt: expires})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateAccessTokenResponse{Token: tokenMessage(value.Token), Plaintext: value.Plaintext}), nil
}
func (h *IdentityHandler) RevokeAccessToken(ctx context.Context, req *connect.Request[platformv1.RevokeAccessTokenRequest]) (*connect.Response[platformv1.RevokeAccessTokenResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.RevokeToken(ctx, p, req.Msg.TokenId); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.RevokeAccessTokenResponse{}), nil
}
func (h *IdentityHandler) ListUsers(ctx context.Context, _ *connect.Request[platformv1.ListUsersRequest]) (*connect.Response[platformv1.ListUsersResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	values, err := h.Service.ListUsers(ctx, p)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListUsersResponse{}
	for _, value := range values {
		response.Users = append(response.Users, userMessage(value))
	}
	return connect.NewResponse(response), nil
}
func (h *IdentityHandler) SetUserStatus(ctx context.Context, req *connect.Request[platformv1.SetUserStatusRequest]) (*connect.Response[platformv1.SetUserStatusResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.SetUserStatus(ctx, p, req.Msg.UserId, req.Msg.Status); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.SetUserStatusResponse{}), nil
}
func (h *IdentityHandler) CreatePasswordReset(ctx context.Context, req *connect.Request[platformv1.CreatePasswordResetRequest]) (*connect.Response[platformv1.CreatePasswordResetResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	plain, err := h.Service.CreatePasswordReset(ctx, p, req.Msg.UserId)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreatePasswordResetResponse{Token: plain}), nil
}
func (h *IdentityHandler) CreateInvitation(ctx context.Context, req *connect.Request[platformv1.CreateInvitationRequest]) (*connect.Response[platformv1.CreateInvitationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	value, err := h.Service.CreateInvitation(ctx, p, req.Msg.WorkspaceId, req.Msg.Username, req.Msg.Role)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateInvitationResponse{Invitation: invitationMessage(value.Invitation), Token: value.Plaintext}), nil
}
func (h *IdentityHandler) ListInvitations(ctx context.Context, req *connect.Request[platformv1.ListInvitationsRequest]) (*connect.Response[platformv1.ListInvitationsResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	values, err := h.Service.ListInvitations(ctx, p, req.Msg.WorkspaceId)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListInvitationsResponse{}
	for _, value := range values {
		response.Invitations = append(response.Invitations, invitationMessage(value))
	}
	return connect.NewResponse(response), nil
}
func (h *IdentityHandler) RevokeInvitation(ctx context.Context, req *connect.Request[platformv1.RevokeInvitationRequest]) (*connect.Response[platformv1.RevokeInvitationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.RevokeInvitation(ctx, p, req.Msg.WorkspaceId, req.Msg.InvitationId); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.RevokeInvitationResponse{}), nil
}
