// Package cli 提供与控制面共用产品内核的按需管理命令，不启动额外常驻服务。
package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	platformmcp "github.com/sunshanpeng/yummy/internal/bff/mcp"
	"github.com/sunshanpeng/yummy/internal/controlplane/server"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/identity"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/infra/workerexec"
	"github.com/sunshanpeng/yummy/internal/lib/observability"
	"github.com/sunshanpeng/yummy/internal/transport/clientcli"
	"github.com/sunshanpeng/yummy/internal/transport/workergateway"
	"go.uber.org/zap"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// workerVersion 是worker进程上报的版本串，只用于欢迎消息回显。
const workerVersion = "yummy-worker/1"

func Execute(ctx context.Context, args []string, assets fs.FS) error {
	settings, err := loadStartupConfig(args)
	if err != nil {
		return err
	}
	command := NewRoot(assets, settings)
	command.SetArgs(args)
	return command.ExecuteContext(ctx)
}

func NewRoot(assets fs.FS, settings map[string]any) *cobra.Command {
	config := viper.New()
	config.SetEnvKeyReplacer(envKeyReplacer())
	config.AutomaticEnv()
	config.SetDefault("database-url", "postgres://demo:demo@localhost:5432/demo?sslmode=disable")
	config.SetDefault("redis-addr", "localhost:6379")
	config.SetDefault("public-addr", "127.0.0.1:8080")
	config.SetDefault("transfer-addr", "127.0.0.1:8081")
	config.SetDefault("worker-addr", "127.0.0.1:8443")
	root := &cobra.Command{Use: "platform", Short: "Yummy 应用平台", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.PersistentFlags().StringP("config", "c", "", "启动配置文件路径（yaml/toml/json），未指定时依次查找 ./config.yaml、/etc/yummy/config.yaml")
	// 配置文件在 flag 解析之后归并（PersistentPreRunE）：显式 flag > 配置文件 >
	// 环境变量 > 内置默认。文件里的键与 flag 同名，未知键也照常写入供后续读取。
	if len(settings) > 0 {
		root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
			applyConfigSettings(config, settings, cmd.Flags())
			return nil
		}
	}
	root.PersistentFlags().String("database-url", config.GetString("database-url"), "PostgreSQL 连接地址")
	_ = config.BindPFlag("database-url", root.PersistentFlags().Lookup("database-url"))
	// OTLP 端点兼容标准环境变量：--otel-endpoint 未显式提供时回退 OTEL_EXPORTER_OTLP_ENDPOINT，
	// 与 build-runner 及社区生态保持一致。
	otelEndpoint := config.GetString("otel-endpoint")
	if otelEndpoint == "" {
		otelEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	root.PersistentFlags().String("otel-endpoint", otelEndpoint, "OTLP HTTP 导出端点（trace/metric/log push），留空关闭；未提供时回退 OTEL_EXPORTER_OTLP_ENDPOINT")
	_ = config.BindPFlag("otel-endpoint", root.PersistentFlags().Lookup("otel-endpoint"))
	root.PersistentFlags().String("log-level", config.GetString("log-level"), "日志级别：debug / info / warn / error")
	_ = config.BindPFlag("log-level", root.PersistentFlags().Lookup("log-level"))
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
		product, err := server.Open(cmd.Context(), server.Config{DatabaseURL: config.GetString("database-url"), RedisAddress: config.GetString("redis-addr"), PublicBaseURL: config.GetString("public-base-url"), SessionKey: config.GetString("session-key"), CSRFKey: config.GetString("csrf-key"), MasterKey: config.GetString("master-key"), OAuthKey: config.GetString("oauth-key"), SourceRoot: config.GetString("source-root"), WorkerCACertificatePath: config.GetString("worker-ca-cert"), WorkerCAPrivateKeyPath: config.GetString("worker-ca-key"), AllowLocalHTTP: config.GetBool("allow-local-http"), BehindTLSProxy: config.GetBool("behind-tls-proxy"), Obs: obs}, assets)
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
		httpServer := &http.Server{Addr: config.GetString("public-addr"), Handler: obs.HTTPHandler("yummy-control-plane", product.Handler), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		// 文件传输与普通API共用控制面进程，但独立监听避免30秒整体超时截断上传。
		transferServer := &http.Server{Addr: config.GetString("transfer-addr"), Handler: obs.HTTPHandler("yummy-transfer", product.TransferHandler), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		errorsChannel := make(chan error, 3)
		go func() { errorsChannel <- httpServer.ListenAndServe() }()
		go func() { errorsChannel <- transferServer.ListenAndServe() }()
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
		return errors.Join(runErr, workerErr, httpServer.Shutdown(shutdown), transferServer.Shutdown(shutdown))
	}}
	for _, flag := range []struct{ name, description string }{
		{"redis-addr", "Redis 地址或 redis(s) URL"}, {"public-addr", "HTTP 监听地址"}, {"public-base-url", "明确的公开站点根 URL，不从请求头推断"},
		{"session-key", "Base64 编码32字节会话密钥（推荐使用环境变量）"}, {"csrf-key", "独立的Base64编码32字节CSRF密钥（推荐使用环境变量）"},
		{"master-key", "独立的Base64编码32字节秘密加密主密钥（推荐使用环境变量）"},
		{"oauth-key", "独立的Base64编码32字节OAuth密钥，留空时关闭OAuth及远程MCP"},
		{"source-root", "显式持久化源码目录，必须预先创建"},
		{"worker-ca-cert", "独立worker签发CA证书路径（留空禁用登记）"},
		{"worker-ca-key", "独立worker签发CA私钥路径，仅本进程可读"},
		{"worker-addr", "worker 专用 HTTPS 监听地址（默认127.0.0.1:8443）"},
		{"worker-cert", "worker 监听器服务端 TLS 证书路径"},
		{"worker-key", "worker 监听器服务端 TLS 私钥路径"},
		{"transfer-addr", "同控制面进程的有界文件传输监听地址（默认127.0.0.1:8081）"},
	} {
		serve.Flags().String(flag.name, config.GetString(flag.name), flag.description)
		_ = config.BindPFlag(flag.name, serve.Flags().Lookup(flag.name))
	}
	serve.Flags().Bool("allow-local-http", false, "仅开发时允许回环地址HTTP，生产必须HTTPS")
	_ = config.BindPFlag("allow-local-http", serve.Flags().Lookup("allow-local-http"))
	serve.Flags().Bool("metrics-pull", config.GetBool("metrics-pull"), "在公共端口暴露 /metrics 供 Prometheus 抓取（pull 通道）")
	_ = config.BindPFlag("metrics-pull", serve.Flags().Lookup("metrics-pull"))
	serve.Flags().Bool("behind-tls-proxy", false, "公网入口由可信反向代理终止HTTPS（docs/operations.md §4），应用收到代理转发请求")
	_ = config.BindPFlag("behind-tls-proxy", serve.Flags().Lookup("behind-tls-proxy"))
	var allowRoots []string
	var stdio bool
	localMCP := &cobra.Command{Use: "mcp", Short: "启动按需本地 MCP（源码预览；已登录时桥接业务工具）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !stdio {
			return errors.New("必须显式指定 --stdio；远程 MCP 由控制面提供")
		}
		// 已登录（存在用户凭证）时以用户 PAT 经控制面 API 桥接业务工具；
		// 未登录时保持只读 source_preview 并在 stderr 说明。凭证不打印。
		bridge := platformmcp.BridgeConfig{}
		client, err := clientcli.ResolveClient(cmd.Context(), clientcli.ClientOptions{})
		var clientErr *clientcli.Error
		switch {
		case err == nil:
			bridge.Client = client
		case errors.As(err, &clientErr) && clientErr.Code == "UNAUTHENTICATED":
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "未检测到有效凭证，本次只提供 source_preview；业务工具需要先执行 platform client auth login 或 auth token，然后重新启动。")
		default:
			return err
		}
		server, err := platformmcp.NewBridgeServer(bridge, platformmcp.LocalConfig{AllowRoots: allowRoots})
		if err != nil {
			return err
		}
		return server.Run(cmd.Context(), &sdkmcp.StdioTransport{})
	}}
	localMCP.Flags().BoolVar(&stdio, "stdio", false, "使用标准输入输出运行本地 MCP")
	localMCP.Flags().StringSliceVar(&allowRoots, "allow-root", nil, "显式授权读取的绝对目录，可重复")
	root.AddCommand(migrate, admin, serve, clientcli.Command(), localMCP)
	root.AddCommand(newWorkerCommand(config))
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

// generationClientHolder 解决 Executor 与 WorkerClient 的相互引用：Executor 在
// 每次 Kubernetes 写入前通过它调用 WorkerClient.CheckGeneration（复用同一条已
// 验证 mTLS 连接），连接由 run 命令装配完成后回填。
type generationClientHolder struct {
	mu     sync.Mutex
	client workerexec.GenerationClient
}

func (h *generationClientHolder) CheckGeneration(ctx context.Context, request *platformv1.WorkerGenerationRequest) error {
	h.mu.Lock()
	client := h.client
	h.mu.Unlock()
	if client == nil {
		return errors.New("worker客户端尚未装配完成")
	}
	return client.CheckGeneration(ctx, request)
}

// newWorkerCommand 装配节点侧 worker 进程子命令：register 用一次性注册码登记
// 身份，run 以显式注入的 kube 受限凭据运行产品执行内核。它们是控制面/worker
// 两类常驻进程里 worker 侧的唯一入口，不新增其他常驻服务。
func newWorkerCommand(config *viper.Viper) *cobra.Command {
	worker := &cobra.Command{Use: "worker", Short: "注册并运行本节点 worker 进程"}
	var registerServer, registerToken, registerStateDir, registerCAFile string
	register := &cobra.Command{Use: "register", Short: "用一次性注册码向控制面登记worker身份", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var serverCA []byte
		if registerCAFile != "" {
			data, err := os.ReadFile(registerCAFile)
			if err != nil {
				return err
			}
			serverCA = data
		}
		state, err := workerexec.Register(cmd.Context(), workerexec.RegisterOptions{Server: registerServer, Token: registerToken, StateDir: registerStateDir, ServerCA: serverCA})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
	}}
	register.Flags().StringVar(&registerServer, "server", "", "控制面 worker 专用HTTPS入口地址")
	register.Flags().StringVar(&registerToken, "token", "", "全局管理员签发的一次性注册码")
	register.Flags().StringVar(&registerStateDir, "state-dir", "", "worker状态目录（0700，保存私钥/证书/CA/身份）")
	register.Flags().StringVar(&registerCAFile, "server-ca-file", "", "可选控制面CA证书PEM路径（自签部署）")
	for _, name := range []string{"server", "token", "state-dir"} {
		_ = register.MarkFlagRequired(name)
	}

	var (
		runServer, runStateDir, kubeAPIServer, kubeCAFile, kubeTokenFile string
		runServerCAFile                                                  string
		nodeName, buildNamespace, runtimeNamespace                       string
		storageClass, ingressClass, runnerImage, buildKitImage           string
		qualificationFile, journalDir                                    string
		observeInterval, rotationCheckInterval                           time.Duration
		buildKitAddr, hostBuildCtl, kubeletMetricsEndpoint               string
	)
	run := &cobra.Command{Use: "run", Short: "以已登记身份运行worker执行内核", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		obs, err := observability.Start(cmd.Context(), observability.Options{ServiceName: "yummy-worker", OTLPEndpoint: config.GetString("otel-endpoint"), LogLevel: config.GetString("log-level")})
		if err != nil {
			return err
		}
		// 与 serve 同理：冲刷用的超时 ctx 在退出时创建，不能提前建好放到 defer。
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = obs.Shutdown(shutdownCtx)
		}()
		keyPEM, certPEM, caPEM, identity, err := workerexec.LoadNodeState(runStateDir)
		if err != nil {
			return err
		}
		// TLS 证书经 holder 回调读取：证书轮换成功后回填 holder，已 Clone 的
		// 网关客户端无需重建即可在新握手使用新证书。
		holder, err := workerexec.NewCertificateHolder(certPEM, keyPEM)
		if err != nil {
			return err
		}
		var runServerCA []byte
		if runServerCAFile != "" {
			data, err := os.ReadFile(runServerCAFile)
			if err != nil {
				return err
			}
			runServerCA = data
		}
		if runServer == "" {
			runServer = identity.Server
		}
		restConfig, err := workerexec.NodeRESTConfig(kubeAPIServer, kubeCAFile, kubeTokenFile)
		if err != nil {
			return err
		}
		client, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return err
		}
		// 节点UID/OS/arch只从真实kube API读取，不推断、不缓存。
		facts, err := workerexec.InspectNode(cmd.Context(), client, nodeName)
		if err != nil {
			return err
		}
		capabilities, err := workerexec.LoadInstalledCapabilities(qualificationFile, runnerImage, buildKitImage)
		if err != nil {
			return err
		}
		node := kube.NodeTarget{Name: nodeName, UID: types.UID(facts.UID)}
		adapter, err := kube.NewFromRESTConfig(restConfig, kube.Config{Node: node, BuildNamespace: buildNamespace, RuntimeNamespace: runtimeNamespace, Installed: capabilities, StorageClass: storageClass, IngressClass: ingressClass})
		if err != nil {
			return err
		}
		guard := &generationClientHolder{}
		runOptions := workerexec.Options{Adapter: adapter, Client: client, Node: node, BuildNamespace: buildNamespace, RuntimeNamespace: runtimeNamespace, Generation: workerexec.GenerationGuard(guard), ObserveInterval: observeInterval, Installed: capabilities, BuildKitAddr: buildKitAddr, HostBuildCtl: hostBuildCtl}
		// 指标采集双重失败关闭：安装资格 metrics=true 且显式提供 kubelet metrics 地址。
		if capabilities.Metrics && kubeletMetricsEndpoint != "" {
			caData, err := os.ReadFile(kubeCAFile)
			if err != nil {
				return err
			}
			tokenData, err := os.ReadFile(kubeTokenFile)
			if err != nil {
				return err
			}
			runOptions.MetricsEnabled = true
			runOptions.KubeletMetrics = &kube.MetricsConfig{Endpoint: kubeletMetricsEndpoint, CAData: caData, BearerToken: string(tokenData)}
		}
		executor, err := workerexec.NewExecutor(runOptions)
		if err != nil {
			return err
		}
		if journalDir == "" {
			journalDir = filepath.Join(runStateDir, "journal")
		}
		journal, err := workergateway.OpenJournal(journalDir, 0)
		if err != nil {
			return err
		}
		defer journal.Close()
		rootCAs := x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM(caPEM) {
			return errors.New("worker状态目录CA无效，请重新执行 worker register")
		}
		workerClient, err := workergateway.NewWorkerClient(workergateway.WorkerConfig{
			URL: runServer, NodeID: identity.NodeID, WorkerID: identity.WorkerID, Version: workerVersion,
			TLS:     &tls.Config{MinVersion: tls.VersionTLS12, GetClientCertificate: holder.GetClientCertificate, RootCAs: rootCAs},
			Journal: journal, Executor: executor,
			Heartbeat: func() *platformv1.WorkerHeartbeat {
				return &platformv1.WorkerHeartbeat{ClusterNodeUid: facts.UID, Os: facts.OS, Arch: facts.Arch}
			},
			Qualification: func(nonce string) *platformv1.WorkerQualification {
				// TODO(资格发放流程): probe_artifact_digest/registry_id/credential_version
				// 待资格发放流程接入后补充；当前只上报资格文件登记的能力位与真实节点事实，
				// 由控制面 qualification.Verifier 与登记记录严格核对。
				if capabilities.QualificationID == "" {
					return nil
				}
				return &platformv1.WorkerQualification{QualificationId: capabilities.QualificationID, Nonce: nonce, ClusterNodeUid: facts.UID, Os: facts.OS, Arch: facts.Arch, Build: capabilities.Build, Runtime: capabilities.Runtime, Ingress: capabilities.Ingress, Metrics: capabilities.Metrics}
			},
			Instrumentation: obs.ConnectClientOptions(),
			OnError:         func(err error) { obs.Logger.Error("worker错误", zap.Error(err)) },
		})
		if err != nil {
			return err
		}
		guard.mu.Lock()
		guard.client = workerClient
		guard.mu.Unlock()
		// 自动证书轮换：到达身份文件的 RotateAfter 即向控制面换发新证书并
		// 回填 holder；失败只记录并按检查间隔重试（服务端7天窗口兜底），
		// 不影响窗口内旧证书的现有连接。
		rotation, err := workerexec.NewRotationManager(workerexec.RotationManagerConfig{Server: runServer, StateDir: runStateDir, ServerCA: runServerCA, RotationCheckInterval: rotationCheckInterval, OnRotationError: func(err error) {
			fmt.Fprintln(cmd.ErrOrStderr(), "worker轮换错误:", err)
		}}, holder)
		if err != nil {
			return err
		}
		rotationContext, cancelRotation := context.WithCancel(cmd.Context())
		defer cancelRotation()
		go func() { _ = rotation.Run(rotationContext) }()
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "worker %s/%s 连接 %s（节点UID %s）\n", identity.NodeID, identity.WorkerID, runServer, facts.UID)
		// ctx由进程信号控制；流断开由WorkerClient自动重连，Kubernetes长执行独立于流连接。
		return workerClient.Run(cmd.Context())
	}}
	run.Flags().StringVar(&runServer, "server", "", "控制面 worker 专用HTTPS入口地址（缺省用身份文件记录值）")
	run.Flags().StringVar(&runStateDir, "state-dir", "", "worker状态目录（0700）")
	run.Flags().StringVar(&runServerCAFile, "server-ca-file", "", "可选控制面CA证书PEM路径（自签部署，与 register 同义）")
	run.Flags().DurationVar(&rotationCheckInterval, "rotation-check-interval", time.Hour, "证书轮换检查间隔（默认1小时）")
	run.Flags().StringVar(&kubeAPIServer, "kube-apiserver", "", "Kubernetes HTTPS API server地址（绝不读取kubeconfig）")
	run.Flags().StringVar(&kubeCAFile, "kube-ca-file", "", "集群CA证书PEM路径")
	run.Flags().StringVar(&kubeTokenFile, "kube-token-file", "", "受限服务账户令牌文件路径")
	run.Flags().StringVar(&nodeName, "node-name", "", "本节点在集群中的名称")
	run.Flags().StringVar(&buildNamespace, "build-namespace", "", "构建Job所在命名空间")
	run.Flags().StringVar(&runtimeNamespace, "runtime-namespace", "", "应用运行时所在命名空间")
	run.Flags().StringVar(&storageClass, "storage-class", "", "可选持久卷StorageClass")
	run.Flags().StringVar(&ingressClass, "ingress-class", "", "可选IngressClass")
	run.Flags().StringVar(&runnerImage, "runner-image", "", "可选runner镜像（digest钉住，覆盖资格文件）")
	run.Flags().StringVar(&buildKitImage, "buildkit-image", "", "可选BuildKit镜像（digest钉住，覆盖资格文件）")
	run.Flags().StringVar(&qualificationFile, "qualification-file", "", "可选安装资格JSON文件（缺失时全部能力关闭）")
	run.Flags().StringVar(&journalDir, "journal-dir", "", "可选事件journal目录（缺省为state-dir/journal）")
	run.Flags().StringVar(&buildKitAddr, "buildkit-addr", "", "宿主构建模式：节点级 rootless BuildKit 守护 socket 地址（资格 host_build=true 时必填）")
	run.Flags().StringVar(&hostBuildCtl, "buildctl-path", "", "宿主构建模式：buildctl 固定二进制路径（资格 host_build=true 时必填）")
	run.Flags().StringVar(&kubeletMetricsEndpoint, "kubelet-metrics-endpoint", "", "节点 kubelet metrics 地址（如 https://节点IP:10250/metrics；资格 metrics=true 时与 --metrics-enabled 同开）")
	run.Flags().DurationVar(&observeInterval, "observe-interval", 0, "可选部署观察间隔（默认5秒）")
	for _, name := range []string{"state-dir", "kube-apiserver", "kube-ca-file", "kube-token-file", "node-name", "build-namespace", "runtime-namespace"} {
		_ = run.MarkFlagRequired(name)
	}
	worker.AddCommand(register, run)
	return worker
}
