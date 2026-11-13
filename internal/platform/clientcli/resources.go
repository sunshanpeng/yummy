package clientcli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
)

func newKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func mutationKey(cmd *cobra.Command, provided string) (string, error) {
	if provided != "" {
		return provided, nil
	}
	key, err := newKey()
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "幂等键 %s；若请求结果不确定，请使用相同 --idempotency-key 和相同输入重试。\n", key)
	return key, nil
}
func (a *clientApp) workspaceCommand() *cobra.Command {
	root := &cobra.Command{Use: "workspace", Short: "工作空间操作"}
	var limit int32
	var cursor string
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := a.client()
		if err != nil {
			return err
		}
		r, err := c.workspace.ListWorkspaces(cmd.Context(), auth(c.token, &platformv1.ListWorkspacesRequest{Limit: limit, Cursor: cursor}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	list.Flags().Int32Var(&limit, "limit", 50, "分页大小")
	list.Flags().StringVar(&cursor, "cursor", "", "分页游标")
	var slug, description string
	create := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := a.client()
		if err != nil {
			return err
		}
		r, err := c.workspace.CreateWorkspace(cmd.Context(), auth(c.token, &platformv1.CreateWorkspaceRequest{Name: args[0], Slug: slug, Description: description}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	create.Flags().StringVar(&slug, "slug", "", "工作空间标识")
	create.Flags().StringVar(&description, "description", "", "描述")
	use := &cobra.Command{Use: "use WORKSPACE_ID", Short: "验证并保存当前 profile 的默认工作空间", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		id := strings.TrimSpace(args[0])
		if id == "" {
			return configError(errors.New("工作空间 ID 不能为空"))
		}
		_, err = c.workspace.GetWorkspace(cmd.Context(), auth(c.token, &platformv1.GetWorkspaceRequest{WorkspaceId: id}))
		if err != nil {
			return rpcError(err)
		}
		name := a.v.GetString("profile")
		err = a.store.update(name, func(stored *profile) error {
			if stored.Server != "" && stored.Server != p.Server {
				return errors.New("实例地址与 profile 不匹配，请先 config set")
			}
			if stored.Server == "" {
				stored.Server = p.Server
			}
			stored.Workspace = id
			stored.Application = ""
			return nil
		})
		if err != nil {
			return configError(err)
		}
		return output(cmd, map[string]string{"workspace_id": id})
	}}
	root.AddCommand(list, create, use)
	return root
}
func (a *clientApp) applicationCommand() *cobra.Command {
	root := &cobra.Command{Use: "app", Aliases: []string{"apps", "application"}, Short: "应用操作"}
	var limit int32
	var cursor string
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		w, err := requireWorkspace(p)
		if err != nil {
			return err
		}
		r, err := c.application.ListApplications(cmd.Context(), auth(c.token, &platformv1.ListApplicationsRequest{WorkspaceId: w, Limit: limit, Cursor: cursor}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	list.Flags().Int32Var(&limit, "limit", 50, "分页大小")
	list.Flags().StringVar(&cursor, "cursor", "", "分页游标")
	get := &cobra.Command{Use: "get [APPLICATION_ID]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			p.Application = args[0]
		}
		w, id, err := requireTarget(p)
		if err != nil {
			return err
		}
		r, err := c.application.GetApplication(cmd.Context(), auth(c.token, &platformv1.GetApplicationRequest{WorkspaceId: w, ApplicationId: id}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	var key string
	create := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		w, err := requireWorkspace(p)
		if err != nil {
			return err
		}
		k, err := mutationKey(cmd, key)
		if err != nil {
			return err
		}
		r, err := c.application.CreateApplication(cmd.Context(), auth(c.token, &platformv1.CreateApplicationRequest{WorkspaceId: w, Name: args[0], IdempotencyKey: k}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	create.Flags().StringVar(&key, "idempotency-key", "", "重试使用同一幂等键与输入")
	use := &cobra.Command{Use: "use APPLICATION_ID", Short: "验证并保存默认应用", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		p.Application = args[0]
		w, id, err := requireTarget(p)
		if err != nil {
			return err
		}
		_, err = c.application.GetApplication(cmd.Context(), auth(c.token, &platformv1.GetApplicationRequest{WorkspaceId: w, ApplicationId: id}))
		if err != nil {
			return rpcError(err)
		}
		name := a.v.GetString("profile")
		err = a.store.update(name, func(stored *profile) error {
			if stored.Server != "" && stored.Server != p.Server {
				return errors.New("实例地址与 profile 不匹配，请先 config set")
			}
			stored.Server = p.Server
			stored.Workspace = w
			stored.Application = id
			return nil
		})
		if err != nil {
			return configError(err)
		}
		return output(cmd, map[string]string{"workspace_id": w, "application_id": id})
	}}
	root.AddCommand(list, get, create, use)
	return root
}
