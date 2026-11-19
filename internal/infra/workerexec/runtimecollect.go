package workerexec

// 运行日志跟随与基础监控采样是 Deploy 观察循环的尽力而为旁路：
// - 日志按 (pod_uid, "app", restart_count) 流键跟随当前发布的存活 Pod，分块
//   不超过 LogChunkBytes，按观察间隔逐块发射（发射频率不高于 ObserveInterval）；
//   Pod 重启后开新流，同一键排空后不再重复跟随，Pod 消失或 Deploy 结束即停止。
// - 指标在 MetricsEnabled 且显式注入 Kubelet 凭据时才启用：每个观察间隔对
//   Ready 且非 Deleting 的当前发布 Pod 采样一次，CPU 复用 kube.MetricsCollector
//   的同 startup counter 差值，首样本、counter 重置或字段缺测如实跳过不发。
// 所有旁路 goroutine 由观察循环派生的 ctx 驱动，任何失败只丢弃旁路工作，
// 绝不影响 observeLoop 状态机收敛；Seq=0 交给 workergateway.Journal 递增，
// 重连重放按 journal 与控制面两侧的幂等键去重。

import (
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"sync"
	"time"

	"fmt"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	corev1 "k8s.io/api/core/v1"
	"os"

	"k8s.io/apimachinery/pkg/types"
)

// runtimeLogChunk 是待发射的运行日志块：流身份在入队时固定，发射节奏由
// 管理循环按观察间隔节流。
type runtimeLogChunk struct {
	podUID   types.UID
	restarts int32
	content  []byte
}

// streamResult 报告一个跟随流的结局：drained 表示自然排空（该键不再重复跟随），
// 失败或被取消的键留待下一次证据更新重新决定。
type streamResult struct {
	key     string
	drained bool
}

// runtimeTarget 是指标采样目标：仅当前发布且 Ready、非 Deleting 的 Pod。
type runtimeTarget struct {
	podUID types.UID
}

// runtimeSidecar 挂接 observeLoop 生命周期：管理循环负责切换日志跟随流并按
// 观察间隔发射日志块，指标循环独立采样。updates/finished/chunks 均有界，
// followKey/followCancel/drained 只被管理循环访问，target 由互斥锁保护。
type runtimeSidecar struct {
	exec    *Executor
	command *platformv1.WorkerDeploymentCommand
	change  kube.RuntimeChange
	emitter Emitter

	updates  chan kube.RuntimeEvidence
	finished chan streamResult
	chunks   chan runtimeLogChunk
	wg       sync.WaitGroup

	mu     sync.Mutex
	target runtimeTarget

	followKey       string
	followCancel    context.CancelFunc
	drained         map[string]bool
	foundNone       bool
	metricErrLogged map[string]bool
}

func newRuntimeSidecar(exec *Executor, command *platformv1.WorkerDeploymentCommand, change kube.RuntimeChange, emitter Emitter) *runtimeSidecar {
	return &runtimeSidecar{
		exec:     exec,
		command:  command,
		change:   change,
		emitter:  emitter,
		updates:  make(chan kube.RuntimeEvidence, 1),
		finished: make(chan streamResult, 1),
		// 缓冲只吸收一个观察间隔内的读取抖动；通道满时跟随流读取侧背压暂停，
		// kubelet 会继续落盘日志文件，不丢数据。
		chunks:  make(chan runtimeLogChunk, 8),
		drained: map[string]bool{},
	}
}

// start 启动旁路；未启用指标采集时不启动指标循环，保证 MetricsEnabled=false 零发射。
func (s *runtimeSidecar) start(ctx context.Context) {
	s.wg.Add(1)
	go s.run(ctx)
	if s.exec.metrics != nil {
		s.wg.Add(1)
		go s.runMetrics(ctx)
	}
}

func (s *runtimeSidecar) wait() { s.wg.Wait() }

// update 投递最新运行证据；通道忙时丢弃，下一次观察循环会再次投递。
func (s *runtimeSidecar) update(evidence kube.RuntimeEvidence) {
	select {
	case s.updates <- evidence:
	default:
	}
}

// run 是旁路管理循环：按最新证据切换跟随流，并按观察间隔逐块发射运行日志。
func (s *runtimeSidecar) run(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.exec.options.ObserveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopFollower()
			return
		case evidence := <-s.updates:
			s.applyEvidence(ctx, evidence)
		case result := <-s.finished:
			if result.drained {
				s.drained[result.key] = true
			}
			if s.followKey == result.key {
				s.followKey, s.followCancel = "", nil
			}
		case <-ticker.C:
			s.emitLogChunk()
		}
	}
}

// applyEvidence 依据最新证据选择旁路目标：日志跟随当前发布且存活的 app 容器，
// 指标只记录 Ready 且非 Deleting 的发布 Pod。
func (s *runtimeSidecar) applyEvidence(ctx context.Context, evidence kube.RuntimeEvidence) {
	logPod, metricPod := runtimeTargets(s.command, evidence)
	target := runtimeTarget{}
	if metricPod != nil {
		target = runtimeTarget{podUID: metricPod.UID}
	}
	s.mu.Lock()
	s.target = target
	s.mu.Unlock()

	if logPod == nil {
		// 可运维性：无合格跟随目标时输出一次原因（pod 数与资格），便于节点侧定位旁路静默。
		if s.followKey == "" {
			s.foundNone = true
			fmt.Fprintln(os.Stderr, "worker日志旁路: 本轮无合格跟随目标, pods=", len(evidence.Pods))
		}
		s.stopFollower()
		return
	}
	if s.foundNone {
		s.foundNone = false
		fmt.Fprintln(os.Stderr, "worker日志旁路: 恢复跟随", logPod.Name)
	}
	key := string(logPod.UID) + "/" + strconv.Itoa(int(logPod.Restarts))
	if key == s.followKey {
		return
	}
	s.stopFollower()
	if s.drained[key] {
		return
	}
	s.startFollower(ctx, key, *logPod)
}

// stopFollower 停止当前日志跟随；未发射的待发块按旁路语义直接丢弃。
func (s *runtimeSidecar) stopFollower() {
	if s.followCancel != nil {
		s.followCancel()
	}
	s.followKey, s.followCancel = "", nil
}

// startFollower 为给定流键打开跟随流。FollowLogs 的归属与 restart 一致性校验
// 全部由 kube 适配器完成；打开失败按旁路失败处理，等待下一次证据更新重试。
func (s *runtimeSidecar) startFollower(ctx context.Context, key string, pod kube.PodEvidence) {
	streamCtx, cancel := context.WithCancel(ctx)
	// 命名空间与 change 派生一致（按空间或旧配置回退），保持 FollowLogs 归属校验通过。
	logNS := s.exec.options.RuntimeNamespace
	if s.change.Release.SpaceSlug != "" {
		logNS = kube.RuntimeNamespaceFor(s.change.Release.SpaceSlug)
	}
	stream, err := s.exec.runtime.FollowLogs(streamCtx, s.change, kube.LogRequest{
		Namespace:    logNS,
		PodName:      pod.Name,
		PodUID:       pod.UID,
		Container:    "app",
		RestartCount: pod.Restarts,
	})
	if err != nil {
		// 可运维性：跟随流打开失败原样落节点日志（旁路语义不重试不告警）。
		fmt.Fprintln(os.Stderr, "worker日志旁路: 跟随流打开失败:", err)
		cancel()
		return
	}
	s.followKey, s.followCancel = key, cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		drained := s.followRuntimeStream(streamCtx, stream, pod.UID, pod.Restarts, s.emitter)
		select {
		case s.finished <- streamResult{key: key, drained: drained}:
		case <-streamCtx.Done():
		}
	}()
}

// followRuntimeStream 读取跟随流并按 LogChunkBytes 切块入队：排空（EOF）时补发
// 尾块并返回 true；ctx 取消或读取失败丢弃残余并返回 false（交由重试语义处理，
// 流内已发序号仍连续，尾部缺失不产生断号）。
func (s *runtimeSidecar) followRuntimeStream(ctx context.Context, stream *kube.LogStream, podUID types.UID, restarts int32, emitter Emitter) bool {
	defer stream.Close()
	chunkSize := s.exec.options.LogChunkBytes
	identity := runtimeLogChunk{podUID: podUID, restarts: restarts}
	buffer := make([]byte, 0, chunkSize)
	readBuf := make([]byte, 4096)
	emitDirect := func(content []byte) bool {
		// 取消/排空路径的同步尾块：管理循环可能已退出（不再消费 chunks 队列），
		// 残余日志直接经 emitter 发射；失败按旁路语义丢弃。
		value := &platformv1.WorkerRuntimeLog{
			WorkspaceId:   s.command.WorkspaceId,
			ApplicationId: s.command.ApplicationId,
			OperationId:   s.command.OperationId,
			ReleaseId:     s.command.ReleaseId,
			Generation:    s.command.Generation,
			PodUid:        string(identity.podUID),
			Container:     "app",
			RestartCount:  identity.restarts,
			Seq:           0,
			Content:       content,
		}
		return emitter.RuntimeLog(value) == nil
	}
	lastFlush := time.Now()
	for {
		n, readErr := stream.Read(readBuf)
		if n > 0 {
			buffer = append(buffer, readBuf[:n]...)
			// 时间刷出（发现⑨剩余部分）：分块阈值 16KiB 对低频日志永远凑不满，
			// 按观察间隔把半满 buffer 刷成小块，保证运行日志按时间可见。
			if len(buffer) > 0 && time.Since(lastFlush) >= s.exec.options.ObserveInterval {
				content := append([]byte(nil), buffer...)
				buffer = buffer[:0]
				select {
				case s.chunks <- runtimeLogChunk{podUID: identity.podUID, restarts: identity.restarts, content: content}:
				default:
				}
				lastFlush = time.Now()
			}
			for len(buffer) >= chunkSize {
				chunk := append([]byte(nil), buffer[:chunkSize]...)
				select {
				case s.chunks <- runtimeLogChunk{podUID: identity.podUID, restarts: identity.restarts, content: chunk}:
					buffer = buffer[chunkSize:]
				case <-ctx.Done():
					return false
				}
			}
		}
		if readErr != nil {
			// 尾块策略：EOF（pod 终止/排空）与取消/超时（部署跟踪结束）都把残余
			// buffer 作为尾块同步发射——Follow=true 的流永不 EOF，若无尾块发射，
			// 快速收敛的部署与低频日志应用将永远凑不满分块阈值而零产出（发现⑨）。
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				if len(buffer) > 0 {
					select {
					case s.chunks <- runtimeLogChunk{podUID: identity.podUID, restarts: identity.restarts, content: append([]byte(nil), buffer...)}:
					case <-ctx.Done():
						emitDirect(append([]byte(nil), buffer...))
					}
				}
				return errors.Is(readErr, io.EOF)
			}
			if len(buffer) > 0 {
				emitDirect(append([]byte(nil), buffer...))
			}
			return false
		}
		if ctx.Err() != nil {
			if len(buffer) > 0 {
				emitDirect(append([]byte(nil), buffer...))
			}
			return false
		}
	}
}

// emitLogChunk 每个观察间隔最多发射一块运行日志：字段如实取当前命令与运行证据，
// Seq 固定 0 由 journal 按流键递增；发射失败只停止当前跟随（等待下一次证据更新
// 决定是否重试），绝不影响观察循环。
func (s *runtimeSidecar) emitLogChunk() {
	var chunk runtimeLogChunk
	select {
	case chunk = <-s.chunks:
	default:
		return
	}
	value := &platformv1.WorkerRuntimeLog{
		WorkspaceId:   s.command.WorkspaceId,
		ApplicationId: s.command.ApplicationId,
		OperationId:   s.command.OperationId,
		ReleaseId:     s.command.ReleaseId,
		Generation:    s.command.Generation,
		PodUid:        string(chunk.podUID),
		Container:     "app",
		RestartCount:  chunk.restarts,
		Seq:           0,
		Content:       chunk.content,
	}
	if err := s.emitter.RuntimeLog(value); err != nil {
		fmt.Fprintln(os.Stderr, "worker日志旁路: 发射失败:", err)
		s.stopFollower()
	}
}

// runMetrics 按观察间隔对当前发布 Pod 采样一次；任何失败只跳过本轮。
// seen 与 kube.MetricsCollector 的内部基线同步记账（每次采集出现的 Pod 都记录
// 采集时刻），保证发射窗口与采集器差值窗口一致；lastSent 保证 journal 幂等键
// (pod_uid, collected_unix_milli) 对同一 Pod 严格递增。
func (s *runtimeSidecar) runMetrics(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.exec.options.ObserveInterval)
	defer ticker.Stop()
	seen := map[types.UID]time.Time{}
	lastSent := map[types.UID]int64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			target := s.target
			s.mu.Unlock()
			if target.podUID == "" {
				continue
			}
			samples, err := s.exec.metrics.Collect(ctx)
			if err != nil {
				// 可运维性：指标采集失败按旁路语义静默重试，但把首轮失败落节点日志。
				if _, seen := s.metricErrLogged[err.Error()]; !seen {
					if s.metricErrLogged == nil {
						s.metricErrLogged = map[string]bool{}
					}
					s.metricErrLogged[err.Error()] = true
					fmt.Fprintln(os.Stderr, "worker指标旁路: 采集失败:", err)
				}
				continue
			}
			if s.metricErrLogged != nil {
				for key := range s.metricErrLogged {
					fmt.Fprintln(os.Stderr, "worker指标旁路: 采集恢复")
					delete(s.metricErrLogged, key)
				}
			}
			for _, sample := range samples {
				previous, hasPrevious := seen[sample.PodUID]
				if sample.PodUID == target.podUID {
					s.emitMetric(sample, previous, hasPrevious, lastSent)
				}
				seen[sample.PodUID] = sample.CollectedAt
			}
		}
	}
}

// emitMetric 只在 CPU 差值与内存同时可得且窗口为正时发射一次采样；
// collected_unix_milli 使用 worker 本地时钟（服务端只拒时间回退与同键异值）。
func (s *runtimeSidecar) emitMetric(sample kube.MetricSample, previous time.Time, hasPrevious bool, lastSent map[types.UID]int64) {
	if !hasPrevious || sample.CPUCores == nil || sample.MemoryWorkingSetBytes == nil {
		return
	}
	windowMillis := sample.CollectedAt.Sub(previous).Milliseconds()
	if windowMillis <= 0 {
		return
	}
	nanoCores := math.Round(*sample.CPUCores * float64(windowMillis) * 1e6)
	if math.IsNaN(nanoCores) || nanoCores < 0 || nanoCores > math.MaxInt64 {
		return
	}
	collected := time.Now().UnixMilli()
	if last, ok := lastSent[sample.PodUID]; ok && collected <= last {
		collected = last + 1
	}
	lastSent[sample.PodUID] = collected
	// 发射失败只丢弃本次采样：journal 保留已持久化样本，下一轮重新采集。
	_ = s.emitter.RuntimeMetric(&platformv1.WorkerRuntimeMetric{
		WorkspaceId:           s.command.WorkspaceId,
		ApplicationId:         s.command.ApplicationId,
		ReleaseId:             s.command.ReleaseId,
		Generation:            s.command.Generation,
		PodUid:                string(sample.PodUID),
		CpuNanoCores:          int64(nanoCores),
		CpuWindowMillis:       windowMillis,
		MemoryWorkingSetBytes: *sample.MemoryWorkingSetBytes,
		CollectedUnixMilli:    collected,
	})
}

// runtimeTargets 从运行证据中选择旁路目标：就绪的当前发布 Pod 同时作为日志与
// 指标的首选目标；无就绪 Pod 时日志回退到存活（未终止、非 Deleting）的当前
// 发布 Pod，指标保持无目标。选择与 deploymentObservation 的 PodUid 一致，
// 绝不冒充其他发布的证据。
func runtimeTargets(command *platformv1.WorkerDeploymentCommand, evidence kube.RuntimeEvidence) (*kube.PodEvidence, *kube.PodEvidence) {
	var (
		logPod, metricPod *kube.PodEvidence
		fallback          kube.PodEvidence
		hasFallback       bool
	)
	for i := range evidence.Pods {
		pod := &evidence.Pods[i]
		if pod.ReleaseID != command.ReleaseId || pod.Deleting || pod.Phase == string(corev1.PodSucceeded) || pod.Phase == string(corev1.PodFailed) {
			continue
		}
		if pod.Ready {
			if metricPod == nil {
				metricPod = pod
			}
			if logPod == nil {
				logPod = pod
			}
			continue
		}
		if !hasFallback {
			fallback, hasFallback = *pod, true
		}
	}
	if logPod == nil && hasFallback {
		logPod = &fallback
	}
	return logPod, metricPod
}
