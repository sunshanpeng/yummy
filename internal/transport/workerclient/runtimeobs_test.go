package workerclient

import (
	"errors"
	"testing"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
)

func TestJournalRuntimeLogTruncationDrainsAfterAck(t *testing.T) {
	journal, err := OpenJournal(journalDirectory(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	stream := &platformv1.WorkerRuntimeLog{PodUid: "pod-1", Container: "app", RestartCount: 2}
	id, err := workerprotocol.RuntimeLogStreamID(stream)
	if err != nil {
		t.Fatal(err)
	}
	journal.state.LogBytes[id] = maxRuntimeLogStreamBytes - 1
	if err := journal.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeLog{RuntimeLog: &platformv1.WorkerRuntimeLog{PodUid: "pod-1", Container: "app", RestartCount: 2, Content: []byte("overflow")}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeLog{RuntimeLog: &platformv1.WorkerRuntimeLog{PodUid: "pod-1", Container: "app", RestartCount: 2, Content: []byte("discarded")}}}); err != nil {
		t.Fatal("截断后追加应被丢弃而不是报错:", err)
	}
	pending, err := journal.pending()
	if err != nil || len(pending) != 1 || !pending[0].GetRuntimeLog().Truncated || pending[0].GetRuntimeLog().Seq != 1 {
		t.Fatalf("截断语义与构建日志不一致: %#v %v", pending, err)
	}
	if err := journal.ack(&platformv1.WorkerAck{Channel: "runtime-log", ExecutionId: id, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	pending, _ = journal.pending()
	if len(pending) != 0 {
		t.Fatal("ACK未清理")
	}
	// 流预算记账已随 ACK 清零：截断不应当永久封锁长活的运行日志流。
	if err := journal.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeLog{RuntimeLog: &platformv1.WorkerRuntimeLog{PodUid: "pod-1", Container: "app", RestartCount: 2, Content: []byte("fresh")}}}); err != nil {
		t.Fatal(err)
	}
	pending, _ = journal.pending()
	if len(pending) != 1 || pending[0].GetRuntimeLog().Truncated || string(pending[0].GetRuntimeLog().Content) != "fresh" {
		t.Fatalf("恢复后不应继续截断: %#v", pending)
	}
}

func TestJournalRuntimeMetricMillisecondSequence(t *testing.T) {
	journal, err := OpenJournal(journalDirectory(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	sample := func(millis int64, cpu int64) *platformv1.WorkerEnvelope {
		return &platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeMetric{RuntimeMetric: &platformv1.WorkerRuntimeMetric{PodUid: "pod-1", CpuNanoCores: cpu, CpuWindowMillis: 1000, CollectedUnixMilli: millis}}}
	}
	if err := journal.append(sample(1000, 1)); err != nil {
		t.Fatal(err)
	}
	if err := journal.append(sample(1000, 1)); err != nil {
		t.Fatal("相同采样重放应幂等:", err)
	}
	if err := journal.append(sample(1000, 2)); !errors.Is(err, workerprotocol.ErrProtocol) {
		t.Fatalf("同键不同值应冲突: %v", err)
	}
	if err := journal.append(sample(5000, 3)); err != nil {
		t.Fatal("采样间隔不固定，跳号应接受:", err)
	}
	if err := journal.append(sample(0, 4)); !errors.Is(err, workerprotocol.ErrProtocol) {
		t.Fatalf("缺采集时间应拒绝: %v", err)
	}
	pending, _ := journal.pending()
	if len(pending) != 2 || pending[0].GetRuntimeMetric().CollectedUnixMilli != 1000 || pending[1].GetRuntimeMetric().CollectedUnixMilli != 5000 {
		t.Fatalf("采样条目错误: %#v", pending)
	}
	if err := journal.ack(&platformv1.WorkerAck{Channel: "runtime-metric", ExecutionId: "pod-1", Seq: 1000}); err != nil {
		t.Fatal(err)
	}
	pending, _ = journal.pending()
	if len(pending) != 1 || pending[0].GetRuntimeMetric().CollectedUnixMilli != 5000 {
		t.Fatalf("按毫秒序号ACK失败: %#v", pending)
	}
}

func TestEmitterPersistsRuntimeObservability(t *testing.T) {
	journal, err := OpenJournal(journalDirectory(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	emitter := &journalEmitter{journal: journal}
	if err := emitter.RuntimeLog(&platformv1.WorkerRuntimeLog{PodUid: "pod-1", Container: "app", Content: []byte("line")}); err != nil {
		t.Fatal(err)
	}
	if err := emitter.RuntimeMetric(&platformv1.WorkerRuntimeMetric{PodUid: "pod-1", CollectedUnixMilli: 42, CpuNanoCores: 1, CpuWindowMillis: 1}); err != nil {
		t.Fatal(err)
	}
	if err := emitter.RuntimeMetric(nil); !errors.Is(err, workerprotocol.ErrProtocol) {
		t.Fatalf("空信封应拒绝: %v", err)
	}
	pending, err := journal.pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("持久化条目缺失: %d %v", len(pending), err)
	}
	if pending[0].GetRuntimeLog().Seq != 1 || string(pending[0].GetRuntimeLog().Content) != "line" {
		t.Fatalf("运行日志自动序号错误: %#v", pending[0])
	}
	if pending[1].GetRuntimeMetric().CollectedUnixMilli != 42 {
		t.Fatalf("采样条目错误: %#v", pending[1])
	}
}
