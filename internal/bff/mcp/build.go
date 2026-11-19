package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
)

type buildInput struct {
	SourceSnapshotID       string          `json:"sourceSnapshotId"`
	ConfigRevision         int64           `json:"configRevision"`
	ExpectedPolicyRevision int64           `json:"expectedPolicyRevision"`
	Overrides              build.Overrides `json:"overrides"`
	IdempotencyKey         string          `json:"idempotencyKey"`
	AutoDeploy             bool            `json:"autoDeploy"`
	RuntimeConfigRevision  int64           `json:"runtimeConfigRevision"`
	ExpectedServingRelease string          `json:"expectedServingRelease" jsonschema:"首次发布时显式传空字符串"`
}

func (v buildInput) domain() build.CreateInput {
	return build.CreateInput{SourceSnapshotID: v.SourceSnapshotID, ConfigRevision: v.ConfigRevision, ExpectedPolicyRevision: v.ExpectedPolicyRevision, Overrides: v.Overrides, IdempotencyKey: v.IdempotencyKey, AutoDeploy: v.AutoDeploy, RuntimeConfigRevision: v.RuntimeConfigRevision, ExpectedServingRelease: v.ExpectedServingRelease}
}
func (v buildInput) valid() bool {
	return validID(v.SourceSnapshotID) && v.ConfigRevision >= 0 && v.ExpectedPolicyRevision > 0 && validKey(v.IdempotencyKey) && ((v.AutoDeploy && v.RuntimeConfigRevision > 0 && (v.ExpectedServingRelease == "" || validID(v.ExpectedServingRelease))) || (!v.AutoDeploy && v.RuntimeConfigRevision == 0 && v.ExpectedServingRelease == ""))
}

type retryInput struct {
	ID    string     `json:"id"`
	Input buildInput `json:"input"`
}
type logsInput struct {
	ID       string `json:"id"`
	AfterSeq int64  `json:"afterSeq"`
	Limit    int    `json:"limit,omitempty"`
}

type artifactView struct {
	ID         string     `json:"id"`
	Repository string     `json:"repository"`
	Digest     string     `json:"digest"`
	Platform   string     `json:"platform"`
	State      string     `json:"state"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
}
type buildView struct {
	ID                     string        `json:"id"`
	SourceSnapshotID       string        `json:"sourceSnapshotId"`
	Status                 string        `json:"status"`
	Stage                  string        `json:"stage"`
	DesiredState           string        `json:"desiredState"`
	ErrorCode              string        `json:"errorCode,omitempty"`
	QueueReason            string        `json:"queueReason,omitempty"`
	ConfigRevision         int64         `json:"configRevision"`
	PolicyRevision         int64         `json:"policyRevision"`
	RuntimeConfigRevision  *int64        `json:"runtimeConfigRevision,omitempty"`
	ExpectedServingRelease string        `json:"expectedServingRelease"`
	RetryOf                *string       `json:"retryOf,omitempty"`
	LogsComplete           bool          `json:"logsComplete"`
	LogsTruncated          bool          `json:"logsTruncated"`
	CreatedAt              time.Time     `json:"createdAt"`
	FinishedAt             *time.Time    `json:"finishedAt,omitempty"`
	Artifact               *artifactView `json:"artifact,omitempty"`
}

func safeArtifact(v *build.Artifact) *artifactView {
	if v == nil {
		return nil
	}
	return &artifactView{v.ID, v.Repository, v.Digest, v.Platform, v.State, v.VerifiedAt}
}
func safeBuild(v build.View) buildView {
	return buildView{ID: v.ID, SourceSnapshotID: v.SourceSnapshotID, Status: v.State, Stage: v.Stage, DesiredState: v.DesiredState, ErrorCode: v.ErrorCode, QueueReason: v.QueueReason, ConfigRevision: v.ConfigRevision, PolicyRevision: v.PolicyRevision, RuntimeConfigRevision: v.RuntimeConfigRevision, ExpectedServingRelease: v.ExpectedServingRelease, RetryOf: v.RetryOf, LogsComplete: v.LogsComplete, LogsTruncated: v.LogsTruncated, CreatedAt: v.CreatedAt, FinishedAt: v.FinishedAt, Artifact: safeArtifact(v.Artifact)}
}

type previewView struct {
	SourceSnapshotID      string       `json:"sourceSnapshotId"`
	SourceSHA256          string       `json:"sourceSha256"`
	ConfigRevision        int64        `json:"configRevision"`
	Config                build.Config `json:"config"`
	PolicyRevision        int64        `json:"policyRevision"`
	Platform              string       `json:"platform"`
	ImageRepository       string       `json:"imageRepository"`
	Ready                 bool         `json:"ready"`
	ReadyReasons          []string     `json:"readyReasons"`
	AutoDeploy            bool         `json:"autoDeploy"`
	RuntimeConfigRevision *int64       `json:"runtimeConfigRevision,omitempty"`
}

func inputScopes(in buildInput) []authorization.Action {
	actions := []authorization.Action{authorization.BuildWrite, authorization.SourceRead}
	if in.AutoDeploy {
		actions = append(actions, authorization.DeploymentWrite, authorization.ConfigRead)
	}
	return actions
}

func registerBuildTools(server *sdk.Server, s scope) {
	typedTool(server, "build_config_get", "读取当前应用的构建配置及版本。", annotations(true, false, true, false), func(ctx context.Context, _ emptyInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.ConfigRead); err != nil {
			return nil, err
		}
		v, err := s.services.Builds.GetConfig(ctx, s.principal, s.workspaceID, s.applicationID)
		if err != nil {
			return nil, failure(err)
		}
		return v, nil
	})
	typedTool(server, "build_preview", "检查已上传快照、配置和平台策略，不创建构建。", annotations(true, false, true, false), func(ctx context.Context, in buildInput) (any, *sdk.CallToolResult) {
		if err := s.check(inputScopes(in)...); err != nil {
			return nil, err
		}
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "构建参数、版本或幂等键无效")
		}
		v, err := s.services.Builds.CreatePreview(ctx, s.principal, s.workspaceID, s.applicationID, in.domain())
		if err != nil {
			return nil, failure(err)
		}
		reasons := []string{}
		if v.Execution.Reason != "" {
			reasons = append(reasons, v.Execution.Reason)
		}
		return previewView{v.SourceSnapshotID, v.SourceSHA256, v.ConfigRevision, v.Config, v.Execution.PolicyRevision, v.Execution.Platform, v.Execution.ImageRepository, v.Execution.Ready, reasons, v.AutoDeploy, v.RuntimeConfigRevision}, nil
	})
	typedTool(server, "build_create", "创建应用构建，受理后继续在后台运行；相同幂等键只能提交相同请求。", annotations(false, false, true, true), func(ctx context.Context, in buildInput) (any, *sdk.CallToolResult) {
		if err := s.check(inputScopes(in)...); err != nil {
			return nil, err
		}
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "构建参数、版本或幂等键无效")
		}
		v, err := s.services.Builds.CreateBuild(ctx, s.principal, s.workspaceID, s.applicationID, in.domain())
		if err != nil {
			return nil, failure(err)
		}
		return safeBuild(v), nil
	})
	typedTool(server, "build_get", "读取当前应用的一次构建状态。", annotations(true, false, true, false), func(ctx context.Context, in idInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.BuildRead); err != nil {
			return nil, err
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "构建 ID 无效")
		}
		v, err := s.services.Builds.Get(ctx, s.principal, s.workspaceID, s.applicationID, in.ID)
		if err != nil {
			return nil, failure(err)
		}
		return safeBuild(v), nil
	})
	typedTool(server, "build_list", "分页列出当前应用的构建。", annotations(true, false, true, false), func(ctx context.Context, in pageInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.BuildRead); err != nil {
			return nil, err
		}
		if !validPage(in) {
			return nil, toolError("INVALID_ARGUMENT", "分页参数无效")
		}
		v, err := s.services.Builds.List(ctx, s.principal, s.workspaceID, s.applicationID, in.Limit, in.Cursor)
		if err != nil {
			return nil, failure(err)
		}
		items := make([]buildView, 0, len(v.Items))
		for _, item := range v.Items {
			items = append(items, safeBuild(item))
		}
		return struct {
			Items      []buildView `json:"items"`
			NextCursor string      `json:"nextCursor,omitempty"`
		}{items, v.NextCursor}, nil
	})
	typedTool(server, "build_logs", "分页读取原始构建日志，afterSeq 从零开始；日志可能含用户输出，请勿把秘密写入日志。", annotations(true, false, true, false), func(ctx context.Context, in logsInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.LogsRead); err != nil {
			return nil, err
		}
		if !validID(in.ID) || in.AfterSeq < 0 || in.Limit < 0 || in.Limit > 16 {
			return nil, toolError("INVALID_ARGUMENT", "日志参数无效，每次最多 16 块")
		}
		if in.Limit == 0 {
			in.Limit = 16
		}
		v, err := s.services.Builds.ReadLogs(ctx, s.principal, s.workspaceID, s.applicationID, in.ID, in.AfterSeq, in.Limit)
		if err != nil {
			return nil, failure(err)
		}
		return v, nil
	})
	typedTool(server, "build_cancel", "请求停止构建，停止需等待 worker 确认，不能提前宣告任务已停止。", annotations(false, true, true, true), func(ctx context.Context, in idInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.BuildWrite); err != nil {
			return nil, err
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "构建 ID 无效")
		}
		v, err := s.services.Builds.Cancel(ctx, s.principal, s.workspaceID, s.applicationID, in.ID)
		if err != nil {
			return nil, failure(err)
		}
		return safeBuild(v), nil
	})
	typedTool(server, "build_retry", "重试已结束构建；使用新的幂等键和完整当前快照、配置及策略版本。", annotations(false, false, true, true), func(ctx context.Context, in retryInput) (any, *sdk.CallToolResult) {
		if err := s.check(inputScopes(in.Input)...); err != nil {
			return nil, err
		}
		if !validID(in.ID) || !in.Input.valid() {
			return nil, toolError("INVALID_ARGUMENT", "构建 ID、版本或幂等键无效")
		}
		v, err := s.services.Builds.Retry(ctx, s.principal, s.workspaceID, s.applicationID, in.ID, in.Input.domain())
		if err != nil {
			return nil, failure(err)
		}
		return safeBuild(v), nil
	})
	typedTool(server, "build_artifact_get", "读取指定构建的镜像产物状态；未验证产物不等于可部署。", annotations(true, false, true, false), func(ctx context.Context, in idInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.BuildRead); err != nil {
			return nil, err
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "构建 ID 无效")
		}
		v, err := s.services.Builds.Get(ctx, s.principal, s.workspaceID, s.applicationID, in.ID)
		if err != nil {
			return nil, failure(err)
		}
		return struct {
			BuildID  string        `json:"buildId"`
			Artifact *artifactView `json:"artifact"`
		}{v.ID, safeArtifact(v.Artifact)}, nil
	})
}
