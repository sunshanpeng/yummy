// Package workerexec 把控制面 worker 网关命令装配为节点内 Kubernetes 适配器调用，
// 是 worker 进程的产品执行内核：不读取 kubeconfig、不访问宿主 Shell、不新增常驻服务。
// 序列号策略：所有事件/日志/观察都以 Seq=0 交给 Emitter，由 workerclient.Journal 依据
// 已持久化序列自动递增（journal.append 在 seq==0 时回写 oldSeq+1），从而跨重连稳定。
package workerexec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// GenerationClient 由 workerclient.WorkerClient 实现：复用同一条已验证 mTLS 连接，
// 在每次 Kubernetes 写入前查询控制面当前持久期望态。
type GenerationClient interface {
	CheckGeneration(ctx context.Context, request *platformv1.WorkerGenerationRequest) error
}

// generationGuard 适配 kube.GenerationGuard；不缓存任何校验结果。
type generationGuard struct{ client GenerationClient }

func (g generationGuard) Check(ctx context.Context, c kube.RuntimeChange) error {
	return g.client.CheckGeneration(ctx, &platformv1.WorkerGenerationRequest{
		WorkspaceId:    c.Release.WorkspaceID,
		ApplicationId:  c.Release.ApplicationID,
		OperationId:    c.OperationID,
		Generation:     c.ExpectedGeneration,
		ReleaseId:      c.Release.ID,
		ClusterNodeUid: string(c.Release.Placement.UID),
	})
}

// GenerationGuard 把网关客户端包装成 kube.Runtime(GenerationGuard) 所需的守卫。
func GenerationGuard(client GenerationClient) kube.GenerationGuard {
	return generationGuard{client: client}
}

// Options 全部显式注入；worker 进程入口负责从 flags/文件构造，禁止环境隐式默认。
type Options struct {
	// Adapter 执行构建 Job 与运行时变更的 Kubernetes 适配器。
	Adapter *kube.Adapter
	// Client 仅用于按受管标签发现构建 Pod 以便跟随日志；真正的归属核对
	// 仍由 Adapter.FollowLogs 对 Job/Pod UID 重新验证完成。
	Client kubernetes.Interface
	// Node 本节点冻结身份；BuildTask.Node 与 RuntimeChange Placement 必须与其一致。
	Node kube.NodeTarget
	// BuildNamespace 构建命名空间，用于 Pod 发现与日志请求。
	BuildNamespace string
	// RuntimeNamespace 运行命名空间，用于部署失败条件复核等补充读取。
	RuntimeNamespace string
	// Generation 每次 Kubernetes 写入前的控制面代际守卫，必填。
	Generation kube.GenerationGuard
	// BuildPollInterval 构建证据轮询间隔（默认2秒）。
	BuildPollInterval time.Duration
	// Installed 安装资格能力快照（worker 启动时从资格文件解析）。
	Installed kube.InstalledCapabilities
	// BuildKitAddr 宿主构建模式下的节点级 rootless BuildKit 守护 socket 地址；
	// Installed.HostBuild 为真时必填，缺失即失败关闭。
	BuildKitAddr string
	// HostBuildCtl 宿主构建模式下的 buildctl 固定二进制路径；同为必填。
	HostBuildCtl string
	// ObserveInterval 部署观察间隔（默认5秒）。
	ObserveInterval time.Duration
	// SourceMaxBytes 源码归档大小上限；wire 协议未携带，须由节点配置显式给出
	//（默认1GiB，kube 适配器上限2GiB）。
	SourceMaxBytes int64
	// LogChunkBytes 单个日志块上限（默认16KiB，协议上限 maxLogChunk）。
	LogChunkBytes int
	// MetricsEnabled 是否采集并上报 CPU/内存基础监控；默认 false 失败关闭，
	// 未显式开启时绝不发射指标。
	MetricsEnabled bool
	// KubeletMetrics 本节点 kubelet /metrics/resource 的显式凭据注入（CA 与
	// nodes/metrics 受限凭据，形态即 kube.MetricsConfig）；MetricsEnabled 为真时
	// 必填，构造失败直接拒绝启动，指标采集器与 Executor 同生命周期。
	KubeletMetrics *kube.MetricsConfig
}

// Executor 实现 workerclient.Executor；生命周期独立于任何一次流连接，
// 长循环一律尊重 ctx 取消，返回非 nil 错误时保留 journal 待重投。
type Executor struct {
	options Options
	runtime *kube.Runtime
	// metrics 为 nil 表示未启用指标采集（MetricsEnabled 默认 false 失败关闭）；
	// 启用时跨 Deploy 复用，CPU counter 基线因此跨发布保持连续。
	metrics *kube.MetricsCollector
	// collectorsMu/collectors 管理按 release 的持久运行采集器（生命周期挂部署
	// 本体，见 collectors.go）；进程关闭时统一等待退出。
	collectorsMu sync.Mutex
	collectors   map[string]*collectorHandle
	wgCollectors sync.WaitGroup
}

// NewExecutor 校验并冻结执行参数，构造 kube.Runtime 变更通道。
func NewExecutor(options Options) (*Executor, error) {
	if options.Adapter == nil || options.Client == nil || options.Node.Name == "" || options.Node.UID == "" || options.BuildNamespace == "" || options.RuntimeNamespace == "" {
		return nil, errors.New("workerexec: 适配器、Pod客户端、构建/运行命名空间与节点身份必须显式注入")
	}
	if options.Generation == nil {
		return nil, errors.New("workerexec: 缺少控制面 generation 守卫")
	}
	if options.BuildPollInterval == 0 {
		options.BuildPollInterval = 2 * time.Second
	}
	if options.ObserveInterval == 0 {
		options.ObserveInterval = 5 * time.Second
	}
	if options.SourceMaxBytes == 0 {
		options.SourceMaxBytes = 1 << 30
	}
	if options.LogChunkBytes == 0 {
		options.LogChunkBytes = 16 << 10
	}
	if options.BuildPollInterval < 100*time.Millisecond || options.ObserveInterval < 100*time.Millisecond || options.SourceMaxBytes <= 0 || options.SourceMaxBytes > 2<<30 || options.LogChunkBytes <= 0 || options.LogChunkBytes > 16<<10 {
		return nil, errors.New("workerexec: 轮询间隔、源码上限或日志块大小越界")
	}
	runtime, err := options.Adapter.Runtime(options.Generation)
	if err != nil {
		return nil, fmt.Errorf("workerexec: 构造运行时通道失败: %w", err)
	}
	var metrics *kube.MetricsCollector
	if options.MetricsEnabled {
		if options.KubeletMetrics == nil {
			return nil, errors.New("workerexec: 启用指标采集必须显式注入Kubelet metrics凭据")
		}
		collector, err := options.Adapter.Metrics(*options.KubeletMetrics)
		if err != nil {
			return nil, fmt.Errorf("workerexec: 构造Kubelet指标采集器失败: %w", err)
		}
		metrics = collector
	}
	return &Executor{options: options, runtime: runtime, metrics: metrics}, nil
}

// Build 把构建命令翻译为 kube.BuildTask 并按 RUN/CANCEL/RECONCILE 三种动作执行。
// reconcile 只作提示：同一 attempt 永远复用控制面 StableJobName，kube.Run 先读后建、
// 绝不补跑已确认的 Job，重复安全性由适配器契约保证，Executor 不另起第二个任务。
func (e *Executor) Build(ctx context.Context, command *platformv1.WorkerBuildCommand, reconcile bool, emitter Emitter) error {
	if command == nil || emitter == nil {
		return errors.New("workerexec: 构建命令或发射器为空")
	}
	task, err := e.buildTask(command)
	if err != nil {
		return err
	}
	switch command.Action {
	case "RUN":
		if e.options.Installed.HostBuild {
			return e.runHostBuild(ctx, command, task, emitter)
		}
		if err := e.options.Adapter.BootstrapBuildNamespace(ctx, task.SpaceSlug, task.WorkspaceID); err != nil {
			return err
		}
		return e.runBuild(ctx, command, task, emitter)
	case "CANCEL":
		if e.options.Installed.HostBuild {
			return e.cancelHostBuild(ctx, command, emitter)
		}
		return e.cancelBuild(ctx, command, task, emitter)
	case "RECONCILE":
		if e.options.Installed.HostBuild {
			return e.runHostBuild(ctx, command, task, emitter)
		}
		if err := e.options.Adapter.BootstrapBuildNamespace(ctx, task.SpaceSlug, task.WorkspaceID); err != nil {
			return err
		}
		return e.reconcileBuild(ctx, command, task, emitter)
	default:
		return fmt.Errorf("workerexec: 未知构建动作 %q", command.Action)
	}
}

// secretWireVersion 是构建密钥的固定版本标记：wire 协议未携带每个密钥的版本号，
// 固定值保证同一 attempt 的 Job 快照指纹稳定（密钥明文不参与 kube 快照指纹）。
// maxLogChunk 是分块日志的协议上限，与控制面 build 域的 MaxLogChunk 保持一致。
const maxLogChunk = 16 << 10

const secretWireVersion = "wire-v1"

// buildTask 只做字段翻译与本地一致性检查；Kubernetes 安全校验留给 kube 适配器。
// JobName/StableJobName 一律取 command.StableJobName：控制面持久名是事实来源。
func (e *Executor) buildTask(command *platformv1.WorkerBuildCommand) (kube.BuildTask, error) {
	task := kube.BuildTask{
		AttemptID:     command.AttemptId,
		JobName:       command.StableJobName,
		StableJobName: command.StableJobName,
		WorkspaceID:   command.WorkspaceId,
		ApplicationID: command.ApplicationId,
		SpaceSlug:     command.SpaceSlug,
		Node:          e.options.Node,
		Platform:      command.Platform,
		Dockerfile:    command.Dockerfile,
		Context:       command.ContextPath,
		Target:        command.Target,
		BuildArgs:     command.BuildArgs,
		ImageTag:      command.Repository + ":" + command.Tag,
		JobUID:        types.UID(command.JobUid),
		// JobUid 非空说明控制面 attempt 已持久化该 UID（此前 ACCEPTED 已被确认）；
		// kube 适配器据此在 Job 消失时拒绝补跑。
		Acknowledged: command.JobUid != "",
	}
	if command.SourceGrant != nil {
		task.Source = kube.SourceGrant{
			URI:           command.SourceGrant.Url,
			ArchiveSHA256: command.SourceGrant.Sha256,
			MaxBytes:      e.options.SourceMaxBytes,
			Token:         []byte(command.SourceGrant.Token),
		}
	}
	if command.Registry != nil {
		config, err := dockerConfigJSON(command.Registry)
		if err != nil {
			return kube.BuildTask{}, err
		}
		task.RegistryDockerConfig = config
	}
	for _, secret := range command.Secrets {
		task.Secrets = append(task.Secrets, kube.BuildSecret{ID: secret.Name, Version: secretWireVersion, Value: secret.Value})
	}
	return task, nil
}

// dockerConfigJSON 把控制面下发的一次性仓库凭据转换为固定 auths 形态的
// dockerconfig（kube 适配器与 runner 只接受该形态，禁止凭据 helper）。
// Origin 与仓库 host 由控制面在签发侧强一致，此处再做一次显式核对。
func dockerConfigJSON(credential *platformv1.WorkerRegistryCredential) ([]byte, error) {
	origin, err := url.Parse(credential.GetOrigin())
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("workerexec: 仓库凭据 Origin 必须是无路径参数的HTTPS地址")
	}
	auth := base64.StdEncoding.EncodeToString([]byte(credential.GetUsername() + ":" + string(credential.GetPassword())))
	// auths 键必须是 host:port 完整形态（origin.Host）：带端口的私有仓库
	// 若只取 Hostname()（丢端口），buildkitd 推送时按 host:port 查凭据必然
	// 落空导致 401（2026-11-13 实机验收缺陷⑤）。
	return json.Marshal(map[string]map[string]map[string]string{
		"auths": {origin.Host: {"auth": auth}},
	})
}

// listManagedBuildPods 按受管标签列出本 attempt 的构建 Pod；只做发现，
// 归属与安全核对由 FollowLogs 对 Job/Pod owner UID 重新验证完成。
func (e *Executor) listManagedBuildPods(ctx context.Context, attemptID string, spaceSlug string) ([]corev1.Pod, error) {
	selector := kube.LabelManaged + "=worker," + kube.LabelAttempt + "=" + attemptID + "," + kube.LabelNodeUID + "=" + string(e.options.Node.UID)
	// 构建命名空间按空间派生（旧命令回退安装配置的固定命名空间）。
	buildNS := e.options.BuildNamespace
	if spaceSlug != "" {
		buildNS = kube.BuildNamespaceFor(spaceSlug)
	}
	pods, err := e.options.Client.CoreV1().Pods(buildNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// runBuild 用 kube.BuildTask 启动同一稳定 Job 名的构建（kube.Run 先读后建，
// 已确认的 Job 绝不补跑），发射 ACCEPTED 后转入证据轮询直到终态。
// 终态 Digest 只取 kube 适配器从 build 容器终止消息 metadata 解析出的证据，
// 绝不从日志文本推断；拿不到 UID 运行证据立即失败，不伪造任何事件。
func (e *Executor) runBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, emitter Emitter) error {
	evidence, err := e.options.Adapter.Run(ctx, task)
	if err != nil {
		return err
	}
	if !evidence.Found || evidence.JobUID == "" {
		return fmt.Errorf("workerexec: Job %s 缺少UID运行证据", task.JobName)
	}
	task.JobUID = evidence.JobUID
	task.Acknowledged = true
	event := &platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "ACCEPTED", JobUid: string(evidence.JobUID)}
	if err := emitter.BuildEvent(event); err != nil {
		return err
	}
	return e.trackBuild(ctx, command, task, emitter, evidence)
}

// trackBuild 轮询构建证据直到终态；日志跟随是尽力而为的旁路，日志失败或
// 无法排空只影响 LogsComplete，不得阻断状态事件。ctx 取消立即退出且不发终态，
// journal 保留已持久化事件，由 RECONCILE 重投恢复。
func (e *Executor) trackBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, emitter Emitter, evidence kube.BuildEvidence) error {
	logCtx, stopLogs := context.WithCancel(ctx)
	defer stopLogs()
	logsDone := make(chan bool, 1)
	go func() {
		drained, err := e.followBuildLogs(logCtx, task, emitter)
		if drained && err == nil {
			logsDone <- true
			return
		}
		logsDone <- false
	}()
	for !evidence.Complete && !evidence.Failed {
		if err := sleepContext(ctx, e.options.BuildPollInterval); err != nil {
			return err
		}
		// 已确认的 Job 只按 UID 恢复只读观察；消失即 ErrJobLost，绝不补跑。
		next, err := e.options.Adapter.Reconcile(ctx, task)
		if err != nil {
			return err
		}
		if next.JobUID != "" {
			task.JobUID = next.JobUID
			task.Acknowledged = true
		}
		if next.Found {
			evidence = next
		}
	}
	// 给日志流最多1秒排空窗口；只有确认自然排空才声明 LogsComplete。
	logsComplete := false
	timer := time.NewTimer(logDrainGrace)
	defer timer.Stop()
	select {
	case drained := <-logsDone:
		logsComplete = drained
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		stopLogs()
	}
	return e.emitBuildTerminal(command, evidence, logsComplete, emitter)
}

// logDrainGrace 是终态确认前等待构建日志流自然排空的窗口；Pod 终止后日志
// 排空通常立即完成，超时只影响 LogsComplete 标志，不改变状态证据。
const logDrainGrace = time.Second

// emitBuildTerminal 把 kube 证据映射为终态构建事件：SUCCEEDED 必须携带
// termination metadata 的 Digest；Job 完成但缺少可信 metadata 时按适配器
// 约束拒绝视为成功，如实上报 FAILED。
func (e *Executor) emitBuildTerminal(command *platformv1.WorkerBuildCommand, evidence kube.BuildEvidence, logsComplete bool, emitter Emitter) error {
	event := &platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, JobUid: string(evidence.JobUID), LogsComplete: logsComplete}
	switch {
	case evidence.Complete && evidence.Digest != "":
		event.Kind = "SUCCEEDED"
		event.Digest = evidence.Digest
	case evidence.Complete:
		event.Kind = "FAILED"
		event.ErrorCode = "ARTIFACT_METADATA_MISSING"
	default:
		event.Kind = "FAILED"
		event.ErrorCode = terminalErrorCode(evidence.Reason)
	}
	return emitter.BuildEvent(event)
}

// cancelBuild 删除稳定 Job 名对应的 Job 并循环确认；只有 kube 适配器返回
// Stopped（Job 与 Pod 均已消失）才发射 STOPPED，不凭删除请求编造终态。
// JobUID 用控制面已持久化的运行证据（command.JobUid），从不发明新 UID。
func (e *Executor) cancelBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, emitter Emitter) error {
	for {
		stop, err := e.options.Adapter.Cancel(ctx, task)
		if err != nil {
			return err
		}
		if stop.Stopped {
			return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "STOPPED", JobUid: command.JobUid, LogsComplete: true})
		}
		if err := sleepContext(ctx, e.options.BuildPollInterval); err != nil {
			return err
		}
	}
}

// reconcileBuild 按持久化 JobUID 恢复只读观察：绝不创建或补跑 Job。
// 终态证据存在时发射终态事件封存执行，否则报告 RUNNING 让控制面继续调度；
// Job 尚未创建时静默返回，等待 RUN 流程建立，不做任何伪造状态。
func (e *Executor) reconcileBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, emitter Emitter) error {
	evidence, err := e.options.Adapter.Reconcile(ctx, task)
	if err != nil {
		return err
	}
	if !evidence.Found || evidence.JobUID == "" {
		return nil
	}
	if !evidence.Complete && !evidence.Failed {
		return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "RUNNING", JobUid: string(evidence.JobUID)})
	}
	return e.emitBuildTerminal(command, evidence, false, emitter)
}

// followBuildLogs 按受管标签发现本 attempt 的构建 Pod 并跟随 build 容器日志，
// 按固定块大小发射；单实例排空后不再重复跟随。跟随失败不影响构建状态。
func (e *Executor) followBuildLogs(ctx context.Context, task kube.BuildTask, emitter Emitter) (bool, error) {
	drained := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		pods, err := e.listManagedBuildPods(ctx, task.AttemptID, task.SpaceSlug)
		if err != nil {
			return false, err
		}
		pod, status := pickBuildContainer(pods)
		if pod == nil {
			if err := sleepContext(ctx, e.options.BuildPollInterval); err != nil {
				return false, err
			}
			continue
		}
		terminated := status.State.Terminated != nil
		key := string(pod.UID) + "/" + strconv.Itoa(int(status.RestartCount))
		if drained[key] {
			if terminated {
				return true, nil
			}
			if err := sleepContext(ctx, e.options.BuildPollInterval); err != nil {
				return false, err
			}
			continue
		}
		followNS := e.options.BuildNamespace
		if task.SpaceSlug != "" {
			followNS = kube.BuildNamespaceFor(task.SpaceSlug)
		}
		stream, err := e.options.Adapter.FollowLogs(ctx, task, kube.LogRequest{Namespace: followNS, PodName: pod.Name, PodUID: pod.UID, Container: "build", RestartCount: status.RestartCount})
		if err != nil {
			return false, err
		}
		err = e.drainLogStream(ctx, stream, task.AttemptID, emitter)
		_ = stream.Close()
		if err != nil {
			return false, err
		}
		drained[key] = true
		if terminated {
			return true, nil
		}
	}
}

// drainLogStream 读取日志流并按 LogChunkBytes 分块发射；块大小不超过
// workerclient.Journal 的单块上限，末尾不足一块时单独发射。
func (e *Executor) drainLogStream(ctx context.Context, stream *kube.LogStream, attemptID string, emitter Emitter) error {
	buffer := make([]byte, e.options.LogChunkBytes)
	pending := []byte{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := stream.Read(buffer)
		if n > 0 {
			pending = append(pending, buffer[:n]...)
			for len(pending) >= e.options.LogChunkBytes {
				chunk := append([]byte(nil), pending[:e.options.LogChunkBytes]...)
				if err := emitter.BuildLog(&platformv1.WorkerBuildLog{AttemptId: attemptID, Content: chunk}); err != nil {
					return err
				}
				pending = pending[e.options.LogChunkBytes:]
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				break
			}
			return readErr
		}
	}
	if len(pending) > 0 {
		return emitter.BuildLog(&platformv1.WorkerBuildLog{AttemptId: attemptID, Content: pending})
	}
	return nil
}

// pickBuildContainer 选择需要跟随日志的 Pod 与 build 容器状态；
// 优先仍在运行的实例，全部终止时取第一个有证据的实例。
func pickBuildContainer(pods []corev1.Pod) (*corev1.Pod, *corev1.ContainerStatus) {
	var fallbackPod *corev1.Pod
	var fallbackStatus *corev1.ContainerStatus
	for i := range pods {
		pod := &pods[i]
		for j := range pod.Status.ContainerStatuses {
			status := &pod.Status.ContainerStatuses[j]
			if status.Name != "build" {
				continue
			}
			if status.State.Terminated == nil {
				return pod, status
			}
			if fallbackPod == nil {
				fallbackPod, fallbackStatus = pod, status
			}
		}
	}
	return fallbackPod, fallbackStatus
}

// terminalErrorCode 把终止原因转成有界错误码；空值与噪声统一回退为
// BUILD_FAILED，控制面错误码约束（无NUL、有限长度）由转换保证。
func terminalErrorCode(reason string) string {
	clean := strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, reason), "_")
	if clean == "" {
		return "BUILD_FAILED"
	}
	if len(clean) > 128 {
		clean = clean[:128]
	}
	return clean
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
