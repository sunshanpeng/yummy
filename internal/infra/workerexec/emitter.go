package workerexec

import platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"

// Emitter 是执行内核向控制面回传事件与日志的出口契约：本包是消费方，接口随消费方
// 定义；worker 网关的 journalEmitter/WorkerClient 提供实现，实现必须先持久化再发送，
// 重传路径才能保持幂等。
type Emitter interface {
	BuildEvent(*platformv1.WorkerBuildEvent) error
	BuildLog(*platformv1.WorkerBuildLog) error
	Observation(*platformv1.WorkerDeploymentObservation) error
	RuntimeLog(*platformv1.WorkerRuntimeLog) error
	RuntimeMetric(*platformv1.WorkerRuntimeMetric) error
}
