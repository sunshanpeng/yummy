package clientcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Error 是不包含凭据的机器可读命令错误；由根命令统一输出到 stderr。
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Exit      int    `json:"exit_code"`
}

func (e *Error) Error() string { data, _ := json.Marshal(e); return string(data) }
func fail(code string, exit int, retry bool, message string) error {
	return &Error{Code: code, Message: message, Retryable: retry, Exit: exit}
}
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Exit
	}
	return 1
}
func rpcError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fail("CANCELED", 130, false, "客户端请求已取消；服务端操作不因此取消")
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument:
		return fail("INVALID_ARGUMENT", 2, false, "请求参数无效")
	case connect.CodeUnauthenticated:
		return fail("UNAUTHENTICATED", 3, false, "认证失败，请检查实例和凭据")
	case connect.CodePermissionDenied:
		return fail("PERMISSION_DENIED", 4, false, "当前凭据无权执行此操作")
	case connect.CodeNotFound:
		return fail("NOT_FOUND", 5, false, "资源不存在或不可访问")
	case connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
		return fail("CONFLICT", 6, false, "状态或版本冲突，请重新读取后确认")
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return fail("UNAVAILABLE", 7, true, "服务不可用或请求超时；写操作请先确认结果再重试")
	default:
		return fail("REQUEST_FAILED", 1, false, "请求失败；请检查控制面")
	}
}

func output(cmd *cobra.Command, value any) error {
	if msg, ok := value.(proto.Message); ok {
		data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

type clientApp struct {
	v     *viper.Viper
	store configStore
	ctx   context.Context
}
type apiClient struct {
	base        string
	token       string
	http        *http.Client
	workspace   platformv1connect.WorkspaceServiceClient
	application platformv1connect.ApplicationServiceClient
	runtime     platformv1connect.RuntimeConfigServiceClient
	source      platformv1connect.SourceServiceClient
	build       platformv1connect.BuildServiceClient
	deployment  platformv1connect.DeploymentServiceClient
	inventory   platformv1connect.InventoryServiceClient
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (a *clientApp) config() (string, profile, error) {
	name := a.v.GetString("profile")
	if !validProfile(name) {
		return "", profile{}, configError(errors.New("profile 名称无效"))
	}
	p, err := a.store.read(name)
	if err != nil {
		return "", profile{}, configError(err)
	}
	if raw := a.v.GetString("server"); raw != "" {
		server, err := validateServer(raw, a.v.GetBool("allow-local-http"))
		if err != nil {
			return "", profile{}, configError(err)
		}
		if p.Server != "" && p.Server != server {
			p.Token = ""
			p.RefreshToken = ""
			p.TokenExpiry = time.Time{}
			p.Workspace = ""
			p.Application = ""
			p.Projects = nil
		}
		p.Server = server
	}
	if p.Server != "" {
		p.Server, err = validateServer(p.Server, a.v.GetBool("allow-local-http"))
		if err != nil {
			return "", profile{}, configError(err)
		}
	}
	if v := a.v.GetString("workspace"); v != "" {
		p.Workspace = v
	}
	if v := a.v.GetString("application"); v != "" {
		p.Application = v
	}
	if token := os.Getenv("YUMMY_TOKEN"); token != "" {
		if strings.ContainsAny(token, " \t\r\n") {
			return "", profile{}, configError(errors.New("环境令牌格式无效"))
		}
		p.Token = token
	}
	return name, p, nil
}
func (a *clientApp) client() (*apiClient, profile, error) {
	name, p, err := a.config()
	if err != nil {
		return nil, p, err
	}
	if p.Server == "" {
		return nil, p, configError(errors.New("请使用 --server 或设置 profile 实例地址"))
	}
	if os.Getenv("YUMMY_TOKEN") == "" && !p.TokenExpiry.IsZero() && time.Now().Add(30*time.Second).After(p.TokenExpiry) {
		refreshed, refreshErr := a.refreshToken(a.ctx, name, p.Server)
		if refreshErr != nil {
			return nil, p, refreshErr
		}
		p.Token = refreshed.Token
	}
	if p.Token == "" {
		return nil, p, fail("UNAUTHENTICATED", 3, false, "请使用 auth login、auth token --token-stdin 或 YUMMY_TOKEN")
	}
	httpClient := newHTTPClient()
	base := p.Server + "/api"
	c := &apiClient{base: p.Server, token: p.Token, http: httpClient}
	c.workspace = platformv1connect.NewWorkspaceServiceClient(httpClient, base)
	c.application = platformv1connect.NewApplicationServiceClient(httpClient, base)
	c.runtime = platformv1connect.NewRuntimeConfigServiceClient(httpClient, base)
	c.source = platformv1connect.NewSourceServiceClient(httpClient, base)
	c.build = platformv1connect.NewBuildServiceClient(httpClient, base)
	c.deployment = platformv1connect.NewDeploymentServiceClient(httpClient, base)
	c.inventory = platformv1connect.NewInventoryServiceClient(httpClient, base)
	return c, p, nil
}

func (a *clientApp) clientForServer(base string) (*apiClient, error) {
	c, _, err := a.client()
	if err != nil {
		return nil, err
	}
	if c.base != base {
		return nil, fail("CONFLICT", 6, false, "本地配置的实例地址已变更，请核对原实例的操作结果")
	}
	return c, nil
}
func auth[T any](token string, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}
func requireWorkspace(p profile) (string, error) {
	if p.Workspace == "" {
		return "", fail("INVALID_ARGUMENT", 2, false, "必须明确 --workspace 或设置默认工作空间")
	}
	return p.Workspace, nil
}
func requireTarget(p profile) (string, string, error) {
	w, err := requireWorkspace(p)
	if err != nil {
		return "", "", err
	}
	if p.Application == "" {
		return "", "", fail("INVALID_ARGUMENT", 2, false, "必须明确 --application 或设置默认应用")
	}
	return w, p.Application, nil
}

// Command 创建用户侧 CLI 的根命令。
func Command() *cobra.Command {
	v := viper.New()
	v.SetEnvPrefix("YUMMY")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	defaultDir, _ := defaultConfigDir()
	a := &clientApp{v: v}
	root := &cobra.Command{Use: "yummy", Short: "通过正式 API 操作 Yummy", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("profile", "default", "本地配置名称")
	root.PersistentFlags().String("config-dir", defaultDir, "安全配置目录")
	root.PersistentFlags().String("server", "", "实例根 URL")
	root.PersistentFlags().String("workspace", "", "工作空间 ID")
	root.PersistentFlags().String("application", "", "应用 ID")
	root.PersistentFlags().String("output", "json", "输出格式")
	root.PersistentFlags().Bool("allow-local-http", false, "仅开发时允许回环地址 HTTP")
	for _, key := range []string{"profile", "config-dir", "server", "workspace", "application", "allow-local-http"} {
		_ = v.BindPFlag(key, root.PersistentFlags().Lookup(key))
	}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		format, err := cmd.Flags().GetString("output")
		if err != nil || format != "json" {
			return fail("INVALID_ARGUMENT", 2, false, "输出格式使用 json")
		}
		a.ctx = cmd.Context()
		a.store.dir = v.GetString("config-dir")
		if a.store.dir == "" {
			return configError(errors.New("缺少配置目录"))
		}
		return nil
	}
	root.AddCommand(a.versionCommand(), a.authCommand(), a.configCommand(), a.workspaceCommand(), a.applicationCommand(), a.useProjectCommand(), a.runtimeConfigCommand(), a.sourceCommand(), a.buildCommand(), a.deploymentCommand(), a.statusCommand(), a.logsCommand(), a.metricsCommand())
	return root
}
func (a *clientApp) versionCommand() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "输出客户端版本", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		version := "devel"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		return output(cmd, map[string]string{"version": version})
	}}
}
func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("输入过大")
	}
	return data, nil
}
