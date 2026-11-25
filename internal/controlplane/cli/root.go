// Package cli 装配控制面服务和实例管理命令。
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/sunshanpeng/yummy/internal/controlplane/server"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/identity"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/lib/observability"
	"github.com/sunshanpeng/yummy/internal/lib/startupconfig"
	"golang.org/x/term"
)

func Execute(ctx context.Context, args []string, assets fs.FS) error {
	settings, err := startupconfig.Load(args)
	if err != nil {
		return err
	}
	command := NewRoot(assets, settings)
	command.SetArgs(args)
	return command.ExecuteContext(ctx)
}

func NewRoot(assets fs.FS, settings map[string]any) *cobra.Command {
	root, config := startupconfig.NewCommand("yummy-server", "Yummy 控制面", settings)
	config.SetDefault("database-url", "postgres://demo:demo@localhost:5432/demo?sslmode=disable")
	config.SetDefault("redis-addr", "localhost:6379")
	config.SetDefault("public-addr", "127.0.0.1:8080")
	config.SetDefault("admin-addr", "127.0.0.1:8082")
	config.SetDefault("worker-addr", "127.0.0.1:8443")
	root.PersistentFlags().String("database-url", config.GetString("database-url"), "PostgreSQL 连接地址")
	_ = config.BindPFlag("database-url", root.PersistentFlags().Lookup("database-url"))
	migrate := &cobra.Command{Use: "migrate", Short: "执行带锁的版本化数据库迁移", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		db, err := store.Open(cmd.Context(), config.GetString("database-url"))
		if err != nil {
			return err
		}
		defer store.Close(db)
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "数据库迁移完成")
		return err
	}}
	admin := &cobra.Command{Use: "admin", Short: "本机实例管理"}
	var username string
	var passwordStdin bool
	initAdmin := &cobra.Command{Use: "init", Short: "在本机创建唯一首位全局管理员", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		password, err := readPassword(cmd, passwordStdin)
		if err != nil {
			return err
		}
		db, err := store.Open(cmd.Context(), config.GetString("database-url"))
		if err != nil {
			return err
		}
		defer store.Close(db)
		access, err := authorization.NewAccess(db)
		if err != nil {
			return err
		}
		service, err := identity.NewService(db, access)
		if err != nil {
			return err
		}
		user, err := service.InitializeAdmin(cmd.Context(), username, password)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(user)
	}}
	initAdmin.Flags().StringVar(&username, "username", "", "管理员用户名")
	initAdmin.Flags().BoolVar(&passwordStdin, "password-stdin", false, "从标准输入读取密码，避免命令历史或进程参数泄露")
	_ = initAdmin.MarkFlagRequired("username")
	admin.AddCommand(initAdmin)
	serve := &cobra.Command{Use: "serve", Short: "启动正式产品控制面", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		obs, err := observability.Start(cmd.Context(), observability.Options{ServiceName: "yummy-control-plane", OTLPEndpoint: config.GetString("otel-endpoint"), MetricsPull: config.GetBool("metrics-pull"), LogLevel: config.GetString("log-level")})
		if err != nil {
			return err
		}
		// 退出时冲刷遥测；带超时的 ctx 必须在退出那一刻创建，进程长期运行后
		// 提前创建的 ctx 早已过期，Shutdown 会全部失败。
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = obs.Shutdown(shutdownCtx)
		}()
		product, err := server.Open(cmd.Context(), server.Config{DatabaseURL: config.GetString("database-url"), RedisAddress: config.GetString("redis-addr"), PublicBaseURL: config.GetString("public-base-url"), SessionKey: config.GetString("session-key"), CSRFKey: config.GetString("csrf-key"), MasterKey: config.GetString("master-key"), OAuthKey: config.GetString("oauth-key"), OIDCIssuer: config.GetString("oidc-issuer"), OIDCClientID: config.GetString("oidc-client-id"), OIDCClientSecret: config.GetString("oidc-client-secret"), SourceRoot: config.GetString("source-root"), WorkerCACertificatePath: config.GetString("worker-ca-cert"), WorkerCAPrivateKeyPath: config.GetString("worker-ca-key"), AllowLocalHTTP: config.GetBool("allow-local-http"), BehindTLSProxy: config.GetBool("behind-tls-proxy"), Obs: obs}, assets)
		if err != nil {
			return err
		}
		defer product.Close()
		var workerServer *http.Server
		if product.WorkerHandler != nil {
			if config.GetString("worker-cert") == "" || config.GetString("worker-key") == "" {
				return errors.New("启用 worker CA 时必须配置 worker HTTPS 服务端证书和私钥")
			}
			tlsConfig, err := product.WorkerTLSConfig()
			if err != nil {
				return err
			}
			workerServer = &http.Server{Addr: config.GetString("worker-addr"), Handler: obs.HTTPHandler("yummy-worker-gateway", product.WorkerHandler), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		} else if config.GetString("worker-cert") != "" || config.GetString("worker-key") != "" {
			return errors.New("配置 worker HTTPS 证书时必须同时配置 worker CA")
		}
		// 业务超时预算在路由层（/transfer 传输自带 10 分钟截止，其余 30s），
		// 服务器级只保留慢速连接的头部与空闲防护。
		httpServer := &http.Server{Addr: config.GetString("public-addr"), Handler: obs.HTTPHandler("yummy-control-plane", product.Handler), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		adminServer := &http.Server{Addr: config.GetString("admin-addr"), Handler: product.AdminHandler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
		errorsChannel := make(chan error, 3)
		go func() { errorsChannel <- httpServer.ListenAndServe() }()
		go func() { errorsChannel <- adminServer.ListenAndServe() }()
		if workerServer != nil {
			go func() {
				errorsChannel <- workerServer.ListenAndServeTLS(config.GetString("worker-cert"), config.GetString("worker-key"))
			}()
		}
		var runErr error
		select {
		case err := <-errorsChannel:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = err
			}
		case <-cmd.Context().Done():
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if product.WorkerGateway != nil {
			product.WorkerGateway.Close()
		}
		var workerErr error
		if workerServer != nil {
			workerErr = workerServer.Shutdown(shutdown)
		}
		return errors.Join(runErr, workerErr, httpServer.Shutdown(shutdown), adminServer.Shutdown(shutdown))
	}}
	for _, flag := range []struct{ name, description string }{
		{"redis-addr", "Redis 地址或 redis(s) URL"}, {"public-addr", "HTTP 监听地址"}, {"public-base-url", "明确的公开站点根 URL，不从请求头推断"},
		{"session-key", "Base64 编码32字节会话密钥（推荐使用环境变量）"}, {"csrf-key", "独立的Base64编码32字节CSRF密钥（推荐使用环境变量）"},
		{"master-key", "独立的Base64编码32字节秘密加密主密钥（推荐使用环境变量）"},
		{"oauth-key", "独立的Base64编码32字节OAuth密钥，留空时关闭OAuth及远程MCP"},
		{"oidc-issuer", "团队登录的 OIDC Issuer URL"}, {"oidc-client-id", "外部 OIDC 客户端 ID"},
		{"oidc-client-secret", "外部 OIDC 客户端密钥（推荐配置文件或环境变量）"},
		{"source-root", "显式持久化源码目录，必须预先创建"},
		{"worker-ca-cert", "独立worker签发CA证书路径（留空禁用登记）"},
		{"worker-ca-key", "独立worker签发CA私钥路径，仅本进程可读"},
		{"worker-addr", "worker 专用 HTTPS 监听地址（默认127.0.0.1:8443）"},
		{"worker-cert", "worker 监听器服务端 TLS 证书路径"},
		{"worker-key", "worker 监听器服务端 TLS 私钥路径"},
		{"admin-addr", "管理监听地址：/healthz 与 /metrics，默认只绑回环，公网不可达（默认127.0.0.1:8082）"},
	} {
		serve.Flags().String(flag.name, config.GetString(flag.name), flag.description)
		_ = config.BindPFlag(flag.name, serve.Flags().Lookup(flag.name))
	}
	serve.Flags().Bool("allow-local-http", false, "仅开发时允许回环地址HTTP，生产必须HTTPS")
	_ = config.BindPFlag("allow-local-http", serve.Flags().Lookup("allow-local-http"))
	serve.Flags().Bool("metrics-pull", config.GetBool("metrics-pull"), "在管理端口暴露 /metrics 供 Prometheus 抓取（pull 通道）")
	_ = config.BindPFlag("metrics-pull", serve.Flags().Lookup("metrics-pull"))
	serve.Flags().Bool("behind-tls-proxy", false, "公网入口由可信反向代理终止HTTPS（docs/operations.md §4），应用收到代理转发请求")
	_ = config.BindPFlag("behind-tls-proxy", serve.Flags().Lookup("behind-tls-proxy"))
	root.AddCommand(migrate, admin, serve)
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	return root
}

func readPassword(cmd *cobra.Command, fromStdin bool) (string, error) {
	if fromStdin {
		reader := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 130))
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r"), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("非交互终端请使用 --password-stdin，不支持明文密码参数")
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), "管理员密码（不会回显）：")
	value, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), "再次输入密码：")
	confirmation, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	if string(value) != string(confirmation) {
		return "", errors.New("两次密码不一致")
	}
	return string(value), nil
}
