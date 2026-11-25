package clientcli

import (
	"time"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
)

func (a *clientApp) statusCommand() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "查看当前目录关联应用的运行状态", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, ws, app, err := a.currentProject()
		if err != nil {
			return err
		}
		return c.runtimeStatus(cmd, ws, app)
	}}
}

func (a *clientApp) metricsCommand() *cobra.Command {
	var since, until string
	cmd := &cobra.Command{Use: "metrics", Short: "读取当前目录关联应用的 CPU 与内存采样", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, ws, app, err := a.currentProject()
		if err != nil {
			return err
		}
		res, err := c.deployment.GetRuntimeMetrics(cmd.Context(), auth(c.token, &platformv1.GetRuntimeMetricsRequest{WorkspaceId: ws, ApplicationId: app, Since: since, Until: until}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, res.Msg)
	}}
	cmd.Flags().StringVar(&since, "since", "", "RFC3339 起始时间")
	cmd.Flags().StringVar(&until, "until", "", "RFC3339 截止时间")
	return cmd
}

func (a *clientApp) currentProject() (*apiClient, string, string, error) {
	c, p, err := a.client()
	if err != nil {
		return nil, "", "", err
	}
	dir, err := projectDirectory(".")
	if err != nil {
		return nil, "", "", configError(err)
	}
	ws, app, err := a.projectTarget(p, dir)
	return c, ws, app, err
}

func (c *apiClient) runtimeStatus(cmd *cobra.Command, ws, app string) error {
	res, err := c.deployment.GetRuntimeStatus(cmd.Context(), auth(c.token, &platformv1.GetRuntimeStatusRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil {
		return rpcError(err)
	}
	return output(cmd, res.Msg)
}

func (a *clientApp) logsCommand() *cobra.Command {
	var buildID, cursor, since, until string
	var limit int32
	var follow bool
	cmd := &cobra.Command{Use: "logs", Short: "读取当前目录关联应用的日志", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, ws, app, err := a.currentProject()
		if err != nil {
			return err
		}
		if buildID != "" {
			if since != "" || until != "" {
				return fail("INVALID_ARGUMENT", 2, false, "时间范围选项用于运行日志")
			}
			if cursor != "" {
				return fail("INVALID_ARGUMENT", 2, false, "--cursor 用于运行日志；按序号续读构建日志请使用 yummy build logs --after")
			}
			return a.buildLogs(cmd, c, ws, app, buildID, 0, limit, follow)
		}
		for {
			res, err := c.deployment.GetRuntimeLogs(cmd.Context(), auth(c.token, &platformv1.GetRuntimeLogsRequest{WorkspaceId: ws, ApplicationId: app, Cursor: cursor, Limit: limit, Since: since, Until: until}))
			if err != nil {
				return rpcError(err)
			}
			if !follow || len(res.Msg.Chunks) > 0 {
				if err := output(cmd, res.Msg); err != nil || !follow {
					return err
				}
			}
			next := res.Msg.GetResumeCursor()
			if len(res.Msg.Chunks) > 0 && (next == "" || next == cursor) {
				return fail("INVALID_RESPONSE", 1, false, "控制面未返回新的续读游标")
			}
			if next != "" {
				cursor = next
			}
			if res.Msg.GetNextCursor() == "" {
				if err := waitContext(cmd, time.Second); err != nil {
					return err
				}
			}
			c, err = a.clientForServer(c.base)
			if err != nil {
				return err
			}
		}
	}}
	cmd.Flags().StringVar(&buildID, "build", "", "查看指定构建的日志")
	cmd.Flags().StringVar(&cursor, "cursor", "", "从返回的运行日志游标继续读取")
	cmd.Flags().StringVar(&since, "since", "", "RFC3339 起始时间")
	cmd.Flags().StringVar(&until, "until", "", "RFC3339 截止时间")
	cmd.Flags().Int32Var(&limit, "limit", 100, "单次读取的日志块数")
	cmd.Flags().BoolVar(&follow, "follow", false, "连续读取 JSON 行")
	return cmd
}
