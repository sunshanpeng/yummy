package clientcli

import (
	"context"
	"strings"

	"github.com/spf13/viper"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
)

// APIClient 是按 CLI 凭证规则构造的已认证 Connect 客户端，供本地 stdio MCP
// 桥接等程序化调用方使用。Token 为用户个人凭证（PAT 或 OAuth 令牌），仅在
// 进程内持有，不得写入日志、输出或配置导出。
type APIClient struct {
	// Base 为实例根地址（不含 /api 前缀）。
	Base string
	// Token 为当前 Bearer 凭证；由 ResolveClient 解析，不对外打印。
	Token string
	// DefaultWorkspace 与 DefaultApplication 为 profile 保存的默认目标，可为空。
	DefaultWorkspace   string
	DefaultApplication string

	Workspace   platformv1connect.WorkspaceServiceClient
	Application platformv1connect.ApplicationServiceClient
	Runtime     platformv1connect.RuntimeConfigServiceClient
	Source      platformv1connect.SourceServiceClient
	Build       platformv1connect.BuildServiceClient
	Deployment  platformv1connect.DeploymentServiceClient
}

// ClientOptions 汇集程序化客户端的解析输入，语义与命令行标志一致；缺省时
// 依次回退到环境变量（YUMMY_PROFILE 等）与默认配置目录。
type ClientOptions struct {
	Profile        string
	ConfigDir      string
	Server         string
	Workspace      string
	Application    string
	AllowLocalHTTP bool
}

// ResolveClient 按与 CLI 完全相同的规则解析实例地址与凭证并返回已认证客户端，
// 包括 YUMMY_TOKEN 环境变量、profile 凭证存储与到期前的刷新令牌。未登录返回
// UNAUTHENTICATED 的 *Error；配置无效返回 CONFIG_ERROR 的 *Error。
func ResolveClient(ctx context.Context, o ClientOptions) (*APIClient, error) {
	dir := o.ConfigDir
	if dir == "" {
		var err error
		if dir, err = defaultConfigDir(); err != nil {
			return nil, configError(err)
		}
	}
	v := viper.New()
	v.SetEnvPrefix("YUMMY")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	if o.Profile != "" {
		v.Set("profile", o.Profile)
	} else if v.GetString("profile") == "" {
		// 与命令行 flag 默认值保持一致。
		v.Set("profile", "default")
	}
	v.Set("config-dir", dir)
	if o.Server != "" {
		v.Set("server", o.Server)
	}
	if o.Workspace != "" {
		v.Set("workspace", o.Workspace)
	}
	if o.Application != "" {
		v.Set("application", o.Application)
	}
	if o.AllowLocalHTTP {
		v.Set("allow-local-http", true)
	}
	a := &clientApp{v: v, ctx: ctx, store: configStore{dir: dir}}
	// 完全未配置实例地址视为未登录（MCP 桥接可降级为只读预览），与显式的
	// 无效配置（CONFIG_ERROR）区分开。
	if _, cfg, err := a.config(); err != nil {
		return nil, err
	} else if cfg.Server == "" {
		return nil, fail("UNAUTHENTICATED", 3, false, "未配置实例地址；请先用 platform client config set server 配置并登录")
	}
	c, p, err := a.client()
	if err != nil {
		return nil, err
	}
	return &APIClient{
		Base:               c.base,
		Token:              c.token,
		DefaultWorkspace:   p.Workspace,
		DefaultApplication: p.Application,
		Workspace:          c.workspace,
		Application:        c.application,
		Runtime:            c.runtime,
		Source:             c.source,
		Build:              c.build,
		Deployment:         c.deployment,
	}, nil
}
