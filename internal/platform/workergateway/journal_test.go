package workergateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
)

func journalDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}
func TestJournalFsyncReplayAckAndConflictingSequence(t *testing.T) {
	directory := journalDirectory(t)
	journal, err := OpenJournal(directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	event := &platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildEvent{BuildEvent: &platformv1.WorkerBuildEvent{AttemptId: "attempt", Kind: "SUCCEEDED", Digest: "sha256:known"}}}
	if err := journal.append(event); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, "journal.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal权限=%v,%v", info, err)
	}
	restarted, err := OpenJournal(directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	pending, err := restarted.pending()
	if err != nil || len(pending) != 1 || pending[0].GetBuildEvent().Kind != "SUCCEEDED" || pending[0].GetBuildEvent().Seq != 1 {
		t.Fatalf("重启遗失成功=%#v,%v", pending, err)
	}
	if err := restarted.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildEvent{BuildEvent: &platformv1.WorkerBuildEvent{AttemptId: "attempt", Seq: 1, Kind: "FAILED"}}}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("相同序号不同内容=%v", err)
	}
	if err := restarted.ack(&platformv1.WorkerAck{Channel: "build-event", ExecutionId: "other", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	pending, _ = restarted.pending()
	if len(pending) != 1 {
		t.Fatal("跨任务ACK误删")
	}
	if err := restarted.ack(&platformv1.WorkerAck{Channel: "build-event", ExecutionId: "attempt", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	pending, _ = restarted.pending()
	if len(pending) != 0 {
		t.Fatal("ACK未清理")
	}
}
func TestJournalTruncatesLogsAndKeepsStateAndExecutionRecovery(t *testing.T) {
	journal, err := OpenJournal(journalDirectory(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	journal.state.LogBytes["attempt"] = maxAttemptLogBytes - 1
	if err := journal.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildLog{BuildLog: &platformv1.WorkerBuildLog{AttemptId: "attempt", Content: []byte("overflow")}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildLog{BuildLog: &platformv1.WorkerBuildLog{AttemptId: "attempt", Content: []byte("discarded")}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.append(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildEvent{BuildEvent: &platformv1.WorkerBuildEvent{AttemptId: "attempt", Kind: "SUCCEEDED"}}}); err != nil {
		t.Fatal(err)
	}
	pending, _ := journal.pending()
	if len(pending) != 2 || !pending[0].GetBuildLog().Truncated || pending[1].GetBuildEvent().Kind != "SUCCEEDED" {
		t.Fatalf("截断阻塞关键状态=%#v", pending)
	}
	seen, done, err := journal.begin("build:attempt", "stable")
	if err != nil || seen || done {
		t.Fatal(seen, done, err)
	}
	seen, done, err = journal.begin("build:attempt", "stable")
	if err != nil || !seen || done {
		t.Fatal(seen, done, err)
	}
	if _, _, err := journal.begin("build:attempt", "changed"); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if err := journal.generation("app", 2); err != nil {
		t.Fatal(err)
	}
	if err := journal.generation("app", 1); !errors.Is(err, ErrIdentity) {
		t.Fatal("旧generation未拒绝")
	}
}

// 控制面重投允许更换临时授权和已观察到的 Job UID，但不允许悄然改变冻结构建参数或发布配置。
func TestExecutionFingerprintsProtectImmutableSnapshotsWithoutSecrets(t *testing.T) {
	buildCommand := &platformv1.WorkerBuildCommand{AttemptId: "a", BuildId: "b", SourceSnapshotId: "source", Dockerfile: "Dockerfile", ContextPath: ".", BuildArgs: map[string]string{"MODE": "prod"}, SourceGrant: &platformv1.WorkerSourceGrant{Token: "secret-one"}, Secrets: []*platformv1.WorkerBuildSecret{{Name: "token", Value: []byte("private")}}}
	first, err := buildFingerprint(buildCommand)
	if err != nil {
		t.Fatal(err)
	}
	buildCommand.Action = "RECONCILE"
	buildCommand.JobUid = "job-uid"
	buildCommand.SourceGrant.Token = "secret-two"
	second, err := buildFingerprint(buildCommand)
	if err != nil || second != first {
		t.Fatalf("可变授权导致误拒绝: %q %q %v", first, second, err)
	}
	buildCommand.BuildArgs["MODE"] = "changed"
	second, _ = buildFingerprint(buildCommand)
	if second == first {
		t.Fatal("构建参数变化未检测")
	}
	deployment := &platformv1.WorkerDeploymentCommand{OperationId: "op", Generation: 1, Action: "DEPLOY", Config: &platformv1.RuntimeConfig{Revision: 1, Variables: []*platformv1.RuntimeVariable{{Name: "SECRET", Sensitive: true}}}, MaterializedVariables: map[string]string{"SECRET": "first"}}
	first, err = deploymentFingerprint(deployment)
	if err != nil {
		t.Fatal(err)
	}
	deployment.MaterializedVariables["SECRET"] = "second"
	second, _ = deploymentFingerprint(deployment)
	if second != first {
		t.Fatal("不应持久化敏感明文的摘要")
	}
	deployment.Config.Variables[0].Sensitive = false
	second, _ = deploymentFingerprint(deployment)
	if second == first {
		t.Fatal("敏感标志变化未检测")
	}
}

type stepExecutor struct{ calls atomic.Int64 }

func (e *stepExecutor) Build(_ context.Context, command *platformv1.WorkerBuildCommand, _ bool, out Emitter) error {
	if e.calls.Add(1) == 1 {
		return out.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "ACCEPTED", JobUid: "job"})
	}
	return out.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "SUCCEEDED", JobUid: "job"})
}
func (e *stepExecutor) Deploy(context.Context, *platformv1.WorkerDeploymentCommand, bool, Emitter) error {
	return nil
}

func TestExecutorCompletionRequiresTerminalEvidence(t *testing.T) {
	journal, err := OpenJournal(journalDirectory(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	executor := &stepExecutor{}
	client := &WorkerClient{config: WorkerConfig{NodeID: "node", WorkerID: "worker", Journal: journal, Executor: executor, MaxExecutions: 4}, active: map[string]bool{}}
	command := &platformv1.WorkerBuildCommand{BuildId: "build", AttemptId: "attempt", NodeId: "node", WorkerId: "worker", Action: "RUN", StableJobName: "job"}
	wait := func() {
		waitUntil(t, func() bool { client.mu.Lock(); defer client.mu.Unlock(); return len(client.active) == 0 })
	}
	if err := client.startBuild(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	wait()
	if err := client.startBuild(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	wait()
	if err := client.startBuild(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	wait()
	if executor.calls.Load() != 2 {
		t.Fatalf("中间状态阻塞重试或终态被重复执行: %d", executor.calls.Load())
	}
}

type independentExecutor struct {
	started   chan struct{}
	release   chan struct{}
	canceled  atomic.Bool
	calls     atomic.Int64
	reconcile atomic.Bool
}

func (e *independentExecutor) Build(ctx context.Context, value *platformv1.WorkerBuildCommand, reconcile bool, emitter Emitter) error {
	e.calls.Add(1)
	e.reconcile.Store(reconcile)
	if value.Action == "CANCEL" {
		return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: value.AttemptId, Kind: "STOPPED"})
	}
	select {
	case e.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		e.canceled.Store(true)
		return ctx.Err()
	case <-e.release:
		return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: value.AttemptId, Kind: "SUCCEEDED", LogsComplete: true})
	}
}
func (e *independentExecutor) Deploy(context.Context, *platformv1.WorkerDeploymentCommand, bool, Emitter) error {
	return nil
}
func TestExecutionManagerAcceptsCancelWhileRunIsActiveAndReconcilesJournal(t *testing.T) {
	journal, err := OpenJournal(journalDirectory(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	executor := &independentExecutor{started: make(chan struct{}, 1), release: make(chan struct{})}
	client := &WorkerClient{config: WorkerConfig{NodeID: "node", WorkerID: "worker", Journal: journal, Executor: executor, MaxExecutions: 4}, active: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := &platformv1.WorkerBuildCommand{BuildId: "build", AttemptId: "attempt", NodeId: "node", WorkerId: "worker", Action: "RUN", StableJobName: "stable-job"}
	if err := client.startBuild(ctx, command); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("执行未异步启动")
	}
	if err := client.startBuild(ctx, command); err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 1 {
		t.Fatal("重投重复执行")
	}
	cancelCommand := *command
	cancelCommand.Action = "CANCEL"
	if err := client.startBuild(ctx, &cancelCommand); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return executor.calls.Load() == 2 })
	if executor.canceled.Load() {
		t.Fatal("取消命令不应通过杀执行ctx伪造停止")
	}
	close(executor.release)
	waitUntil(t, func() bool { client.mu.Lock(); defer client.mu.Unlock(); return len(client.active) == 0 })
	pending, _ := journal.pending()
	if len(pending) != 2 {
		t.Fatalf("取消/成功关键状态未持久化=%d", len(pending))
	}
}
