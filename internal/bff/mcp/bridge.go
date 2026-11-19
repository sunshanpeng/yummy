package mcp

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/transport/clientcli"
)

// bridgeAuthNote 是全部桥接工具描述的鉴权语义前缀：本地 stdio 进程只持有
// 用户自己的个人凭证（PAT），不做本地授权判断；实际权限范围由控制面服务端
// 按凭证授权强制。
const bridgeAuthNote = "以当前登录用户的 PAT 经控制面 API 执行，权限由服务端按凭证授权强制。"

// BridgeConfig 由 CLI 组装。Client 必须来自 clientcli.ResolveClient，凭证解析、
// 存储与刷新规则和 CLI 完全一致；令牌只在进程内使用，不打印、不落盘。
type BridgeConfig struct {
	Client *clientcli.APIClient
}

// NewBridgeServer 构造本地 stdio MCP 服务。local.AllowRoots 非空时提供与
// NewLocalServer 相同语义的只读 source_preview；bridge.Client 非空时额外注册
// 经控制面 API 的业务工具。两者都缺省时报错；未登录（Client 为 nil）时服务
// instructions 明确说明只有 source_preview 可用。
func NewBridgeServer(bridge BridgeConfig, local LocalConfig) (*sdk.Server, error) {
	var roots []string
	if len(local.AllowRoots) > 0 {
		var err error
		if roots, err = validateAllowRoots(local.AllowRoots); err != nil {
			return nil, err
		}
	}
	if bridge.Client == nil && roots == nil {
		return nil, errors.New("必须提供授权源码根目录或已登录的用户凭证")
	}
	opts := &sdk.ServerOptions{Instructions: "业务工具以用户个人凭证经控制面 API 执行，权限由服务端强制；目标应用可用 workspace_list/app_list 查询。"}
	if bridge.Client == nil {
		opts = &sdk.ServerOptions{Instructions: "当前未登录：只有本地源码只读预览 source_preview；业务工具需要先用 platform client auth login 或 auth token 配置用户凭证后重新启动。"}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "yummy-local", Version: "1.0.0"}, opts)
	if roots != nil {
		registerSourcePreview(server, roots)
	}
	if bridge.Client != nil {
		registerBridgeTools(server, bridge.Client)
	}
	return server, nil
}

// bridgeRPCError 把控制面 Connect 错误映射到既有 toolError 码表；语义与远程
// MCP 的 failure() 对齐，避免向模型暴露传输层细节。
func bridgeRPCError(err error) *sdk.CallToolResult {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return toolError("REQUEST_INTERRUPTED", "请求中断；若写入已经受理，请使用原幂等键查询或重试")
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument:
		return toolError("INVALID_ARGUMENT", "输入无效，请检查工具参数")
	case connect.CodeUnauthenticated:
		return toolError("UNAUTHENTICATED", "凭证无效或已过期，请重新登录后重启 MCP")
	case connect.CodePermissionDenied:
		return toolError("INSUFFICIENT_SCOPE", "当前凭证缺少执行此操作所需的范围")
	case connect.CodeNotFound:
		return toolError("NOT_FOUND_OR_DENIED", "资源不存在或无权访问")
	case connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
		return toolError("CONFLICT", "资源版本或幂等键冲突，请重新读取")
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return toolError("NOT_READY", "控制面暂时不可用，请稍后重试")
	default:
		return toolError("INTERNAL", "服务暂时不可用，请稍后重试")
	}
}

// bridgeRequest 构造携带用户凭证的 Connect 请求；凭证只写入单次请求头。
func bridgeRequest[T any](c *clientcli.APIClient, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+c.Token)
	return req
}

// bridgeApp 解析目标应用；空值回退 profile 默认目标，仍缺省时报参数错误。
func bridgeApp(c *clientcli.APIClient, workspaceID, applicationID string) (string, string, *sdk.CallToolResult) {
	if workspaceID == "" {
		workspaceID = c.DefaultWorkspace
	}
	if !validID(workspaceID) {
		return "", "", toolError("INVALID_ARGUMENT", "必须提供有效 workspaceId；可先用 workspace_list 查询")
	}
	if applicationID == "" {
		applicationID = c.DefaultApplication
	}
	if !validID(applicationID) {
		return "", "", toolError("INVALID_ARGUMENT", "必须提供有效 applicationId；可先用 app_list 查询")
	}
	return workspaceID, applicationID, nil
}

// ---- 输入类型：字段名与远程 MCP 工具一致，额外增加 workspaceId/applicationId
// 目标定位（远程入口由 resource 路径绑定，本地需要显式指定）。----

type bridgeListInput struct {
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"目标工作空间 ID；缺省用 profile 默认工作空间"`
	Limit       int    `json:"limit,omitempty" jsonschema:"每页数量，上限 100"`
	Cursor      string `json:"cursor,omitempty" jsonschema:"上次响应提供的分页游标"`
}

func (v bridgeListInput) valid() bool { return validPage(pageInput{Limit: v.Limit, Cursor: v.Cursor}) }

// bridgeAppListInput 是应用级工具的目标与分页输入；应用可缺省回退 profile 默认。
type bridgeAppListInput struct {
	WorkspaceID   string `json:"workspaceId,omitempty" jsonschema:"目标工作空间 ID；缺省用 profile 默认工作空间"`
	ApplicationID string `json:"applicationId,omitempty" jsonschema:"目标应用 ID；缺省用 profile 默认应用"`
	Limit         int    `json:"limit,omitempty" jsonschema:"每页数量，上限 100"`
	Cursor        string `json:"cursor,omitempty" jsonschema:"上次响应提供的分页游标"`
}

func (v bridgeAppListInput) valid() bool {
	return validPage(pageInput{Limit: v.Limit, Cursor: v.Cursor})
}

type bridgeAppCreateInput struct {
	WorkspaceID    string `json:"workspaceId,omitempty" jsonschema:"目标工作空间 ID；缺省用 profile 默认工作空间"`
	Name           string `json:"name" jsonschema:"应用名称；创建应用只填名称，其余配置创建后在应用配置页完成"`
	IdempotencyKey string `json:"idempotencyKey"`
}

func (v bridgeAppCreateInput) valid() bool {
	return v.Name != "" && len(v.Name) <= 200 && validKey(v.IdempotencyKey)
}

type bridgeSecretRef struct {
	SecretID string `json:"secretId"`
	Name     string `json:"name"`
	Version  int64  `json:"version"`
}
type bridgeOverrides struct {
	Dockerfile *string           `json:"dockerfile,omitempty"`
	Context    *string           `json:"context,omitempty"`
	Target     *string           `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	// SecretRefs 只允许引用已配置密钥的 ID 与版本，不接受明文值。
	SecretRefs []bridgeSecretRef `json:"secretRefs,omitempty"`
}

func (v bridgeOverrides) domain() *platformv1.BuildOverrides {
	out := &platformv1.BuildOverrides{Dockerfile: v.Dockerfile, Context: v.Context, Target: v.Target, Args: v.Args}
	for _, ref := range v.SecretRefs {
		out.SecretRefs = append(out.SecretRefs, &platformv1.BuildSecretRef{SecretId: ref.SecretID, Name: ref.Name, Version: ref.Version})
	}
	return out
}

type bridgeBuildCreateInput struct {
	WorkspaceID            string          `json:"workspaceId,omitempty"`
	ApplicationID          string          `json:"applicationId,omitempty"`
	SourceSnapshotID       string          `json:"sourceSnapshotId"`
	ConfigRevision         int64           `json:"configRevision"`
	ExpectedPolicyRevision int64           `json:"expectedPolicyRevision"`
	Overrides              bridgeOverrides `json:"overrides"`
	IdempotencyKey         string          `json:"idempotencyKey"`
	AutoDeploy             bool            `json:"autoDeploy"`
	RuntimeConfigRevision  int64           `json:"runtimeConfigRevision"`
	ExpectedServingRelease string          `json:"expectedServingRelease" jsonschema:"首次发布时显式传空字符串"`
}

func (v bridgeBuildCreateInput) valid() bool {
	ok := validID(v.SourceSnapshotID) && v.ConfigRevision >= 0 && v.ExpectedPolicyRevision > 0 && validKey(v.IdempotencyKey) &&
		((v.AutoDeploy && v.RuntimeConfigRevision > 0 && (v.ExpectedServingRelease == "" || validID(v.ExpectedServingRelease))) ||
			(!v.AutoDeploy && v.RuntimeConfigRevision == 0 && v.ExpectedServingRelease == ""))
	return ok
}

type bridgeIDInput struct {
	WorkspaceID   string `json:"workspaceId,omitempty"`
	ApplicationID string `json:"applicationId,omitempty"`
	ID            string `json:"id" jsonschema:"当前应用内的对象 ID"`
}
type bridgeBuildLogsInput struct {
	WorkspaceID   string `json:"workspaceId,omitempty"`
	ApplicationID string `json:"applicationId,omitempty"`
	ID            string `json:"id"`
	AfterSeq      int64  `json:"afterSeq"`
	Limit         int    `json:"limit,omitempty" jsonschema:"本次读取的日志块数，上限 16"`
}
type bridgeDeployInput struct {
	WorkspaceID            string        `json:"workspaceId,omitempty"`
	ApplicationID          string        `json:"applicationId,omitempty"`
	ArtifactID             string        `json:"artifactId"`
	RuntimeConfigRevision  int64         `json:"runtimeConfigRevision"`
	ExpectedPolicyRevision int64         `json:"expectedPolicyRevision"`
	Expected               expectedInput `json:"expected"`
	IdempotencyKey         string        `json:"idempotencyKey"`
}
type bridgeRollbackInput struct {
	WorkspaceID            string        `json:"workspaceId,omitempty"`
	ApplicationID          string        `json:"applicationId,omitempty"`
	TargetReleaseID        string        `json:"targetReleaseId"`
	ExpectedPolicyRevision int64         `json:"expectedPolicyRevision"`
	Expected               expectedInput `json:"expected"`
	IdempotencyKey         string        `json:"idempotencyKey"`
}
type bridgeChangeInput struct {
	WorkspaceID    string        `json:"workspaceId,omitempty"`
	ApplicationID  string        `json:"applicationId,omitempty"`
	Expected       expectedInput `json:"expected"`
	IdempotencyKey string        `json:"idempotencyKey"`
}
type bridgeRuntimeLogsInput struct {
	WorkspaceID   string `json:"workspaceId,omitempty"`
	ApplicationID string `json:"applicationId,omitempty"`
	Limit         int    `json:"limit,omitempty" jsonschema:"本次读取的日志块数，上限 50"`
	Cursor        string `json:"cursor,omitempty" jsonschema:"上次响应提供的分页游标"`
	PodUID        string `json:"podUid,omitempty"`
	Container     string `json:"container,omitempty"`
}
type bridgeRuntimeMetricsInput struct {
	WorkspaceID   string `json:"workspaceId,omitempty"`
	ApplicationID string `json:"applicationId,omitempty"`
	Since         string `json:"since,omitempty" jsonschema:"RFC3339 起始时间，可空"`
	Until         string `json:"until,omitempty" jsonschema:"RFC3339 截止时间，可空"`
}

// ---- 输出视图：字段名与远程 MCP 的 camelCase 视图对齐，内容来自控制面响应。
// 不含构建密钥值或敏感变量值：桥接工具集本身不提供读取密钥的能力。----

type bridgeWorkspaceView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	Role        string `json:"role,omitempty"`
}
type bridgeApplicationView struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
}
type bridgeArtifactView struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	Platform   string `json:"platform"`
	State      string `json:"state"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
}
type bridgeBuildView struct {
	ID                    string              `json:"id"`
	SourceSnapshotID      string              `json:"sourceSnapshotId"`
	Status                string              `json:"status"`
	Stage                 string              `json:"stage"`
	DesiredState          string              `json:"desiredState"`
	ErrorCode             string              `json:"errorCode,omitempty"`
	QueueReason           string              `json:"queueReason,omitempty"`
	ConfigRevision        int64               `json:"configRevision"`
	PolicyRevision        int64               `json:"policyRevision"`
	RuntimeConfigRevision *int64              `json:"runtimeConfigRevision,omitempty"`
	RetryOf               string              `json:"retryOf,omitempty"`
	LogsComplete          bool                `json:"logsComplete"`
	LogsTruncated         bool                `json:"logsTruncated"`
	CreatedAt             string              `json:"createdAt"`
	FinishedAt            string              `json:"finishedAt,omitempty"`
	Artifact              *bridgeArtifactView `json:"artifact,omitempty"`
}
type bridgeReleaseView struct {
	ID                 string   `json:"id"`
	ArtifactID         string   `json:"artifactId"`
	ArtifactDigest     string   `json:"artifactDigest"`
	ArtifactRepository string   `json:"artifactRepository"`
	Platform           string   `json:"platform"`
	ConfigRevision     int64    `json:"configRevision"`
	PreviousServingID  string   `json:"previousServingId"`
	RollbackOf         string   `json:"rollbackOf,omitempty"`
	Domains            []string `json:"domains"`
	CreatedAt          string   `json:"createdAt"`
}
type bridgeDeploymentView struct {
	Release     bridgeReleaseView `json:"release"`
	OperationID string            `json:"operationId"`
	Status      string            `json:"status"`
	Action      string            `json:"action"`
	Generation  int64             `json:"generation"`
	Active      bool              `json:"active"`
	ErrorCode   string            `json:"errorCode,omitempty"`
	CreatedAt   string            `json:"createdAt"`
	FinishedAt  string            `json:"finishedAt,omitempty"`
}
type bridgeRuntimeStatusView struct {
	WorkspaceID        string `json:"workspaceId"`
	ApplicationID      string `json:"applicationId"`
	Generation         int64  `json:"generation"`
	DesiredState       string `json:"desiredState"`
	DesiredReleaseID   string `json:"desiredReleaseId"`
	ServingReleaseID   string `json:"servingReleaseId"`
	ServingGeneration  int64  `json:"servingGeneration"`
	ObservedGeneration int64  `json:"observedGeneration"`
	ObservedState      string `json:"observedState"`
	ActiveOperationID  string `json:"activeOperationId"`
	LastObservedAt     string `json:"lastObservedAt,omitempty"`
}
type bridgeBuildLogChunk struct {
	Seq       int64  `json:"seq"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	CreatedAt string `json:"createdAt,omitempty"`
}
type bridgeBuildLogs struct {
	Items     []bridgeBuildLogChunk `json:"items"`
	NextSeq   int64                 `json:"nextSeq"`
	Complete  bool                  `json:"complete"`
	Truncated bool                  `json:"truncated"`
}
type bridgeRuntimeLogChunk struct {
	ID           string `json:"id"`
	PodUID       string `json:"podUid,omitempty"`
	Container    string `json:"container,omitempty"`
	RestartCount int32  `json:"restartCount"`
	Seq          int64  `json:"seq"`
	Content      string `json:"content"`
	Truncated    bool   `json:"truncated"`
	CreatedAt    string `json:"createdAt,omitempty"`
}
type bridgeRuntimeLogs struct {
	Chunks     []bridgeRuntimeLogChunk `json:"chunks"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}
type bridgeRuntimeMetricSample struct {
	ID                    string `json:"id"`
	ReleaseID             string `json:"releaseId,omitempty"`
	Generation            int64  `json:"generation"`
	PodUID                string `json:"podUid,omitempty"`
	CpuNanoCores          int64  `json:"cpuNanoCores"`
	CpuWindowMillis       int64  `json:"cpuWindowMillis"`
	MemoryWorkingSetBytes int64  `json:"memoryWorkingSetBytes"`
	CollectedAt           string `json:"collectedAt,omitempty"`
}

func bridgeArtifact(v *platformv1.Artifact) *bridgeArtifactView {
	if v == nil {
		return nil
	}
	return &bridgeArtifactView{ID: v.Id, Repository: v.Repository, Digest: v.Digest, Platform: v.Platform, State: v.State, VerifiedAt: v.VerifiedAt}
}
func bridgeBuild(v *platformv1.Build) bridgeBuildView {
	if v == nil {
		return bridgeBuildView{}
	}
	return bridgeBuildView{ID: v.Id, SourceSnapshotID: v.SourceSnapshotId, Status: v.State, Stage: v.Stage, DesiredState: v.DesiredState, ErrorCode: v.ErrorCode, QueueReason: v.QueueReason, ConfigRevision: v.ConfigRevision, PolicyRevision: v.PolicyRevision, RuntimeConfigRevision: v.RuntimeConfigRevision, RetryOf: v.RetryOf, LogsComplete: v.LogsComplete, LogsTruncated: v.LogsTruncated, CreatedAt: v.CreatedAt, FinishedAt: v.FinishedAt, Artifact: bridgeArtifact(v.Artifact)}
}
func bridgeDeployment(v *platformv1.Deployment) bridgeDeploymentView {
	out := bridgeDeploymentView{}
	if v == nil {
		return out
	}
	if r := v.Release; r != nil {
		domains := r.Domains
		if domains == nil {
			domains = []string{}
		}
		artifactID, digest, repository, platform := "", "", "", ""
		if a := r.Artifact; a != nil {
			artifactID, digest, repository, platform = a.Id, a.Digest, a.Repository, a.Platform
		}
		out.Release = bridgeReleaseView{ID: r.Id, ArtifactID: artifactID, ArtifactDigest: digest, ArtifactRepository: repository, Platform: platform, ConfigRevision: r.RuntimeConfigRevision, PreviousServingID: r.PreviousServingId, RollbackOf: r.RollbackOf, Domains: domains, CreatedAt: r.CreatedAt}
	}
	if o := v.Operation; o != nil {
		out.OperationID, out.Status, out.Action, out.Generation, out.Active = o.Id, o.State, o.Action, o.Generation, o.Active
		out.ErrorCode, out.CreatedAt, out.FinishedAt = o.ErrorCode, o.CreatedAt, o.FinishedAt
	}
	return out
}
func bridgeRuntimeStatus(v *platformv1.RuntimeStatus) bridgeRuntimeStatusView {
	if v == nil {
		return bridgeRuntimeStatusView{}
	}
	return bridgeRuntimeStatusView{WorkspaceID: v.WorkspaceId, ApplicationID: v.ApplicationId, Generation: v.Generation, DesiredState: v.DesiredState, DesiredReleaseID: v.DesiredReleaseId, ServingReleaseID: v.ServingReleaseId, ServingGeneration: v.ServingGeneration, ObservedGeneration: v.ObservedGeneration, ObservedState: v.ObservedState, ActiveOperationID: v.ActiveOperationId, LastObservedAt: v.LastObservedAt}
}

// registerBridgeTools 注册经控制面 API 的业务工具。工具命名、入参 schema 与
// 描述与远程 HTTP MCP 保持一致；鉴权与授权在服务端强制执行。
func registerBridgeTools(server *sdk.Server, c *clientcli.APIClient) {
	typedTool(server, "workspace_list", bridgeAuthNote+"分页列出当前用户可访问的工作空间。", annotations(true, false, true, true), func(ctx context.Context, in bridgeListInput) (any, *sdk.CallToolResult) {
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "分页参数无效")
		}
		r, err := c.Workspace.ListWorkspaces(ctx, bridgeRequest(c, &platformv1.ListWorkspacesRequest{Limit: int32(in.Limit), Cursor: in.Cursor}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		items := []bridgeWorkspaceView{}
		for _, w := range r.Msg.Workspaces {
			items = append(items, bridgeWorkspaceView{ID: w.Id, Name: w.Name, Slug: w.Slug, Description: w.Description, Status: w.Status, Role: w.Role})
		}
		return struct {
			Items      []bridgeWorkspaceView `json:"items"`
			NextCursor string                `json:"nextCursor,omitempty"`
		}{items, r.Msg.NextCursor}, nil
	})
	typedTool(server, "app_list", bridgeAuthNote+"分页列出工作空间内的应用。", annotations(true, false, true, true), func(ctx context.Context, in bridgeListInput) (any, *sdk.CallToolResult) {
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "分页参数无效")
		}
		workspace, _, failure := bridgeApp(c, in.WorkspaceID, "")
		if failure != nil {
			return nil, failure
		}
		r, err := c.Application.ListApplications(ctx, bridgeRequest(c, &platformv1.ListApplicationsRequest{WorkspaceId: workspace, Limit: int32(in.Limit), Cursor: in.Cursor}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		items := []bridgeApplicationView{}
		for _, a := range r.Msg.Applications {
			items = append(items, bridgeApplicationView{ID: a.Id, WorkspaceID: a.WorkspaceId, Name: a.Name, Description: a.Description, Status: a.Status})
		}
		return struct {
			Items      []bridgeApplicationView `json:"items"`
			NextCursor string                  `json:"nextCursor,omitempty"`
		}{items, r.Msg.NextCursor}, nil
	})
	typedTool(server, "app_create", bridgeAuthNote+"以名称创建应用，继承目标工作空间；创建后直接进入应用配置页确认，不触发构建或部署。", annotations(false, false, true, true), func(ctx context.Context, in bridgeAppCreateInput) (any, *sdk.CallToolResult) {
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "应用名称或幂等键无效")
		}
		workspace, _, failure := bridgeApp(c, in.WorkspaceID, "")
		if failure != nil {
			return nil, failure
		}
		r, err := c.Application.CreateApplication(ctx, bridgeRequest(c, &platformv1.CreateApplicationRequest{WorkspaceId: workspace, Name: in.Name, IdempotencyKey: in.IdempotencyKey}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		out := struct {
			Application       bridgeApplicationView `json:"application"`
			ConfigurationPath string                `json:"configurationPath,omitempty"`
		}{ConfigurationPath: r.Msg.ConfigurationPath}
		if a := r.Msg.Application; a != nil {
			out.Application = bridgeApplicationView{ID: a.Id, WorkspaceID: a.WorkspaceId, Name: a.Name, Description: a.Description, Status: a.Status}
		}
		return out, nil
	})
	typedTool(server, "build_create", bridgeAuthNote+"创建应用构建（引用已有源码快照），受理后继续在后台运行；相同幂等键只能提交相同请求。", annotations(false, false, true, true), func(ctx context.Context, in bridgeBuildCreateInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "构建参数、版本或幂等键无效")
		}
		r, err := c.Build.CreateBuild(ctx, bridgeRequest(c, &platformv1.CreateBuildRequest{WorkspaceId: workspace, ApplicationId: application, Input: &platformv1.BuildInput{SourceSnapshotId: in.SourceSnapshotID, ConfigRevision: in.ConfigRevision, ExpectedPolicyRevision: in.ExpectedPolicyRevision, Overrides: in.Overrides.domain(), IdempotencyKey: in.IdempotencyKey, AutoDeploy: in.AutoDeploy, RuntimeConfigRevision: in.RuntimeConfigRevision, ExpectedServingRelease: in.ExpectedServingRelease}}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeBuild(r.Msg.Build), nil
	})
	typedTool(server, "build_get", bridgeAuthNote+"读取当前应用的一次构建状态。", annotations(true, false, true, false), func(ctx context.Context, in bridgeIDInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "构建 ID 无效")
		}
		r, err := c.Build.GetBuild(ctx, bridgeRequest(c, &platformv1.GetBuildRequest{WorkspaceId: workspace, ApplicationId: application, BuildId: in.ID}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeBuild(r.Msg.Build), nil
	})
	typedTool(server, "build_list", bridgeAuthNote+"分页列出当前应用的构建。", annotations(true, false, true, false), func(ctx context.Context, in bridgeAppListInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "分页参数无效")
		}
		r, err := c.Build.ListBuilds(ctx, bridgeRequest(c, &platformv1.ListBuildsRequest{WorkspaceId: workspace, ApplicationId: application, Limit: int32(in.Limit), Cursor: in.Cursor}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		items := []bridgeBuildView{}
		for _, b := range r.Msg.Builds {
			items = append(items, bridgeBuild(b))
		}
		return struct {
			Items      []bridgeBuildView `json:"items"`
			NextCursor string            `json:"nextCursor,omitempty"`
		}{items, r.Msg.NextCursor}, nil
	})
	typedTool(server, "build_logs", bridgeAuthNote+"分页读取原始构建日志，afterSeq 从零开始，每次最多 16 块；日志可能含用户输出，请勿把秘密写入日志。", annotations(true, false, true, false), func(ctx context.Context, in bridgeBuildLogsInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validID(in.ID) || in.AfterSeq < 0 || in.Limit < 0 || in.Limit > 16 {
			return nil, toolError("INVALID_ARGUMENT", "日志参数无效，每次最多 16 块")
		}
		if in.Limit == 0 {
			in.Limit = 16
		}
		r, err := c.Build.ReadBuildLogs(ctx, bridgeRequest(c, &platformv1.ReadBuildLogsRequest{WorkspaceId: workspace, ApplicationId: application, BuildId: in.ID, AfterSeq: in.AfterSeq, Limit: int32(in.Limit)}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		items := []bridgeBuildLogChunk{}
		for _, chunk := range r.Msg.Chunks {
			items = append(items, bridgeBuildLogChunk{Seq: chunk.Seq, Content: chunk.Content, Truncated: chunk.Truncated, CreatedAt: chunk.CreatedAt})
		}
		return bridgeBuildLogs{Items: items, NextSeq: r.Msg.NextSeq, Complete: r.Msg.Complete, Truncated: r.Msg.Truncated}, nil
	})
	typedTool(server, "build_cancel", bridgeAuthNote+"请求停止构建，停止需等待 worker 确认，不能提前宣告任务已停止。", annotations(false, true, true, true), func(ctx context.Context, in bridgeIDInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "构建 ID 无效")
		}
		r, err := c.Build.CancelBuild(ctx, bridgeRequest(c, &platformv1.CancelBuildRequest{WorkspaceId: workspace, ApplicationId: application, BuildId: in.ID}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeBuild(r.Msg.Build), nil
	})
	status := func(ctx context.Context, in bridgeAppListInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		r, err := c.Deployment.GetRuntimeStatus(ctx, bridgeRequest(c, &platformv1.GetRuntimeStatusRequest{WorkspaceId: workspace, ApplicationId: application}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeRuntimeStatus(r.Msg.Status), nil
	}
	typedTool(server, "app_status", bridgeAuthNote+"读取应用当前期望状态、实际观察与正在服务的 release；缺测不会当作正常。", annotations(true, false, true, false), status)
	typedTool(server, "runtime_status", bridgeAuthNote+"读取应用运行状态及已生效版本。", annotations(true, false, true, false), status)
	typedTool(server, "deployment_create", bridgeAuthNote+"使用已验证产物和固定配置版本创建发布；返回操作状态而非提前宣告上线。", annotations(false, true, true, true), func(ctx context.Context, in bridgeDeployInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validID(in.ArtifactID) || in.RuntimeConfigRevision < 1 || in.ExpectedPolicyRevision < 1 || !in.Expected.valid() || in.Expected.ConfigRevision != in.RuntimeConfigRevision || !validKey(in.IdempotencyKey) {
			return nil, toolError("INVALID_ARGUMENT", "产物、版本或幂等键无效")
		}
		r, err := c.Deployment.CreateDeployment(ctx, bridgeRequest(c, &platformv1.CreateDeploymentRequest{WorkspaceId: workspace, ApplicationId: application, ArtifactId: in.ArtifactID, RuntimeConfigRevision: in.RuntimeConfigRevision, ExpectedPolicyRevision: in.ExpectedPolicyRevision, Expected: &platformv1.ExpectedRuntime{Generation: in.Expected.Generation, ServingReleaseId: in.Expected.ServingReleaseID, ConfigRevision: in.Expected.ConfigRevision}, IdempotencyKey: in.IdempotencyKey}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeDeployment(r.Msg.Deployment), nil
	})
	typedTool(server, "deployment_get", bridgeAuthNote+"读取当前应用的发布操作及其不可变 release。", annotations(true, false, true, false), func(ctx context.Context, in bridgeIDInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "操作 ID 无效")
		}
		r, err := c.Deployment.GetDeployment(ctx, bridgeRequest(c, &platformv1.GetDeploymentRequest{WorkspaceId: workspace, ApplicationId: application, OperationId: in.ID}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeDeployment(r.Msg.Deployment), nil
	})
	typedTool(server, "deployment_list", bridgeAuthNote+"分页列出当前应用发布操作和 release。", annotations(true, false, true, false), func(ctx context.Context, in bridgeAppListInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !in.valid() {
			return nil, toolError("INVALID_ARGUMENT", "分页参数无效")
		}
		r, err := c.Deployment.ListDeployments(ctx, bridgeRequest(c, &platformv1.ListDeploymentsRequest{WorkspaceId: workspace, ApplicationId: application, Limit: int32(in.Limit), Cursor: in.Cursor}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		items := []bridgeDeploymentView{}
		for _, d := range r.Msg.Deployments {
			items = append(items, bridgeDeployment(d))
		}
		return struct {
			Items      []bridgeDeploymentView `json:"items"`
			NextCursor string                 `json:"nextCursor,omitempty"`
		}{items, r.Msg.NextCursor}, nil
	})
	typedTool(server, "deployment_rollback", bridgeAuthNote+"回滚到当前应用的历史成功 release；不会回退持久数据。", annotations(false, true, true, true), func(ctx context.Context, in bridgeRollbackInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validID(in.TargetReleaseID) || in.ExpectedPolicyRevision < 1 || !in.Expected.valid() || !validKey(in.IdempotencyKey) {
			return nil, toolError("INVALID_ARGUMENT", "目标、版本或幂等键无效")
		}
		r, err := c.Deployment.RollbackDeployment(ctx, bridgeRequest(c, &platformv1.RollbackDeploymentRequest{WorkspaceId: workspace, ApplicationId: application, TargetReleaseId: in.TargetReleaseID, ExpectedPolicyRevision: in.ExpectedPolicyRevision, Expected: &platformv1.ExpectedRuntime{Generation: in.Expected.Generation, ServingReleaseId: in.Expected.ServingReleaseID, ConfigRevision: in.Expected.ConfigRevision}, IdempotencyKey: in.IdempotencyKey}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		return bridgeDeployment(r.Msg.Deployment), nil
	})
	actions := []struct {
		name, description, action string
	}{
		{"runtime_start", "启动当前应用服务。", "START"},
		{"runtime_stop", "停止当前应用服务；保留配置和持久数据。", "STOP"},
		{"runtime_restart", "重启当前服务版本。", "RESTART"},
	}
	for _, item := range actions {
		item := item
		typedTool(server, item.name, bridgeAuthNote+item.description, annotations(false, true, true, true), func(ctx context.Context, in bridgeChangeInput) (any, *sdk.CallToolResult) {
			workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
			if failure != nil {
				return nil, failure
			}
			if !in.Expected.valid() || !validKey(in.IdempotencyKey) {
				return nil, toolError("INVALID_ARGUMENT", "预期版本或幂等键无效")
			}
			r, err := c.Deployment.ChangeRuntime(ctx, bridgeRequest(c, &platformv1.ChangeRuntimeRequest{WorkspaceId: workspace, ApplicationId: application, Action: item.action, Expected: &platformv1.ExpectedRuntime{Generation: in.Expected.Generation, ServingReleaseId: in.Expected.ServingReleaseID, ConfigRevision: in.Expected.ConfigRevision}, IdempotencyKey: in.IdempotencyKey}))
			if err != nil {
				return nil, bridgeRPCError(err)
			}
			return bridgeDeployment(r.Msg.Deployment), nil
		})
	}
	typedTool(server, "runtime_logs", bridgeAuthNote+"分页读取运行日志，cursor 从空开始，每次最多 50 块；日志可能含用户输出，请勿把秘密写入日志。", annotations(true, false, true, false), func(ctx context.Context, in bridgeRuntimeLogsInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		if !validPage(pageInput{Limit: in.Limit, Cursor: in.Cursor}) || in.Limit > 50 {
			return nil, toolError("INVALID_ARGUMENT", "日志参数无效，每次最多 50 块")
		}
		if in.Limit == 0 {
			in.Limit = 50
		}
		r, err := c.Deployment.GetRuntimeLogs(ctx, bridgeRequest(c, &platformv1.GetRuntimeLogsRequest{WorkspaceId: workspace, ApplicationId: application, Limit: int32(in.Limit), Cursor: in.Cursor, PodUid: in.PodUID, Container: in.Container}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		chunks := []bridgeRuntimeLogChunk{}
		for _, chunk := range r.Msg.Chunks {
			chunks = append(chunks, bridgeRuntimeLogChunk{ID: chunk.Id, PodUID: chunk.PodUid, Container: chunk.Container, RestartCount: chunk.RestartCount, Seq: chunk.Seq, Content: string(chunk.Content), Truncated: chunk.Truncated, CreatedAt: chunk.CreatedAt})
		}
		return bridgeRuntimeLogs{Chunks: chunks, NextCursor: r.Msg.NextCursor}, nil
	})
	typedTool(server, "runtime_metrics", bridgeAuthNote+"查询应用运行指标采样（CPU/内存），可用 since/until 限定 RFC3339 时间范围。", annotations(true, false, true, false), func(ctx context.Context, in bridgeRuntimeMetricsInput) (any, *sdk.CallToolResult) {
		workspace, application, failure := bridgeApp(c, in.WorkspaceID, in.ApplicationID)
		if failure != nil {
			return nil, failure
		}
		r, err := c.Deployment.GetRuntimeMetrics(ctx, bridgeRequest(c, &platformv1.GetRuntimeMetricsRequest{WorkspaceId: workspace, ApplicationId: application, Since: in.Since, Until: in.Until}))
		if err != nil {
			return nil, bridgeRPCError(err)
		}
		samples := []bridgeRuntimeMetricSample{}
		for _, s := range r.Msg.Samples {
			samples = append(samples, bridgeRuntimeMetricSample{ID: s.Id, ReleaseID: s.ReleaseId, Generation: s.Generation, PodUID: s.PodUid, CpuNanoCores: s.CpuNanoCores, CpuWindowMillis: s.CpuWindowMillis, MemoryWorkingSetBytes: s.MemoryWorkingSetBytes, CollectedAt: s.CollectedAt})
		}
		return struct {
			Samples []bridgeRuntimeMetricSample `json:"samples"`
		}{samples}, nil
	})
}
