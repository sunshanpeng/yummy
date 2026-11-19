package clientcli

import (
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func (a *clientApp) configCommand() *cobra.Command {
	root := &cobra.Command{Use: "config", Short: "管理本机实例配置"}
	show := &cobra.Command{Use: "show", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name, p, err := a.config()
		if err != nil {
			return err
		}
		return output(cmd, publicView(name, p))
	}}
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		values, err := a.store.list()
		if err != nil {
			return configError(err)
		}
		return output(cmd, values)
	}}
	set := &cobra.Command{Use: "set", Short: "设置实例地址（更换实例会清除旧凭据和默认资源）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name := a.v.GetString("profile")
		raw := a.v.GetString("server")
		if raw == "" {
			return configError(errors.New("必须显式提供 --server"))
		}
		server, err := validateServer(raw, a.v.GetBool("allow-local-http"))
		if err != nil {
			return configError(err)
		}
		err = a.store.update(name, func(p *profile) error {
			if p.Server != server {
				*p = profile{Server: server}
			}
			return nil
		})
		if err != nil {
			return configError(err)
		}
		p, err := a.store.read(name)
		if err != nil {
			return configError(err)
		}
		return output(cmd, publicView(name, p))
	}}
	root.AddCommand(show, list, set)
	return root
}

func (a *clientApp) authCommand() *cobra.Command {
	root := &cobra.Command{Use: "auth", Short: "管理当前实例凭据"}
	var fromStdin bool
	token := &cobra.Command{Use: "token", Short: "从标准输入安全保存 PAT", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !fromStdin {
			return fail("INVALID_ARGUMENT", 2, false, "必须使用 --token-stdin；不接受明文命令行参数")
		}
		name, p, err := a.config()
		if err != nil {
			return err
		}
		if p.Server == "" {
			return configError(errors.New("必须先指定实例 --server"))
		}
		value, err := readToken(cmd.InOrStdin())
		if err != nil {
			return configError(err)
		}
		err = a.store.update(name, func(stored *profile) error {
			if stored.Server != p.Server {
				*stored = profile{Server: p.Server}
			}
			stored.Token = value
			stored.RefreshToken = ""
			stored.TokenExpiry = time.Time{}
			return nil
		})
		if err != nil {
			return configError(err)
		}
		return output(cmd, map[string]any{"profile": name, "server": p.Server, "credential": "pat", "configured": true})
	}}
	token.Flags().BoolVar(&fromStdin, "token-stdin", false, "从标准输入读取 PAT")
	logout := &cobra.Command{Use: "logout", Short: "删除本地保存的凭据（不会撤销服务端令牌）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name := a.v.GetString("profile")
		err := a.store.update(name, func(p *profile) error { p.Token = ""; p.RefreshToken = ""; p.TokenExpiry = time.Time{}; return nil })
		if err != nil {
			return configError(err)
		}
		return output(cmd, map[string]any{"profile": name, "local_credential_removed": true})
	}}
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name, p, err := a.config()
		if err != nil {
			return err
		}
		source := "none"
		if p.Token != "" {
			source = "profile"
		}
		if strings.TrimSpace(a.v.GetString("token")) != "" {
			source = "environment"
		}
		return output(cmd, map[string]any{"profile": name, "server": p.Server, "configured": p.Token != "", "source": source})
	}}
	root.AddCommand(token, logout, status, a.oauthLoginCommand())
	return root
}
