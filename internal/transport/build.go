package transport

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
	"go.uber.org/zap"
)

type BuildHandler struct {
	Service *build.Service
	Log     *zap.Logger
}

func NewBuildHandler(service *build.Service, log *zap.Logger, opts ...connect.HandlerOption) (string, http.Handler) {
	return platformv1connect.NewBuildServiceHandler(&BuildHandler{service, log}, append(opts, connect.WithCodec(StrictJSONCodec{}), connect.WithReadMaxBytes(512<<10), connect.WithInterceptors(ValidationInterceptor()))...)
}
func buildError(log *zap.Logger, err error) error {
	if errors.Is(err, build.ErrSourceExpired) || errors.Is(err, build.ErrSourceInvalid) || errors.Is(err, inventory.ErrNotReady) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return RPCError(log, err)
}
func buildConfigMessage(v build.Config) *platformv1.BuildConfig {
	result := &platformv1.BuildConfig{Dockerfile: v.Dockerfile, Context: v.Context, Target: v.Target, Args: v.Args}
	for _, ref := range v.SecretRefs {
		result.SecretRefs = append(result.SecretRefs, &platformv1.BuildSecretRef{SecretId: ref.SecretID, Name: ref.Name, Version: ref.Version})
	}
	return result
}
func buildConfigInput(v *platformv1.BuildConfig) build.Config {
	if v == nil {
		return build.Config{}
	}
	result := build.Config{Dockerfile: v.Dockerfile, Context: v.Context, Target: v.Target, Args: v.Args}
	for _, ref := range v.SecretRefs {
		result.SecretRefs = append(result.SecretRefs, build.SecretRef{SecretID: ref.SecretId, Name: ref.Name, Version: ref.Version})
	}
	return result
}
func buildInput(v *platformv1.BuildInput) (build.CreateInput, error) {
	if v == nil {
		return build.CreateInput{}, authorization.ErrInvalid
	}
	result := build.CreateInput{SourceSnapshotID: v.SourceSnapshotId, ConfigRevision: v.ConfigRevision, ExpectedPolicyRevision: v.ExpectedPolicyRevision, IdempotencyKey: v.IdempotencyKey, AutoDeploy: v.AutoDeploy, RuntimeConfigRevision: v.RuntimeConfigRevision, ExpectedServingRelease: v.ExpectedServingRelease}
	if o := v.Overrides; o != nil {
		result.Overrides = build.Overrides{Dockerfile: o.Dockerfile, Context: o.Context, Target: o.Target, Args: o.Args}
		if o.SecretRefs != nil {
			result.Overrides.SecretRefs = make([]build.SecretRef, 0, len(o.SecretRefs))
		}
		for _, ref := range o.SecretRefs {
			result.Overrides.SecretRefs = append(result.Overrides.SecretRefs, build.SecretRef{SecretID: ref.SecretId, Name: ref.Name, Version: ref.Version})
		}
	}
	return result, nil
}
func buildMessage(v build.View) *platformv1.Build {
	result := &platformv1.Build{Id: v.ID, WorkspaceId: v.WorkspaceID, ApplicationId: v.ApplicationID, ActorId: v.ActorID, SourceSnapshotId: v.SourceSnapshotID, ConfigRevision: v.ConfigRevision, Config: buildConfigMessage(v.Config), PolicyRevision: v.PolicyRevision, NodeId: v.NodeID, Repository: v.Repository, Platform: v.Platform, State: v.State, Stage: v.Stage, DesiredState: v.DesiredState, ErrorCode: v.ErrorCode, QueueReason: v.QueueReason, Version: v.Version, AutoDeploy: v.AutoDeploy, RuntimeConfigRevision: v.RuntimeConfigRevision, LogsComplete: v.LogsComplete, LogsTruncated: v.LogsTruncated, CreatedAt: timestamp(v.CreatedAt), FinishedAt: optionalTimestamp(v.FinishedAt)}
	if v.RetryOf != nil {
		result.RetryOf = *v.RetryOf
	}
	if a := v.Artifact; a != nil {
		result.Artifact = &platformv1.Artifact{Id: a.ID, RegistryId: a.RegistryID, CredentialVersion: a.CredentialVersion, Repository: a.Repository, Tag: a.Tag, Digest: a.Digest, Platform: a.Platform, State: a.State, VerifiedAt: optionalTimestamp(a.VerifiedAt)}
	}
	return result
}
func (h *BuildHandler) GetBuildConfig(ctx context.Context, r *connect.Request[platformv1.GetBuildConfigRequest]) (*connect.Response[platformv1.GetBuildConfigResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.GetConfig(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetBuildConfigResponse{Config: buildConfigMessage(v.Config), Revision: v.Revision}), nil
}
func (h *BuildHandler) SaveBuildConfig(ctx context.Context, r *connect.Request[platformv1.SaveBuildConfigRequest]) (*connect.Response[platformv1.SaveBuildConfigResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	if r.Msg.Config == nil {
		return nil, buildError(h.Log, authorization.ErrInvalid)
	}
	c := buildConfigInput(r.Msg.Config)
	revision, e := h.Service.SaveConfig(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.ExpectedRevision, c)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.GetConfigRevision(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, revision)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.SaveBuildConfigResponse{Config: buildConfigMessage(v.Config), Revision: revision}), nil
}
func (h *BuildHandler) PreviewBuild(ctx context.Context, r *connect.Request[platformv1.PreviewBuildRequest]) (*connect.Response[platformv1.PreviewBuildResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	input, e := buildInput(r.Msg.Input)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.CreatePreview(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, input)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.PreviewBuildResponse{Preview: &platformv1.BuildPreview{SourceSnapshotId: v.SourceSnapshotID, SourceSha256: v.SourceSHA256, ConfigRevision: v.ConfigRevision, Config: buildConfigMessage(v.Config), Execution: summaryMessage(v.Execution), AutoDeploy: v.AutoDeploy, RuntimeConfigRevision: v.RuntimeConfigRevision}}), nil
}
func (h *BuildHandler) CreateBuild(ctx context.Context, r *connect.Request[platformv1.CreateBuildRequest]) (*connect.Response[platformv1.CreateBuildResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	i, e := buildInput(r.Msg.Input)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.CreateBuild(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, i)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.CreateBuildResponse{Build: buildMessage(v)}), nil
}
func (h *BuildHandler) GetBuild(ctx context.Context, r *connect.Request[platformv1.GetBuildRequest]) (*connect.Response[platformv1.GetBuildResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.Get(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.BuildId)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.GetBuildResponse{Build: buildMessage(v)}), nil
}
func (h *BuildHandler) ListBuilds(ctx context.Context, r *connect.Request[platformv1.ListBuildsRequest]) (*connect.Response[platformv1.ListBuildsResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.List(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, int(r.Msg.Limit), r.Msg.Cursor)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	out := &platformv1.ListBuildsResponse{NextCursor: v.NextCursor}
	for _, b := range v.Items {
		out.Builds = append(out.Builds, buildMessage(b))
	}
	return connect.NewResponse(out), nil
}
func (h *BuildHandler) ReadBuildLogs(ctx context.Context, r *connect.Request[platformv1.ReadBuildLogsRequest]) (*connect.Response[platformv1.ReadBuildLogsResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.ReadLogs(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.BuildId, r.Msg.AfterSeq, int(r.Msg.Limit))
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	out := &platformv1.ReadBuildLogsResponse{NextSeq: v.NextSeq, Complete: v.Complete, Truncated: v.Truncated}
	for _, c := range v.Items {
		out.Chunks = append(out.Chunks, &platformv1.BuildLogChunk{Seq: c.Seq, Content: c.Content, Truncated: c.Truncated, CreatedAt: timestamp(c.CreatedAt)})
	}
	return connect.NewResponse(out), nil
}
func (h *BuildHandler) CancelBuild(ctx context.Context, r *connect.Request[platformv1.CancelBuildRequest]) (*connect.Response[platformv1.CancelBuildResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.Cancel(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.BuildId)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.CancelBuildResponse{Build: buildMessage(v)}), nil
}
func (h *BuildHandler) RetryBuild(ctx context.Context, r *connect.Request[platformv1.RetryBuildRequest]) (*connect.Response[platformv1.RetryBuildResponse], error) {
	p, e := authorization.FromContext(ctx)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	i, e := buildInput(r.Msg.Input)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	v, e := h.Service.Retry(ctx, p, r.Msg.WorkspaceId, r.Msg.ApplicationId, r.Msg.BuildId, i)
	if e != nil {
		return nil, buildError(h.Log, e)
	}
	return connect.NewResponse(&platformv1.RetryBuildResponse{Build: buildMessage(v)}), nil
}
