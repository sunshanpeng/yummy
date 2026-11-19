// Package mcp 把受授权的应用业务服务适配为 MCP 工具，不启动独立服务。
package mcp

import (
	"context"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"github.com/sunshanpeng/yummy/internal/domains/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/domains/source"
)

// Authenticate 必须验证 OAuth token 的签名、有效期、撤销及精确 resource。
// 远程入口额外拒绝 PAT、浏览器会话和无应用范围的 Principal。
type Authenticate func(context.Context, string, string) (authorization.Principal, error)

type Config struct {
	// PublicBaseURL 为公开 OAuth issuer 根地址，不使用 Host 或转发头拼接。
	PublicBaseURL string
	Authenticate  Authenticate
	// 仅供显式指定本机 HTTP 的开发环境；生产地址必须使用 HTTPS。
	AllowInsecureLoopback bool
	// 请求和工具都有独立超时；超时不隐式取消已经持久化受理的任务。
	RequestTimeout time.Duration
}

// Applications 的 ResolveWorkspace 必须从数据库查找归属并复核应用读取权限。
type Applications interface {
	ResolveWorkspace(context.Context, authorization.Principal, string) (string, error)
	Get(context.Context, authorization.Principal, string, string, string) (application.View, error)
}

type RuntimeConfigs interface {
	Get(context.Context, authorization.Principal, string, string) (runtimeconfig.View, error)
	Save(context.Context, authorization.Principal, string, string, runtimeconfig.Input) (runtimeconfig.View, error)
}

type Sources interface {
	GetSnapshot(context.Context, authorization.Principal, string, string, string) (source.Snapshot, error)
}

type Builds interface {
	GetConfig(context.Context, authorization.Principal, string, string) (build.ConfigView, error)
	CreatePreview(context.Context, authorization.Principal, string, string, build.CreateInput) (build.Preview, error)
	CreateBuild(context.Context, authorization.Principal, string, string, build.CreateInput) (build.View, error)
	Get(context.Context, authorization.Principal, string, string, string) (build.View, error)
	List(context.Context, authorization.Principal, string, string, int, string) (build.Page, error)
	ReadLogs(context.Context, authorization.Principal, string, string, string, int64, int) (build.Logs, error)
	Cancel(context.Context, authorization.Principal, string, string, string) (build.View, error)
	Retry(context.Context, authorization.Principal, string, string, string, build.CreateInput) (build.View, error)
}

type Deployments interface {
	GetStatus(context.Context, authorization.Principal, string, string) (deployment.Status, error)
	CreateDeployment(context.Context, authorization.Principal, string, string, deployment.CreateInput) (deployment.Result, error)
	Get(context.Context, authorization.Principal, string, string, string) (deployment.Result, error)
	List(context.Context, authorization.Principal, string, string, int, string) (deployment.Page, error)
	Rollback(context.Context, authorization.Principal, string, string, deployment.RollbackInput) (deployment.Result, error)
	Start(context.Context, authorization.Principal, string, string, deployment.ChangeInput) (deployment.Result, error)
	Stop(context.Context, authorization.Principal, string, string, deployment.ChangeInput) (deployment.Result, error)
	Restart(context.Context, authorization.Principal, string, string, deployment.ChangeInput) (deployment.Result, error)
}

// Services 为已装配的业务服务；业务服务必须在每次操作内重查授权。
// 未配置的功能不会暴露为可成功调用的假能力。
type Services struct {
	Applications   Applications
	RuntimeConfigs RuntimeConfigs
	Sources        Sources
	Builds         Builds
	Deployments    Deployments
}
