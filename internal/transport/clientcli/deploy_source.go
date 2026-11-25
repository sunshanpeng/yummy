package clientcli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

type deploySourceOptions struct {
	dryRun, buildOnly, noWait bool
	timeout                   time.Duration
}

type deploySourcePreview struct {
	WorkspaceID   string                       `json:"workspace_id"`
	ApplicationID string                       `json:"application_id"`
	Execution     *platformv1.ExecutionSummary `json:"execution"`
	sourcePreview
}

func (a *clientApp) deploySourceCommand() *cobra.Command {
	var options deploySourceOptions
	cmd := &cobra.Command{Use: "deploy [目录]", Short: "从本地目录构建发布，或管理已有发布", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if options.timeout <= 0 {
				return fail("INVALID_ARGUMENT", 2, false, "等待时限必须大于零")
			}
			directory := "."
			if len(args) != 0 {
				directory = args[0]
			}
			root, err := projectDirectory(directory)
			if err != nil {
				return configError(err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), options.timeout)
			defer cancel()
			cmd.SetContext(ctx)
			a.ctx = ctx
			return a.deploySource(cmd, root, options)
		},
	}
	cmd.Flags().BoolVar(&options.dryRun, "dry-run", false, "预览上传文件、构建方式和目标，不上传")
	cmd.Flags().BoolVar(&options.buildOnly, "build-only", false, "构建并验证镜像，不发布")
	cmd.Flags().BoolVar(&options.noWait, "no-wait", false, "构建受理后返回，由控制面继续处理")
	cmd.Flags().DurationVar(&options.timeout, "timeout", 75*time.Minute, "客户端等待时限；到期不取消服务端任务")
	return cmd
}

func (a *clientApp) deploySource(cmd *cobra.Command, root string, options deploySourceOptions) error {
	ctx := cmd.Context()
	c, profile, err := a.client()
	if err != nil {
		return err
	}
	ws, app, err := a.projectTarget(profile, root)
	if err != nil {
		return err
	}
	configResponse, err := c.build.GetBuildConfig(ctx, auth(c.token, &platformv1.GetBuildConfigRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil {
		return rpcError(err)
	}
	config := configResponse.Msg.GetConfig()
	if config == nil {
		return fail("INVALID_RESPONSE", 1, false, "未返回应用构建配置")
	}
	resolved, err := c.inventory.ResolveExecution(ctx, auth(c.token, &platformv1.ResolveExecutionRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil {
		return rpcError(err)
	}
	execution := resolved.Msg.GetSummary()
	if execution == nil {
		return fail("INVALID_RESPONSE", 1, false, "未返回构建执行策略")
	}
	archiveOptions := sourcearchive.Options{Root: root, AllowRoots: []string{root}, Context: config.GetContext(), Dockerfile: config.GetDockerfile()}
	if options.dryRun {
		preview, err := sourcearchive.Preview(ctx, archiveOptions)
		if err != nil {
			return configError(err)
		}
		return output(cmd, deploySourcePreview{WorkspaceID: ws, ApplicationID: app, Execution: execution, sourcePreview: sourcePreview{BuildMethod: previewBuildMethod(preview), Result: preview}})
	}
	if !execution.GetReady() || execution.GetPolicyRevision() < 1 {
		return fail("BUILD_NOT_READY", 6, false, "构建条件未就绪。"+execution.GetReason())
	}

	input := &platformv1.BuildInput{
		ConfigRevision: configResponse.Msg.GetRevision(), ExpectedPolicyRevision: execution.GetPolicyRevision(),
	}
	var expected *platformv1.ExpectedRuntime
	if !options.buildOnly {
		expected, err = c.deploymentExpected(ctx, ws, app)
		if err != nil {
			return err
		}
		if options.noWait {
			input.AutoDeploy = true
			input.RuntimeConfigRevision = expected.GetConfigRevision()
			input.ExpectedServingRelease = expected.GetServingReleaseId()
			input.ExpectedRuntimeGeneration = &expected.Generation
		}
	}
	uploaded, err := c.uploadSource(ctx, ws, app, archiveOptions)
	if err != nil {
		return err
	}
	input.SourceSnapshotId = uploaded.GetSnapshot().GetId()
	dockerfile, contextRoot := uploaded.GetSnapshot().GetDockerfile(), "."
	// 本机归档只包含选定上下文，归档内的 Dockerfile 路径已相对该上下文。
	input.Overrides = &platformv1.BuildOverrides{Context: &contextRoot, Dockerfile: &dockerfile}
	preview, err := c.build.PreviewBuild(ctx, auth(c.token, &platformv1.PreviewBuildRequest{WorkspaceId: ws, ApplicationId: app, Input: input}))
	if err != nil {
		return rpcError(err)
	}
	if !preview.Msg.GetPreview().GetExecution().GetReady() {
		return fail("BUILD_NOT_READY", 6, false, "构建预检未通过。"+preview.Msg.GetPreview().GetExecution().GetReason())
	}
	input.IdempotencyKey, err = newKey()
	if err != nil {
		return err
	}
	created, err := c.build.CreateBuild(ctx, auth(c.token, &platformv1.CreateBuildRequest{WorkspaceId: ws, ApplicationId: app, Input: input}))
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "源码快照 %s，构建请求幂等键 %s。请先核对构建列表。\n", input.SourceSnapshotId, input.IdempotencyKey)
		return rpcError(err)
	}
	currentBuild := created.Msg.GetBuild()
	if currentBuild.GetId() == "" {
		return fail("INVALID_RESPONSE", 1, false, "未返回构建 ID")
	}
	if options.noWait {
		return output(cmd, created.Msg)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "构建已受理。可用 yummy build get %s --workspace %s --application %s 查看进度。\n", currentBuild.GetId(), ws, app)
	currentBuild, err = a.waitSourceBuild(cmd, c.base, ws, app, currentBuild)
	if err != nil {
		return err
	}
	if options.buildOnly {
		return output(cmd, &platformv1.GetBuildResponse{Build: currentBuild})
	}
	// 冻结构建开始前的运行与配置版本，若期间已有发布或配置保存，服务端拒绝覆盖。
	return a.releaseSource(cmd, c.base, ws, app, currentBuild, execution.GetPolicyRevision(), expected)
}

func (c *apiClient) deploymentExpected(ctx context.Context, ws, app string) (*platformv1.ExpectedRuntime, error) {
	config, err := c.runtime.GetRuntimeConfig(ctx, auth(c.token, &platformv1.GetRuntimeConfigRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil {
		return nil, rpcError(err)
	}
	if config.Msg.GetConfig().GetRevision() < 1 || !config.Msg.GetConfig().GetConfirmed() {
		return nil, fail("RUNTIME_CONFIG_UNCONFIRMED", 6, false, "请先保存并确认应用运行配置")
	}
	state, err := c.deployment.GetRuntimeStatus(ctx, auth(c.token, &platformv1.GetRuntimeStatusRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil {
		return nil, rpcError(err)
	}
	if state.Msg.GetStatus() == nil {
		return nil, fail("INVALID_RESPONSE", 1, false, "未返回应用运行状态")
	}
	status := state.Msg.GetStatus()
	return &platformv1.ExpectedRuntime{Generation: status.GetGeneration(), ServingReleaseId: status.GetServingReleaseId(), ConfigRevision: config.Msg.GetConfig().GetRevision()}, nil
}

func (a *clientApp) waitSourceBuild(cmd *cobra.Command, base, ws, app string, build *platformv1.Build) (*platformv1.Build, error) {
	id := build.GetId()
	for {
		switch build.GetState() {
		case "SUCCEEDED":
			if build.GetArtifact().GetId() == "" || build.GetArtifact().GetState() != "VERIFIED" {
				return nil, fail("INVALID_RESPONSE", 1, false, "构建返回成功但缺少已验证产物")
			}
			return build, nil
		case "FAILED", "CANCELED", "TIMED_OUT", "LOST":
			return nil, fail("BUILD_FAILED", 1, false, fmt.Sprintf("构建 %s 结束于 %s（%s）", id, build.GetState(), build.GetErrorCode()))
		}
		if err := waitContext(cmd, 2*time.Second); err != nil {
			return nil, err
		}
		c, err := a.clientForServer(base)
		if err != nil {
			return nil, err
		}
		r, err := c.build.GetBuild(cmd.Context(), auth(c.token, &platformv1.GetBuildRequest{WorkspaceId: ws, ApplicationId: app, BuildId: id}))
		if err != nil {
			return nil, rpcError(err)
		}
		build = r.Msg.GetBuild()
		if build.GetId() != id {
			return nil, fail("INVALID_RESPONSE", 1, false, "未返回当前构建")
		}
	}
}

func (a *clientApp) releaseSource(cmd *cobra.Command, base, ws, app string, build *platformv1.Build, policy int64, expected *platformv1.ExpectedRuntime) error {
	c, err := a.clientForServer(base)
	if err != nil {
		return err
	}
	key, err := newKey()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	created, err := c.deployment.CreateDeployment(ctx, auth(c.token, &platformv1.CreateDeploymentRequest{
		WorkspaceId: ws, ApplicationId: app, ArtifactId: build.GetArtifact().GetId(), RuntimeConfigRevision: expected.GetConfigRevision(),
		ExpectedPolicyRevision: policy, Expected: expected, IdempotencyKey: key,
	}))
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "已验证产物 %s，发布请求幂等键 %s。请先核对发布列表。\n", build.GetArtifact().GetId(), key)
		return rpcError(err)
	}
	deployment := created.Msg.GetDeployment()
	id := deployment.GetOperation().GetId()
	if id == "" {
		return fail("INVALID_RESPONSE", 1, false, "未返回发布操作 ID")
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "发布已受理。可用 yummy deploy get %s --workspace %s --application %s 查看进度。\n", id, ws, app)
	for {
		switch operation := deployment.GetOperation(); operation.GetState() {
		case "SUCCEEDED":
			return output(cmd, &platformv1.GetDeploymentResponse{Deployment: deployment})
		case "FAILED", "SUPERSEDED":
			return fail("DEPLOYMENT_FAILED", 1, false, fmt.Sprintf("发布 %s 结束于 %s（%s）", id, operation.GetState(), operation.GetErrorCode()))
		}
		if err := waitContext(cmd, 2*time.Second); err != nil {
			return err
		}
		c, err := a.clientForServer(base)
		if err != nil {
			return err
		}
		result, err := c.deployment.GetDeployment(ctx, auth(c.token, &platformv1.GetDeploymentRequest{WorkspaceId: ws, ApplicationId: app, OperationId: id}))
		if err != nil {
			return rpcError(err)
		}
		deployment = result.Msg.GetDeployment()
		if deployment.GetOperation().GetId() != id {
			return fail("INVALID_RESPONSE", 1, false, "未返回当前发布操作")
		}
	}
}
