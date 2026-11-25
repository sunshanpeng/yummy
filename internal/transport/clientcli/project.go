package clientcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/lib/naming"
)

func projectDirectory(input string) (string, error) {
	path, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("源码路径必须是目录")
	}
	return path, nil
}

func (a *clientApp) useProjectCommand() *cobra.Command {
	return &cobra.Command{Use: "use 空间名/应用名", Short: "关联当前目录与应用", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		space, app, ok := strings.Cut(args[0], "/")
		if !ok || !naming.ValidWorkspaceSlug(space) || !naming.ValidName(app) {
			return fail("INVALID_ARGUMENT", 2, false, "请使用 空间名/应用名 格式")
		}
		directory, err := projectDirectory(".")
		if err != nil {
			return configError(err)
		}
		c, selected, err := a.client()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		ws, err := c.workspace.GetWorkspace(ctx, auth(c.token, &platformv1.GetWorkspaceRequest{Slug: space}))
		if err != nil {
			return rpcError(err)
		}
		workspaceID := ws.Msg.GetWorkspace().GetId()
		if workspaceID == "" {
			return fail("INVALID_RESPONSE", 1, false, "未返回工作空间 ID")
		}
		result, err := c.application.GetApplication(ctx, auth(c.token, &platformv1.GetApplicationRequest{WorkspaceId: workspaceID, ApplicationName: app}))
		if err != nil {
			return rpcError(err)
		}
		applicationID := result.Msg.GetApplication().GetId()
		if applicationID == "" || result.Msg.GetApplication().GetWorkspaceId() != workspaceID {
			return fail("INVALID_RESPONSE", 1, false, "未返回该工作空间的应用")
		}
		err = a.store.update(a.v.GetString("profile"), func(stored *profile) error {
			if stored.Server != "" && stored.Server != selected.Server {
				return errors.New("实例地址与 profile 不匹配，请先 config set")
			}
			stored.Server = selected.Server
			if stored.Projects == nil {
				stored.Projects = make(map[string]projectTarget)
			}
			stored.Projects[directory] = projectTarget{Workspace: workspaceID, Application: applicationID}
			return nil
		})
		if err != nil {
			return configError(err)
		}
		return output(cmd, map[string]string{"directory": directory, "workspace_id": workspaceID, "application_id": applicationID})
	}}
}

func (a *clientApp) projectTarget(p profile, directory string) (string, string, error) {
	workspace, application := a.v.GetString("workspace"), a.v.GetString("application")
	if workspace != "" || application != "" {
		if workspace == "" || application == "" {
			return "", "", fail("INVALID_ARGUMENT", 2, false, "请同时指定 --workspace 和 --application")
		}
		return workspace, application, nil
	}
	if bound, ok := p.Projects[directory]; ok {
		p.Workspace, p.Application = bound.Workspace, bound.Application
	}
	return requireTarget(p)
}
