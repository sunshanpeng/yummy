// 持久运行采集器注册表：运行日志/指标采集的生命周期挂部署本体而非操作跟踪窗口
// （发现⑨根因，2026-11-14 实机确认——部署快速收敛后跟踪窗口只剩数秒，旁路来不及
// 产出任何块）。Apply 成功即按应用启动，部署消失（Observe NotFound）或显式
// STOP 时停止；同一应用同一 release+generation 重复下发（RECONCILE 重放）幂等复用，
// release 或 generation 变化（RESTART 提升代际、发布新版本、回滚）时替换既有采集器：
// 运行 Deployment 按应用唯一，旧采集器的命令身份（上报 release/generation）随之过期，
// 复用会让新 Pod 的数据带旧代际被控制面拒绝（重启应用后运行日志/指标停更的根因），
// 也不再来回轮询已易主的 Deployment。采集循环按观察间隔自跑 Observe，把
// 最新证据推给 runtimeSidecar；worker 重启后由部署命令的 journal 重放自然恢复。
package workerexec

import (
	"context"
	"errors"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type collectorHandle struct {
	cancel  context.CancelFunc
	command *platformv1.WorkerDeploymentCommand
	change  kube.RuntimeChange
	http    bool
	emitter Emitter
}

func (e *Executor) initCollectors() {
	e.collectorsMu.Lock()
	defer e.collectorsMu.Unlock()
	if e.collectors == nil {
		e.collectors = map[string]*collectorHandle{}
	}
}

// collectorKey 按应用唯一标识采集器：运行 Deployment/Service 都按应用组织，
// 同一应用同时最多只有一个运行中的发布。
func collectorKey(workspaceID, applicationID string) string {
	return workspaceID + "|" + applicationID
}

// startRuntimeCollector 按应用幂等启动持久采集器：同一应用同一 release+generation
// 再次下发直接复用既有采集器，不叠加并发循环；release 或 generation 变化时
// 先取消旧采集器再以新命令身份启动。
func (e *Executor) startRuntimeCollector(command *platformv1.WorkerDeploymentCommand, change kube.RuntimeChange, http bool, emitter Emitter) error {
	if command == nil || emitter == nil {
		return errors.New("workerexec: 持久采集器缺少命令或发射器")
	}
	key := collectorKey(change.Release.WorkspaceID, change.Release.ApplicationID)
	e.collectorsMu.Lock()
	if e.collectors == nil {
		e.collectors = map[string]*collectorHandle{}
	}
	var stale *collectorHandle
	if handle, exists := e.collectors[key]; exists {
		if handle.change.Release.ID == change.Release.ID && handle.change.ExpectedGeneration == change.ExpectedGeneration {
			e.collectorsMu.Unlock()
			return nil
		}
		stale = handle
	}
	ctx, cancel := context.WithCancel(context.Background())
	handle := &collectorHandle{cancel: cancel, command: command, change: change, http: http, emitter: emitter}
	e.collectors[key] = handle
	e.collectorsMu.Unlock()
	if stale != nil {
		// release/generation 已变化：旧采集器携带的命令身份过期，立即取消让位。
		stale.cancel()
	}

	side := newRuntimeSidecar(e, command, change, emitter)
	side.start(ctx)
	e.wgCollectors.Add(1)
	go func() {
		defer e.wgCollectors.Done()
		defer cancel()
		ticker := time.NewTicker(e.options.ObserveInterval)
		defer ticker.Stop()
		for {
			// 独立观察循环：把最新证据推给旁路；部署消失即自停。
			evidence, err := e.runtime.Observe(ctx, change, http)
			if err == nil {
				side.update(evidence)
			}
			if err != nil && apierrors.IsNotFound(err) {
				e.removeCollector(key, handle)
				return
			}
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

// stopRuntimeCollector 停止并回收应用对应的采集器；不存在时为幂等空操作。
func (e *Executor) stopRuntimeCollector(workspaceID, applicationID string) {
	e.stopCollectorByKey(collectorKey(workspaceID, applicationID))
}

func (e *Executor) stopCollectorByKey(key string) {
	e.collectorsMu.Lock()
	handle, ok := e.collectors[key]
	if ok {
		delete(e.collectors, key)
	}
	e.collectorsMu.Unlock()
	if ok {
		handle.cancel()
	}
}

// removeCollector 只在注册表仍指向该 handle 时移除并取消：采集循环被新
// release/generation 替换后迟到的 NotFound 自停，不得误删误杀新采集器。
func (e *Executor) removeCollector(key string, handle *collectorHandle) {
	e.collectorsMu.Lock()
	current, ok := e.collectors[key]
	if ok && current != handle {
		ok = false
	} else if ok {
		delete(e.collectors, key)
	}
	e.collectorsMu.Unlock()
	if ok {
		handle.cancel()
	}
}
