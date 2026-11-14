// 持久运行采集器注册表：运行日志/指标采集的生命周期挂部署本体而非操作跟踪窗口
// （发现⑨根因，2026-11-14 实机确认——部署快速收敛后跟踪窗口只剩数秒，旁路来不及
// 产出任何块）。Apply 成功即按 release 启动，部署消失（Observe NotFound）或显式
// STOP 时停止；同一 release 重复启动幂等。采集循环按观察间隔自跑 Observe，把
// 最新证据推给 runtimeSidecar；worker 重启后由部署命令的 journal 重放自然恢复。
package workerexec

import (
	"context"
	"errors"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/kube"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type collectorHandle struct {
	cancel  context.CancelFunc
	command *platformv1.WorkerDeploymentCommand
	change  kube.RuntimeChange
	http    bool
	emitter workergateway.Emitter
}

func (e *Executor) initCollectors() {
	e.collectorsMu.Lock()
	defer e.collectorsMu.Unlock()
	if e.collectors == nil {
		e.collectors = map[string]*collectorHandle{}
	}
}

// startRuntimeCollector 按 release 幂等启动持久采集器；同一 release 再次部署
// （如 RECONCILE 重放）直接复用既有采集器，不叠加并发循环。
func (e *Executor) startRuntimeCollector(command *platformv1.WorkerDeploymentCommand, change kube.RuntimeChange, http bool, emitter workergateway.Emitter) error {
	if command == nil || emitter == nil {
		return errors.New("workerexec: 持久采集器缺少命令或发射器")
	}
	key := change.Release.ID
	e.collectorsMu.Lock()
	if e.collectors == nil {
		e.collectors = map[string]*collectorHandle{}
	}
	if _, exists := e.collectors[key]; exists {
		e.collectorsMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	handle := &collectorHandle{cancel: cancel, command: command, change: change, http: http, emitter: emitter}
	e.collectors[key] = handle
	e.collectorsMu.Unlock()

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
				e.stopRuntimeCollector(key)
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

// stopRuntimeCollector 停止并回收 release 对应的采集器；不存在时为幂等空操作。
func (e *Executor) stopRuntimeCollector(releaseID string) {
	e.collectorsMu.Lock()
	handle, ok := e.collectors[releaseID]
	if ok {
		delete(e.collectors, releaseID)
	}
	e.collectorsMu.Unlock()
	if ok {
		handle.cancel()
	}
}
