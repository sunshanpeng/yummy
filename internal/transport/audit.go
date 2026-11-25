package transport

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"go.uber.org/zap"
)

type AuditHandler struct {
	service *audit.Service
	log     *zap.Logger
}

func NewAuditHandler(service *audit.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewAuditServiceHandler(&AuditHandler{service: service, log: log},
		append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(256<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}

func (h *AuditHandler) ListAuditEvents(ctx context.Context, req *connect.Request[platformv1.ListAuditEventsRequest]) (*connect.Response[platformv1.ListAuditEventsResponse], error) {
	principal, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, RPCError(h.log, err)
	}
	page, err := h.service.List(ctx, principal, req.Msg.WorkspaceId, int(req.Msg.Limit), req.Msg.Cursor)
	if err != nil {
		return nil, RPCError(h.log, err)
	}
	response := &platformv1.ListAuditEventsResponse{NextCursor: page.NextCursor}
	for _, event := range page.Events {
		item := &platformv1.AuditEvent{
			Id: event.ID, ActorId: event.ActorID, Action: event.Action, ObjectType: event.ObjectType,
			ObjectId: event.ObjectID, Result: event.Result, TraceId: event.TraceID, CreatedAt: timestamp(event.CreatedAt),
		}
		if event.WorkspaceID != nil {
			item.WorkspaceId = *event.WorkspaceID
		}
		if event.ApplicationID != nil {
			item.ApplicationId = *event.ApplicationID
		}
		response.Events = append(response.Events, item)
	}
	return connect.NewResponse(response), nil
}
