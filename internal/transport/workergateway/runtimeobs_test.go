package workergateway

import (
	"context"
	"sync"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
)

type fakeRuntimeBackend struct {
	mu      sync.Mutex
	logs    []deployment.RuntimeLogInput
	metrics []deployment.RuntimeMetricInput
	failSeq bool
}

func (f *fakeRuntimeBackend) ClaimPending(context.Context, deployment.WorkerIdentity) (deployment.Dispatch, error) {
	return deployment.Dispatch{}, deployment.ErrNoPending
}
func (f *fakeRuntimeBackend) ApplyObservation(_ context.Context, _ deployment.WorkerIdentity, value deployment.Observation) (deployment.Ack, error) {
	return deployment.Ack{Seq: value.Seq}, nil
}
func (f *fakeRuntimeBackend) ApplyRuntimeLog(_ context.Context, _ deployment.WorkerIdentity, value deployment.RuntimeLogInput) (deployment.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, value)
	if f.failSeq {
		return deployment.Ack{}, deployment.ErrSequence
	}
	return deployment.Ack{Seq: value.Seq}, nil
}
func (f *fakeRuntimeBackend) ApplyRuntimeMetric(_ context.Context, _ deployment.WorkerIdentity, value deployment.RuntimeMetricInput) (deployment.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metrics = append(f.metrics, value)
	return deployment.Ack{Seq: value.CollectedAt.UnixMilli()}, nil
}

func TestGatewayRoutesRuntimeLogAndMetric(t *testing.T) {
	backend := &fakeRuntimeBackend{}
	env := setupTLS(t, func(config *Config) { config.Deployments = backend })
	stream, cancel := openStream(t, env, "node", "worker-1")
	defer cancel()
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeLog{RuntimeLog: &platformv1.WorkerRuntimeLog{WorkspaceId: "ws", ApplicationId: "app", OperationId: "op", ReleaseId: "rel", Generation: 1, PodUid: "pod-1", Container: "app", RestartCount: 2, Seq: 3, Content: []byte("hello")}}}); err != nil {
		t.Fatal(err)
	}
	collected := time.UnixMilli(1700000000000)
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeMetric{RuntimeMetric: &platformv1.WorkerRuntimeMetric{WorkspaceId: "ws", ApplicationId: "app", ReleaseId: "rel", Generation: 1, PodUid: "pod-1", CpuNanoCores: 5, CpuWindowMillis: 1000, MemoryWorkingSetBytes: 4096, CollectedUnixMilli: 1700000000000}}}); err != nil {
		t.Fatal(err)
	}
	first, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	id, err := workerprotocol.RuntimeLogStreamID(&platformv1.WorkerRuntimeLog{PodUid: "pod-1", Container: "app", RestartCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ack := first.GetAck(); ack == nil || ack.Channel != "runtime-log" || ack.Seq != 3 || ack.ExecutionId != id {
		t.Fatalf("运行日志ack错误: %+v 期望id=%s", first.GetAck(), id)
	}
	second, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if ack := second.GetAck(); ack == nil || ack.Channel != "runtime-metric" || ack.Seq != 1700000000000 || ack.ExecutionId != "pod-1" {
		t.Fatalf("采样ack错误: %+v", second.GetAck())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.logs) != 1 || string(backend.logs[0].Content) != "hello" || backend.logs[0].RestartCount != 2 || backend.logs[0].Generation != 1 {
		t.Fatalf("运行日志未正确路由: %+v", backend.logs)
	}
	if len(backend.metrics) != 1 || backend.metrics[0].CPUWindowMillis != 1000 || backend.metrics[0].CPUNanoCores != 5 || !backend.metrics[0].CollectedAt.Equal(collected.UTC()) {
		t.Fatalf("采样未正确路由: %+v", backend.metrics)
	}
}

func TestGatewayRejectsRuntimeLogSequenceConflict(t *testing.T) {
	backend := &fakeRuntimeBackend{failSeq: true}
	env := setupTLS(t, func(config *Config) { config.Deployments = backend })
	stream, cancel := openStream(t, env, "node", "worker-1")
	defer cancel()
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeLog{RuntimeLog: &platformv1.WorkerRuntimeLog{WorkspaceId: "ws", ApplicationId: "app", PodUid: "pod-1", Container: "app", Seq: 3, Content: []byte("hello")}}}); err != nil {
		t.Fatal(err)
	}
	message, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if rejection := message.GetRejection(); rejection == nil || rejection.Channel != "runtime-log" || rejection.Code != "SEQUENCE_CONFLICT" {
		t.Fatalf("序号冲突应拒绝: %+v", message)
	}
}
