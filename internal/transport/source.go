package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/source"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
	"go.uber.org/zap"
)

type SourceHandler struct {
	Service *source.Service
	Log     *zap.Logger
}

func NewSourceHandler(service *source.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewSourceServiceHandler(&SourceHandler{service, log}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(128<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}
func sourceUploadMessage(v source.Upload) *platformv1.SourceUpload {
	return &platformv1.SourceUpload{Id: v.ID, WorkspaceId: v.WorkspaceID, ApplicationId: v.ApplicationID, State: v.State, ExpectedSize: v.ExpectedSize, ExpectedSha256: v.ExpectedSHA256, Dockerfile: v.Dockerfile, ExpiresAt: timestamp(v.ExpiresAt), SnapshotId: v.SnapshotID}
}
func sourceSnapshotMessage(v source.Snapshot) *platformv1.SourceSnapshot {
	result := &platformv1.SourceSnapshot{Id: v.ID, WorkspaceId: v.WorkspaceID, ApplicationId: v.ApplicationID, State: v.State, Sha256: v.SHA256, CompressedBytes: v.CompressedBytes, ExpandedBytes: v.ExpandedBytes, FileBytes: v.FileBytes, EntryCount: v.EntryCount, Dockerfile: v.Dockerfile, CreatedAt: timestamp(v.CreatedAt), ExpiresAt: timestamp(v.ExpiresAt)}
	for _, entry := range v.Entries {
		result.Entries = append(result.Entries, &platformv1.SourceEntry{Path: entry.Path, Size: entry.Size, Mode: uint32(entry.Mode.Perm()), Directory: entry.Directory})
	}
	return result
}
func SourceError(log *zap.Logger, err error) error {
	switch {
	case errors.Is(err, source.ErrExpired):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, source.ErrNotReady):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, source.ErrContent), errors.Is(err, sourcearchive.ErrInvalidPath), errors.Is(err, sourcearchive.ErrUnsupported), errors.Is(err, sourcearchive.ErrSensitive), errors.Is(err, sourcearchive.ErrDuplicate), errors.Is(err, sourcearchive.ErrDockerfile), errors.Is(err, sourcearchive.ErrDigestMismatch), errors.Is(err, sourcearchive.ErrInvalidArchive):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, sourcearchive.ErrLimit):
		return connect.NewError(connect.CodeResourceExhausted, sourcearchive.ErrLimit)
	case errors.Is(err, source.ErrUnavailable):
		return connect.NewError(connect.CodeUnavailable, source.ErrUnavailable)
	default:
		return RPCError(log, err)
	}
}
func (h *SourceHandler) CreateSourceUpload(ctx context.Context, req *connect.Request[platformv1.CreateSourceUploadRequest]) (*connect.Response[platformv1.CreateSourceUploadResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, SourceError(h.Log, err)
	}
	v, err := h.Service.CreateUpload(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, source.CreateInput{Size: req.Msg.Size, SHA256: req.Msg.Sha256, Dockerfile: req.Msg.Dockerfile})
	if err != nil {
		return nil, SourceError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CreateSourceUploadResponse{Upload: sourceUploadMessage(v), TransferPath: "/transfer/w/" + v.WorkspaceID + "/apps/" + v.ApplicationID + "/uploads/" + v.ID}), nil
}
func (h *SourceHandler) CompleteSourceUpload(ctx context.Context, req *connect.Request[platformv1.CompleteSourceUploadRequest]) (*connect.Response[platformv1.CompleteSourceUploadResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, SourceError(h.Log, err)
	}
	v, err := h.Service.Complete(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.UploadId)
	if err != nil {
		return nil, SourceError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.CompleteSourceUploadResponse{Snapshot: sourceSnapshotMessage(v)}), nil
}
func (h *SourceHandler) GetSourceSnapshot(ctx context.Context, req *connect.Request[platformv1.GetSourceSnapshotRequest]) (*connect.Response[platformv1.GetSourceSnapshotResponse], error) {
	p, err := authorization.FromContext(ctx)
	if err != nil {
		return nil, SourceError(h.Log, err)
	}
	v, err := h.Service.GetSnapshot(ctx, p, req.Msg.WorkspaceId, req.Msg.ApplicationId, req.Msg.SnapshotId)
	if err != nil {
		return nil, SourceError(h.Log, err)
	}
	return connect.NewResponse(&platformv1.GetSourceSnapshotResponse{Snapshot: sourceSnapshotMessage(v), DownloadPath: "/transfer/w/" + v.WorkspaceID + "/apps/" + v.ApplicationID + "/snapshots/" + v.ID}), nil
}

// TransferHandler 必须挂在没有30秒整体读写超时的传输入口；每次传输设置10分钟绝对截止。
// Cookie/CSRF/纯Bearer与普通API使用相同会话适配，不能绕开对象归属。
func TransferHandler(service *source.Service, log *zap.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /transfer/w/{workspace}/apps/{application}/uploads/{upload}", func(w http.ResponseWriter, r *http.Request) {
		p, err := authorization.FromContext(r.Context())
		if err != nil {
			writeTransferError(w, log, err)
			return
		}
		deadline := time.Now().Add(10 * time.Minute)
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil {
			writeTransferError(w, log, source.ErrUnavailable)
			return
		}
		if err := controller.SetWriteDeadline(deadline); err != nil {
			writeTransferError(w, log, source.ErrUnavailable)
			return
		}
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, sourcearchive.DefaultLimits().MaxCompressedBytes)
		_, err = service.Upload(r.Context(), p, r.PathValue("workspace"), r.PathValue("application"), r.PathValue("upload"), r.Body)
		if err != nil {
			writeTransferError(w, log, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /transfer/w/{workspace}/apps/{application}/snapshots/{snapshot}", func(w http.ResponseWriter, r *http.Request) {
		p, err := authorization.FromContext(r.Context())
		if err != nil {
			writeTransferError(w, log, err)
			return
		}
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Minute)); err != nil {
			writeTransferError(w, log, source.ErrUnavailable)
			return
		}
		reader, snapshot, err := service.OpenSnapshot(r.Context(), p, r.PathValue("workspace"), r.PathValue("application"), r.PathValue("snapshot"))
		if err != nil {
			writeTransferError(w, log, err)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="source.tar.gz"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Source-SHA256", snapshot.SHA256)
		_, _ = io.Copy(w, reader)
	})
	return mux
}
func writeTransferError(w http.ResponseWriter, log *zap.Logger, err error) {
	rpc := SourceError(log, err)
	code := connect.CodeOf(rpc)
	status := http.StatusInternalServerError
	switch code {
	case connect.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case connect.CodePermissionDenied:
		status = http.StatusForbidden
	case connect.CodeNotFound:
		status = http.StatusNotFound
	case connect.CodeInvalidArgument:
		status = http.StatusBadRequest
	case connect.CodeResourceExhausted:
		status = http.StatusRequestEntityTooLarge
	case connect.CodeAborted:
		status = http.StatusConflict
	case connect.CodeFailedPrecondition:
		status = http.StatusConflict
	case connect.CodeUnavailable:
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, rpc.Error(), status)
}
