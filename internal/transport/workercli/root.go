// Package workercli 装配执行宿主机上的登记和运行命令。
package workercli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	"github.com/sunshanpeng/yummy/internal/infra/workerexec"
	"github.com/sunshanpeng/yummy/internal/lib/observability"
	"github.com/sunshanpeng/yummy/internal/lib/startupconfig"
	"github.com/sunshanpeng/yummy/internal/transport/workerclient"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// workerVersion 是 worker 连接时上报的版本串。
const workerVersion = "yummy-worker/1"

func Execute(ctx context.Context, args []string) error {
	settings, err := startupconfig.Load(args)
	if err != nil {
		return err
	}
	command := newRoot(settings)
	command.SetArgs(args)
	return command.ExecuteContext(ctx)
}

// generationClientHolder 让执行器在写入 Kubernetes 前复用 worker 客户端的
// mTLS 连接检查 generation。run 命令完成客户端装配后回填引用。
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

func newRoot(settings map[string]any) *cobra.Command {
	worker, config := startupconfig.NewCommand("yummy-worker", "注册并运行本节点 worker 进程", settings)
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
		railpackFrontendImage, railpackPath                              string
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
		capabilities, err := workerexec.LoadInstalledCapabilities(qualificationFile, runnerImage, buildKitImage, railpackFrontendImage)
		if err != nil {
			return err
		}
		node := kube.NodeTarget{Name: nodeName, UID: types.UID(facts.UID)}
		adapter, err := kube.NewFromRESTConfig(restConfig, kube.Config{Node: node, BuildNamespace: buildNamespace, RuntimeNamespace: runtimeNamespace, Installed: capabilities, StorageClass: storageClass, IngressClass: ingressClass})
		if err != nil {
			return err
		}
		guard := &generationClientHolder{}
		runOptions := workerexec.Options{Adapter: adapter, Client: client, Node: node, BuildNamespace: buildNamespace, RuntimeNamespace: runtimeNamespace, Generation: workerexec.GenerationGuard(guard), ObserveInterval: observeInterval, Installed: capabilities, BuildKitAddr: buildKitAddr, HostBuildCtl: hostBuildCtl, RailpackPath: railpackPath}
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
		journal, err := workerclient.OpenJournal(journalDir, 0)
		if err != nil {
			return err
		}
		defer journal.Close()
		rootCAs := x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM(caPEM) {
			return errors.New("worker状态目录CA无效，请重新执行 yummy-worker register")
		}
		workerClient, err := workerclient.NewWorkerClient(workerclient.WorkerConfig{
			URL: runServer, NodeID: identity.NodeID, WorkerID: identity.WorkerID, Version: workerVersion,
			TLS:     &tls.Config{MinVersion: tls.VersionTLS12, GetClientCertificate: holder.GetClientCertificate, RootCAs: rootCAs},
			Journal: journal, Executor: executor,
			Heartbeat: func() *platformv1.WorkerHeartbeat {
				probeContext, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
				defer cancel()
				report := workerexec.ReadClusterMachines(probeContext, client)
				return &platformv1.WorkerHeartbeat{ClusterNodeUid: facts.UID, Os: facts.OS, Arch: facts.Arch, ClusterMachines: report}
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
	run.Flags().StringVar(&railpackFrontendImage, "railpack-frontend-image", "", "可选 Railpack BuildKit 前端镜像（digest 钉住，覆盖资格文件）；与所用 Railpack 版本配套。未配置时本节点拒绝没有 Dockerfile 的构建")
	run.Flags().StringVar(&railpackPath, "railpack-path", "", "宿主构建模式：Railpack 固定二进制路径（与 --railpack-frontend-image 同时配置才接受没有 Dockerfile 的构建；Pod 模式随 runner 镜像提供）")
	run.Flags().StringVar(&kubeletMetricsEndpoint, "kubelet-metrics-endpoint", "", "节点 kubelet metrics 地址（如 https://节点IP:10250/metrics；资格 metrics=true 时与 --metrics-enabled 同开）")
	run.Flags().DurationVar(&observeInterval, "observe-interval", 0, "可选部署观察间隔（默认5秒）")
	for _, name := range []string{"state-dir", "kube-apiserver", "kube-ca-file", "kube-token-file", "node-name", "build-namespace", "runtime-namespace"} {
		_ = run.MarkFlagRequired(name)
	}
	worker.AddCommand(register, run)
	return worker
}
