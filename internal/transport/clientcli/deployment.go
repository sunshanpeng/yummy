package clientcli

import (
	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
)

type expectedFlags struct {
	generation, config, policy int64
	serving, key               string
}

func (f *expectedFlags) bind(cmd *cobra.Command, policy bool) {
	cmd.Flags().Int64Var(&f.generation, "expected-generation", 0, "预期运行 generation")
	cmd.Flags().Int64Var(&f.config, "expected-config-revision", 0, "预期配置版本")
	cmd.Flags().StringVar(&f.serving, "expected-serving-release", "", "预期正在服务的 release ID；首次部署可为空")
	cmd.Flags().StringVar(&f.key, "idempotency-key", "", "重试沿用同一键与输入")
	_ = cmd.MarkFlagRequired("expected-generation")
	_ = cmd.MarkFlagRequired("expected-config-revision")
	_ = cmd.MarkFlagRequired("expected-serving-release")
	if policy {
		cmd.Flags().Int64Var(&f.policy, "policy-revision", 0, "预期执行策略版本")
		_ = cmd.MarkFlagRequired("policy-revision")
	}
}
func (f expectedFlags) expected() *platformv1.ExpectedRuntime {
	return &platformv1.ExpectedRuntime{Generation: f.generation, ServingReleaseId: f.serving, ConfigRevision: f.config}
}
func (a *clientApp) deploymentCommand() *cobra.Command {
	root := a.deploySourceCommand()
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		return c.runtimeStatus(cmd, w, id)
	}}
	get := &cobra.Command{Use: "get OPERATION_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		r, e := c.deployment.GetDeployment(cmd.Context(), auth(c.token, &platformv1.GetDeploymentRequest{WorkspaceId: w, ApplicationId: id, OperationId: args[0]}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	var limit int32
	var cursor string
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		r, e := c.deployment.ListDeployments(cmd.Context(), auth(c.token, &platformv1.ListDeploymentsRequest{WorkspaceId: w, ApplicationId: id, Limit: limit, Cursor: cursor}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	list.Flags().Int32Var(&limit, "limit", 50, "分页大小")
	list.Flags().StringVar(&cursor, "cursor", "", "分页游标")
	var f expectedFlags
	var runtimeRevision int64
	create := &cobra.Command{Use: "create ARTIFACT_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		key, e := mutationKey(cmd, f.key)
		if e != nil {
			return e
		}
		r, e := c.deployment.CreateDeployment(cmd.Context(), auth(c.token, &platformv1.CreateDeploymentRequest{WorkspaceId: w, ApplicationId: id, ArtifactId: args[0], RuntimeConfigRevision: runtimeRevision, ExpectedPolicyRevision: f.policy, Expected: f.expected(), IdempotencyKey: key}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	f.bind(create, true)
	create.Flags().Int64Var(&runtimeRevision, "runtime-revision", 0, "要发布的运行配置版本")
	_ = create.MarkFlagRequired("runtime-revision")
	var rb expectedFlags
	rollback := &cobra.Command{Use: "rollback RELEASE_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		key, e := mutationKey(cmd, rb.key)
		if e != nil {
			return e
		}
		r, e := c.deployment.RollbackDeployment(cmd.Context(), auth(c.token, &platformv1.RollbackDeploymentRequest{WorkspaceId: w, ApplicationId: id, TargetReleaseId: args[0], ExpectedPolicyRevision: rb.policy, Expected: rb.expected(), IdempotencyKey: key}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	rb.bind(rollback, true)
	root.AddCommand(status, get, list, create, rollback)
	for _, action := range []string{"start", "stop", "restart"} {
		action := action
		var eflags expectedFlags
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			c, p, e := a.client()
			if e != nil {
				return e
			}
			w, id, e := requireTarget(p)
			if e != nil {
				return e
			}
			key, e := mutationKey(cmd, eflags.key)
			if e != nil {
				return e
			}
			actions := map[string]string{"start": "START", "stop": "STOP", "restart": "RESTART"}
			r, e := c.deployment.ChangeRuntime(cmd.Context(), auth(c.token, &platformv1.ChangeRuntimeRequest{WorkspaceId: w, ApplicationId: id, Action: actions[action], Expected: eflags.expected(), IdempotencyKey: key}))
			if e != nil {
				return rpcError(e)
			}
			return output(cmd, r.Msg)
		}}
		eflags.bind(cmd, false)
		root.AddCommand(cmd)
	}
	return root
}
