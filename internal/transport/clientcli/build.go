package clientcli

import (
	"time"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
)

func (a *clientApp) buildCommand() *cobra.Command {
	root := &cobra.Command{Use: "build", Short: "应用构建"}
	config := &cobra.Command{Use: "config", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		r, e := c.build.GetBuildConfig(cmd.Context(), auth(c.token, &platformv1.GetBuildConfigRequest{WorkspaceId: w, ApplicationId: id}))
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
		r, e := c.build.ListBuilds(cmd.Context(), auth(c.token, &platformv1.ListBuildsRequest{WorkspaceId: w, ApplicationId: id, Limit: limit, Cursor: cursor}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	list.Flags().Int32Var(&limit, "limit", 50, "分页大小")
	list.Flags().StringVar(&cursor, "cursor", "", "分页游标")
	get := &cobra.Command{Use: "get BUILD_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		r, e := c.build.GetBuild(cmd.Context(), auth(c.token, &platformv1.GetBuildRequest{WorkspaceId: w, ApplicationId: id, BuildId: args[0]}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	cancel := &cobra.Command{Use: "cancel BUILD_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		r, e := c.build.CancelBuild(cmd.Context(), auth(c.token, &platformv1.CancelBuildRequest{WorkspaceId: w, ApplicationId: id, BuildId: args[0]}))
		if e != nil {
			return rpcError(e)
		}
		return output(cmd, r.Msg)
	}}
	var after int64
	var logLimit int32
	var follow bool
	logs := &cobra.Command{Use: "logs BUILD_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		for {
			r, e := c.build.ReadBuildLogs(cmd.Context(), auth(c.token, &platformv1.ReadBuildLogsRequest{WorkspaceId: w, ApplicationId: id, BuildId: args[0], AfterSeq: after, Limit: logLimit}))
			if e != nil {
				return rpcError(e)
			}
			if e = output(cmd, r.Msg); e != nil {
				return e
			}
			if !follow || r.Msg.Complete {
				return nil
			}
			after = r.Msg.NextSeq
			if e = waitContext(cmd, time.Second); e != nil {
				return e
			}
		}
	}}
	logs.Flags().Int64Var(&after, "after", 0, "日志序号游标")
	logs.Flags().Int32Var(&logLimit, "limit", 100, "日志块数上限")
	logs.Flags().BoolVar(&follow, "follow", false, "连续读取 JSON 行，退出不会取消构建")
	watch := &cobra.Command{Use: "watch BUILD_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, e := a.client()
		if e != nil {
			return e
		}
		w, id, e := requireTarget(p)
		if e != nil {
			return e
		}
		for {
			r, e := c.build.GetBuild(cmd.Context(), auth(c.token, &platformv1.GetBuildRequest{WorkspaceId: w, ApplicationId: id, BuildId: args[0]}))
			if e != nil {
				return rpcError(e)
			}
			if e = output(cmd, r.Msg); e != nil {
				return e
			}
			if r.Msg.Build == nil {
				return fail("INVALID_RESPONSE", 1, false, "服务端未返回构建")
			}
			switch r.Msg.Build.State {
			case "SUCCEEDED", "FAILED", "CANCELED", "TIMED_OUT", "LOST":
				return nil
			}
			if e = waitContext(cmd, time.Second); e != nil {
				return e
			}
		}
	}}
	newSubmit := func(kind string) *cobra.Command {
		var snapshot, key, serving string
		var configRev, policyRev, runtimeRev int64
		var auto bool
		cmd := &cobra.Command{Use: kind, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			c, p, e := a.client()
			if e != nil {
				return e
			}
			w, id, e := requireTarget(p)
			if e != nil {
				return e
			}
			in := &platformv1.BuildInput{SourceSnapshotId: snapshot, ConfigRevision: configRev, ExpectedPolicyRevision: policyRev, AutoDeploy: auto, RuntimeConfigRevision: runtimeRev, ExpectedServingRelease: serving}
			if kind == "preview" {
				r, e := c.build.PreviewBuild(cmd.Context(), auth(c.token, &platformv1.PreviewBuildRequest{WorkspaceId: w, ApplicationId: id, Input: in}))
				if e != nil {
					return rpcError(e)
				}
				return output(cmd, r.Msg)
			}
			in.IdempotencyKey, e = mutationKey(cmd, key)
			if e != nil {
				return e
			}
			if kind == "retry BUILD_ID" {
				r, e := c.build.RetryBuild(cmd.Context(), auth(c.token, &platformv1.RetryBuildRequest{WorkspaceId: w, ApplicationId: id, BuildId: args[0], Input: in}))
				if e != nil {
					return rpcError(e)
				}
				return output(cmd, r.Msg)
			}
			r, e := c.build.CreateBuild(cmd.Context(), auth(c.token, &platformv1.CreateBuildRequest{WorkspaceId: w, ApplicationId: id, Input: in}))
			if e != nil {
				return rpcError(e)
			}
			return output(cmd, r.Msg)
		}}
		if kind == "retry BUILD_ID" {
			cmd.Args = cobra.ExactArgs(1)
		}
		cmd.Flags().StringVar(&snapshot, "snapshot", "", "已有源码快照 ID")
		cmd.Flags().Int64Var(&configRev, "config-revision", 0, "构建配置版本")
		cmd.Flags().Int64Var(&policyRev, "policy-revision", 0, "预览取得的策略版本")
		cmd.Flags().BoolVar(&auto, "auto-deploy", false, "构建成功后部署")
		cmd.Flags().Int64Var(&runtimeRev, "runtime-revision", 0, "运行配置版本")
		cmd.Flags().StringVar(&serving, "expected-serving-release", "", "预期当前发布 ID")
		if kind != "preview" {
			cmd.Flags().StringVar(&key, "idempotency-key", "", "重试沿用同一键与输入")
		}
		return cmd
	}
	root.AddCommand(config, list, get, cancel, logs, watch, newSubmit("preview"), newSubmit("create"), newSubmit("retry BUILD_ID"))
	return root
}
func waitContext(cmd *cobra.Command, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-cmd.Context().Done():
		return rpcError(cmd.Context().Err())
	case <-timer.C:
		return nil
	}
}
