package transport

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/workspace"
	"go.uber.org/zap"
)

type WorkspaceHandler struct {
	Service *workspace.Service
	Log     *zap.Logger
}
type ApplicationHandler struct {
	Service *application.Service
	Log     *zap.Logger
}

func NewWorkspaceHandler(service *workspace.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewWorkspaceServiceHandler(&WorkspaceHandler{service, log}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}
func NewApplicationHandler(service *application.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewApplicationServiceHandler(&ApplicationHandler{service, log}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}
func workspaceMessage(v workspace.View) *platformv1.Workspace {
	return &platformv1.Workspace{Id: v.ID, Name: v.Name, Slug: v.Slug, Description: v.Description, Status: v.Status, Revision: v.Revision, Role: v.Role, Capabilities: v.Capabilities, CreatedAt: timestamp(v.CreatedAt), UpdatedAt: timestamp(v.UpdatedAt)}
}
func applicationMessage(v application.View) *platformv1.Application {
	return &platformv1.Application{Id: v.ID, WorkspaceId: v.WorkspaceID, Name: v.Name, Description: v.Description, Status: v.Status, Revision: v.Revision, Capabilities: v.Capabilities, CreatedAt: timestamp(v.CreatedAt), UpdatedAt: timestamp(v.UpdatedAt), DeletionReady: v.DeletionReady, DeletionState: v.DeletionState, DeletionError: v.DeletionError, DeletingVolumes: v.DeletingVolumes}
}
func (h *WorkspaceHandler) ListWorkspaces(ctx context.Context, req *connect.Request[platformv1.ListWorkspacesRequest]) (*connect.Response[platformv1.ListWorkspacesResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	values, err := h.Service.List(ctx, p, int(req.Msg.Limit), req.Msg.Cursor)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListWorkspacesResponse{NextCursor: values.NextCursor}
	for _, v := range values.Items {
		response.Workspaces = append(response.Workspaces, workspaceMessage(v))
	}
	return connect.NewResponse(response), nil
}
func (h *WorkspaceHandler) GetWorkspace(ctx context.Context, req *connect.Request[platformv1.GetWorkspaceRequest]) (*connect.Response[platformv1.GetWorkspaceResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Get(ctx, p, req.Msg.WorkspaceId, req.Msg.Slug)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.GetWorkspaceResponse{Workspace: workspaceMessage(v)}), nil
}
func (h *WorkspaceHandler) CreateWorkspace(ctx context.Context, req *connect.Request[platformv1.CreateWorkspaceRequest]) (*connect.Response[platformv1.CreateWorkspaceResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Create(ctx, p, workspace.CreateInput{Name: req.Msg.Name, Slug: req.Msg.Slug, Description: req.Msg.Description})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateWorkspaceResponse{Workspace: workspaceMessage(v)}), nil
}
func (h *WorkspaceHandler) UpdateWorkspace(ctx context.Context, req *connect.Request[platformv1.UpdateWorkspaceRequest]) (*connect.Response[platformv1.UpdateWorkspaceResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Update(ctx, p, req.Msg.WorkspaceId, workspace.UpdateInput{Name: req.Msg.Name, Slug: req.Msg.Slug, Description: req.Msg.Description, ExpectedRevision: req.Msg.ExpectedRevision})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.UpdateWorkspaceResponse{Workspace: workspaceMessage(v)}), nil
}
func (h *WorkspaceHandler) ArchiveWorkspace(ctx context.Context, req *connect.Request[platformv1.ArchiveWorkspaceRequest]) (*connect.Response[platformv1.ArchiveWorkspaceResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.Archive(ctx, p, req.Msg.WorkspaceId, req.Msg.ExpectedRevision); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.ArchiveWorkspaceResponse{}), nil
}
func (h *WorkspaceHandler) ListWorkspaceMembers(ctx context.Context, req *connect.Request[platformv1.ListWorkspaceMembersRequest]) (*connect.Response[platformv1.ListWorkspaceMembersResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	page, err := h.Service.ListMembers(ctx, p, req.Msg.WorkspaceId, int(req.Msg.Limit), req.Msg.Cursor)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListWorkspaceMembersResponse{NextCursor: page.NextCursor}
	for _, v := range page.Items {
		member := &platformv1.WorkspaceMember{UserId: v.UserID, Username: v.Username, UserStatus: v.UserStatus, Role: v.Role, InheritsAllApplications: v.InheritsAllApplications, ApplicationsTruncated: v.ApplicationsTruncated}
		for _, grant := range v.Applications {
			member.Applications = append(member.Applications, &platformv1.ApplicationGrant{ApplicationId: grant.ApplicationID, ApplicationName: grant.ApplicationName, Role: grant.Role})
		}
		response.Members = append(response.Members, member)
	}
	return connect.NewResponse(response), nil
}
func (h *WorkspaceHandler) ChangeWorkspaceMemberRole(ctx context.Context, req *connect.Request[platformv1.ChangeWorkspaceMemberRoleRequest]) (*connect.Response[platformv1.ChangeWorkspaceMemberRoleResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.ChangeMemberRole(ctx, p, req.Msg.WorkspaceId, req.Msg.UserId, req.Msg.Role); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.ChangeWorkspaceMemberRoleResponse{}), nil
}
func (h *WorkspaceHandler) RemoveWorkspaceMember(ctx context.Context, req *connect.Request[platformv1.RemoveWorkspaceMemberRequest]) (*connect.Response[platformv1.RemoveWorkspaceMemberResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.RemoveMember(ctx, p, req.Msg.WorkspaceId, req.Msg.UserId); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.RemoveWorkspaceMemberResponse{}), nil
}
func (h *ApplicationHandler) ListApplications(ctx context.Context, req *connect.Request[platformv1.ListApplicationsRequest]) (*connect.Response[platformv1.ListApplicationsResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	list := h.Service.List
	if req.Msg.ArchivedOnly {
		list = h.Service.ListArchived
	}
	page, err := list(ctx, p, req.Msg.WorkspaceId, int(req.Msg.Limit), req.Msg.Cursor)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListApplicationsResponse{NextCursor: page.NextCursor}
	for _, v := range page.Items {
		response.Applications = append(response.Applications, applicationMessage(v))
	}
	return connect.NewResponse(response), nil
}
func (h *ApplicationHandler) GetApplication(ctx context.Context, req *connect.Request[platformv1.GetApplicationRequest]) (*connect.Response[platformv1.GetApplicationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Get(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.ApplicationName)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.GetApplicationResponse{Application: applicationMessage(v)}), nil
}
func (h *ApplicationHandler) CreateApplication(ctx context.Context, req *connect.Request[platformv1.CreateApplicationRequest]) (*connect.Response[platformv1.CreateApplicationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Create(ctx, p, req.Msg.WorkspaceId, application.CreateInput{Name: req.Msg.Name, IdempotencyKey: req.Msg.IdempotencyKey})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateApplicationResponse{Application: applicationMessage(v), ConfigurationPath: "/w/" + v.WorkspaceID + "/apps/" + v.ID + "/settings/runtime"}), nil
}
func (h *ApplicationHandler) UpdateApplication(ctx context.Context, req *connect.Request[platformv1.UpdateApplicationRequest]) (*connect.Response[platformv1.UpdateApplicationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	v, err := h.Service.Update(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, application.UpdateInput{Name: req.Msg.Name, Description: req.Msg.Description, ExpectedRevision: req.Msg.ExpectedRevision})
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.UpdateApplicationResponse{Application: applicationMessage(v)}), nil
}
func (h *ApplicationHandler) ArchiveApplication(ctx context.Context, req *connect.Request[platformv1.ArchiveApplicationRequest]) (*connect.Response[platformv1.ArchiveApplicationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.Archive(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.ExpectedRevision); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.ArchiveApplicationResponse{}), nil
}
func (h *ApplicationHandler) DeleteApplication(ctx context.Context, req *connect.Request[platformv1.DeleteApplicationRequest]) (*connect.Response[platformv1.DeleteApplicationResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	state, err := h.Service.Delete(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.ExpectedRevision)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.DeleteApplicationResponse{State: state}), nil
}
func (h *ApplicationHandler) DeleteApplicationVolumes(ctx context.Context, req *connect.Request[platformv1.DeleteApplicationVolumesRequest]) (*connect.Response[platformv1.DeleteApplicationVolumesResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	state, err := h.Service.DeleteVolumes(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.ExpectedRevision, req.Msg.Confirmation)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.DeleteApplicationVolumesResponse{State: state}), nil
}
func (h *ApplicationHandler) ListApplicationMembers(ctx context.Context, req *connect.Request[platformv1.ListApplicationMembersRequest]) (*connect.Response[platformv1.ListApplicationMembersResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	page, err := h.Service.ListMembers(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, int(req.Msg.Limit), req.Msg.Cursor)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	response := &platformv1.ListApplicationMembersResponse{NextCursor: page.NextCursor}
	for _, v := range page.Items {
		response.Members = append(response.Members, &platformv1.ApplicationMember{UserId: v.UserID, Username: v.Username, UserStatus: v.UserStatus, Role: v.Role, Inherited: v.Inherited})
	}
	return connect.NewResponse(response), nil
}
func (h *ApplicationHandler) SetApplicationMember(ctx context.Context, req *connect.Request[platformv1.SetApplicationMemberRequest]) (*connect.Response[platformv1.SetApplicationMemberResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.SetMember(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.UserId, req.Msg.Role); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.SetApplicationMemberResponse{}), nil
}
func (h *ApplicationHandler) RemoveApplicationMember(ctx context.Context, req *connect.Request[platformv1.RemoveApplicationMemberRequest]) (*connect.Response[platformv1.RemoveApplicationMemberResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.Log, err)
	}
	if err := h.Service.RemoveMember(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.UserId); err != nil {
		return nil, RPCError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.RemoveApplicationMemberResponse{}), nil
}
