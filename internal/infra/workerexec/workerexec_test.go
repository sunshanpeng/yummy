package workerexec

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// 全部测试使用fake clientset与本地httptest回环，不访问真实k3s/BuildKit/控制面。
const (
	testBuildNamespace   = "build"
	testRuntimeNamespace = "runtime"
	testNodeName         = "worker-node"
	testNodeUID          = "node-uid-1"
	testAttempt          = "attempt-1"
	testWorkspace        = "ws-1"
	testApplication      = "app-1"
)

var (
	testRunnerImage    = "registry.example/yummy-runner@sha256:" + strings.Repeat("a", 64)
	testArtifactDigest = "sha256:" + strings.Repeat("c", 64)
	testAppDigest      = "sha256:" + strings.Repeat("d", 64)
	testAppImage       = "registry.example/app@" + testAppDigest
)

func boolPtr(value bool) *bool { return &value }

// stubGeneration 记录守卫调用并可控失败，验证每次写入前都被调用。
type stubGeneration struct {
	mu    sync.Mutex
	calls int
	fail  error
}

func (g *stubGeneration) Check(context.Context, kube.RuntimeChange) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.fail
}

func (g *stubGeneration) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// recordingEmitter 模拟 workergateway.Journal 的序号语义：事件/日志/观察都以
// Seq=0 交给发射器并按执行身份自增，从而断言 Executor 不自造序号。
type recordingEmitter struct {
	mu             sync.Mutex
	events         []*platformv1.WorkerBuildEvent
	logs           []*platformv1.WorkerBuildLog
	observations   []*platformv1.WorkerDeploymentObservation
	runtimeLogs    []*platformv1.WorkerRuntimeLog
	runtimeMetrics []*platformv1.WorkerRuntimeMetric
	sequences      map[string]int64
}

func newRecordingEmitter() *recordingEmitter {
	return &recordingEmitter{sequences: map[string]int64{}}
}

func (e *recordingEmitter) next(channel, id string) int64 {
	e.sequences[channel+":"+id]++
	return e.sequences[channel+":"+id]
}

func (e *recordingEmitter) BuildEvent(value *platformv1.WorkerBuildEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	value.Seq = e.next("build-event", value.AttemptId)
	e.events = append(e.events, value)
	return nil
}

func (e *recordingEmitter) BuildLog(value *platformv1.WorkerBuildLog) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	value.Seq = e.next("build-log", value.AttemptId)
	e.logs = append(e.logs, value)
	return nil
}

func (e *recordingEmitter) Observation(value *platformv1.WorkerDeploymentObservation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	value.Seq = e.next("deployment", value.OperationId)
	e.observations = append(e.observations, value)
	return nil
}

// RuntimeLog/RuntimeMetric 为 D 期新增 Emitter 方法：运行日志按 (pod, container,
// restart) 流键模拟 journal 的 Seq 自增，指标按原样记录，供 E 期旁路断言。
func (e *recordingEmitter) RuntimeLog(value *platformv1.WorkerRuntimeLog) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := value.PodUid + "/" + value.Container + "/" + strconv.Itoa(int(value.RestartCount))
	value.Seq = e.next("runtime-log", key)
	e.runtimeLogs = append(e.runtimeLogs, value)
	return nil
}

func (e *recordingEmitter) RuntimeMetric(value *platformv1.WorkerRuntimeMetric) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runtimeMetrics = append(e.runtimeMetrics, value)
	return nil
}

func (e *recordingEmitter) buildEvents() []*platformv1.WorkerBuildEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*platformv1.WorkerBuildEvent(nil), e.events...)
}

func (e *recordingEmitter) buildLogs() []*platformv1.WorkerBuildLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*platformv1.WorkerBuildLog(nil), e.logs...)
}

func (e *recordingEmitter) observationsList() []*platformv1.WorkerDeploymentObservation {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*platformv1.WorkerDeploymentObservation(nil), e.observations...)
}

func (e *recordingEmitter) runtimeLogsList() []*platformv1.WorkerRuntimeLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*platformv1.WorkerRuntimeLog(nil), e.runtimeLogs...)
}

func (e *recordingEmitter) runtimeMetricsList() []*platformv1.WorkerRuntimeMetric {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*platformv1.WorkerRuntimeMetric(nil), e.runtimeMetrics...)
}

func newKubeFixture(t *testing.T) (*kube.Adapter, *fake.Clientset) {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName, UID: types.UID(testNodeUID)}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewClientset(node)
	// fake不运行admission/controller；UID只为适配逻辑测试显式模拟，不是集群验收。
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, kruntime.Object, error) {
		if object, ok := action.(ktesting.CreateAction).GetObject().(metav1.Object); ok {
			object.SetUID(types.UID("uid-" + object.GetName()))
		}
		return false, nil, nil
	})
	// fake日志流默认返回固定内容；这里替换为可断言的日志块。
	client.PrependReactor("get", "pods", func(action ktesting.Action) (bool, kruntime.Object, error) {
		if action.GetSubresource() == "log" {
			return true, &kruntime.Unknown{Raw: []byte("build log line")}, nil
		}
		return false, nil, nil
	})
	adapter, err := kube.New(client, kube.Config{Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, RuntimeNamespace: testRuntimeNamespace, BuildDeadline: time.Minute, StorageClass: "local-path", IngressClass: "traefik", Installed: kube.InstalledCapabilities{Build: true, Runtime: true, Ingress: true, Metrics: true, QualificationID: "test-only-not-real-qualification", RunnerImage: testRunnerImage, BuildKitImage: testRunnerImage}})
	if err != nil {
		t.Fatal(err)
	}
	return adapter, client
}

func newExecutorFixture(t *testing.T) (*Executor, *fake.Clientset, *stubGeneration, *recordingEmitter) {
	t.Helper()
	adapter, client := newKubeFixture(t)
	generation := &stubGeneration{}
	executor, err := NewExecutor(Options{Adapter: adapter, Client: client, Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, RuntimeNamespace: testRuntimeNamespace, Generation: generation, BuildPollInterval: 100 * time.Millisecond, ObserveInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return executor, client, generation, newRecordingEmitter()
}

func buildCommand(action string) *platformv1.WorkerBuildCommand {
	return &platformv1.WorkerBuildCommand{BuildId: "build-1", AttemptId: testAttempt, WorkspaceId: testWorkspace, ApplicationId: testApplication, NodeId: "node-1", WorkerId: "worker-1", Action: action, StableJobName: kube.JobName(testAttempt), Repository: "registry.example/app", Tag: "build-1", Platform: kube.Platform, Dockerfile: "Dockerfile", ContextPath: ".", SourceGrant: &platformv1.WorkerSourceGrant{Url: "https://source.example/grants/one", Token: "SOURCE-TOKEN", Sha256: "sha256:" + strings.Repeat("b", 64)}, Registry: &platformv1.WorkerRegistryCredential{Origin: "https://registry.example", Username: "user", Password: []byte("pass")}}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 超时时转储全部 goroutine，帮助定位偶发的调度或阻塞问题。
	buffer := make([]byte, 1<<20)
	stackSize := runtime.Stack(buffer, true)
	t.Fatalf("等待条件超时\n%s", buffer[:stackSize])
}

// waitObservationOrDone 等待首条观察或 Deploy 结束；Deploy 提前失败时以真实错误
// 失败，避免把"goroutine 已带错误退出"误报成观察等待超时。成功路径由本函数
// 等待 Deploy 收尾并消费 done，调用方不得再次读取 done。
func waitObservationOrDone(t *testing.T, timeout time.Duration, done <-chan error, emitter *recordingEmitter, client *fake.Clientset) {
	t.Helper()
	failDeploy := func(err error) {
		// 转储 fake 客户端操作序列，定位归属校验失败发生在哪一步。
		var actions strings.Builder
		for _, action := range client.Actions() {
			actions.WriteString(action.GetVerb() + "/" + action.GetResource().Resource + "/" + action.GetSubresource() + "\n")
		}
		t.Fatalf("Deploy在发射观察前失败: %v\n最近操作:\n%s", err, actions.String())
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err != nil {
				failDeploy(err)
			}
			return
		default:
		}
		if len(emitter.observationsList()) >= 1 {
			// 观察已到；observeLoop 发射终态后立即结束，等待其收尾。
			select {
			case err := <-done:
				if err != nil {
					failDeploy(err)
				}
				return
			case <-time.After(timeout):
				buffer := make([]byte, 1<<20)
				stackSize := runtime.Stack(buffer, true)
				t.Fatalf("观察已到但Deploy未收尾\n%s", buffer[:stackSize])
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	buffer := make([]byte, 1<<20)
	stackSize := runtime.Stack(buffer, true)
	t.Fatalf("等待观察超时\n%s", buffer[:stackSize])
}

// completeBuildEvidence 给已存在的Job补齐终止Pod（含termination metadata）与
// Complete状态，模拟kube控制器收敛后的真实证据形态。
func completeBuildEvidence(t *testing.T, client *fake.Clientset, jobName string) {
	t.Helper()
	ctx := context.Background()
	job, err := client.BatchV1().Jobs(testBuildNamespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-pod", Namespace: testBuildNamespace, Labels: map[string]string{kube.LabelManaged: "worker", kube.LabelWorkspace: testWorkspace, kube.LabelApplication: testApplication, kube.LabelNodeUID: testNodeUID, kube.LabelAttempt: testAttempt}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: job.UID, Controller: boolPtr(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "build", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"containerimage.digest":"` + testArtifactDigest + `"}`}}}}}}
	if _, err := client.CoreV1().Pods(testBuildNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.Active = 0
	if _, err := client.BatchV1().Jobs(testBuildNamespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRunCreatesJobWithSequencedEvents(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	command := buildCommand("RUN")
	done := make(chan error, 1)
	go func() { done <- executor.Build(context.Background(), command, false, emitter) }()
	jobName := kube.JobName(testAttempt)
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.BatchV1().Jobs(testBuildNamespace).Get(context.Background(), jobName, metav1.GetOptions{})
		return err == nil
	})
	waitFor(t, 2*time.Second, func() bool { return len(emitter.buildEvents()) >= 1 })
	events := emitter.buildEvents()
	if len(events) != 1 || events[0].Kind != "ACCEPTED" || events[0].Seq != 1 {
		t.Fatalf("首个事件应为ACCEPTED seq=1: %+v", events)
	}
	if events[0].JobUid != "uid-"+jobName {
		t.Fatalf("JobUID必须来自运行证据: %q", events[0].JobUid)
	}
	completeBuildEvidence(t, client, jobName)
	waitFor(t, 2*time.Second, func() bool { return len(emitter.buildEvents()) >= 2 })
	if err := <-done; err != nil {
		t.Fatalf("Build返回错误: %v", err)
	}
	events = emitter.buildEvents()
	if len(events) != 2 || events[1].Kind != "SUCCEEDED" || events[1].Digest != testArtifactDigest {
		t.Fatalf("终态事件应为SUCCEEDED且digest只来自termination metadata: %+v", events)
	}
	if events[1].JobUid != "uid-"+jobName || events[1].Seq != 2 || !events[1].LogsComplete {
		t.Fatalf("终态事件字段不完整: %+v", events[1])
	}
	logs := emitter.buildLogs()
	if len(logs) != 1 || string(logs[0].Content) != "build log line" || logs[0].Seq != 1 || logs[0].AttemptId != testAttempt {
		t.Fatalf("日志块应来自日志流并按序发射: %+v", logs)
	}
}

func TestBuildCancelDeletesJobAndEmitsStopped(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	ctx := context.Background()
	task, err := executor.buildTask(buildCommand("RUN"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := executor.options.Adapter.Run(ctx, task)
	if err != nil || !evidence.Found {
		t.Fatalf("预置Job失败: %v %+v", err, evidence)
	}
	cancel := buildCommand("CANCEL")
	cancel.JobUid = string(evidence.JobUID)
	if err := executor.Build(ctx, cancel, false, emitter); err != nil {
		t.Fatalf("取消返回错误: %v", err)
	}
	_, err = client.BatchV1().Jobs(testBuildNamespace).Get(ctx, kube.JobName(testAttempt), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Job应已删除: %v", err)
	}
	events := emitter.buildEvents()
	if len(events) != 1 || events[0].Kind != "STOPPED" || events[0].JobUid != string(evidence.JobUID) || events[0].Seq != 1 {
		t.Fatalf("取消只应发射一条STOPPED事件: %+v", events)
	}
}

func TestBuildReconcileRestoresWithoutRecreating(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	ctx := context.Background()
	task, err := executor.buildTask(buildCommand("RUN"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := executor.options.Adapter.Run(ctx, task)
	if err != nil || !evidence.Found {
		t.Fatalf("预置Job失败: %v %+v", err, evidence)
	}
	command := buildCommand("RECONCILE")
	command.JobUid = string(evidence.JobUID)
	client.ClearActions()
	if err := executor.Build(ctx, command, true, emitter); err != nil {
		t.Fatalf("RECONCILE返回错误: %v", err)
	}
	for _, action := range client.Actions() {
		// 命名空间引导的创建合法；不得重建的是构建 Job 本体。
		if action.GetVerb() == "create" && action.GetResource().Resource == "jobs" {
			t.Fatalf("RECONCILE按JobUID恢复，不得重建Job: %s", action.GetVerb())
		}
	}
	events := emitter.buildEvents()
	if len(events) != 1 || events[0].Kind != "RUNNING" || events[0].JobUid != string(evidence.JobUID) || events[0].Seq != 1 {
		t.Fatalf("运行中恢复只应报告RUNNING: %+v", events)
	}
}

func TestBuildReconcileReportsTerminalEvidence(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	ctx := context.Background()
	task, err := executor.buildTask(buildCommand("RUN"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := executor.options.Adapter.Run(ctx, task)
	if err != nil || !evidence.Found {
		t.Fatalf("预置Job失败: %v %+v", err, evidence)
	}
	completeBuildEvidence(t, client, kube.JobName(testAttempt))
	command := buildCommand("RECONCILE")
	command.JobUid = string(evidence.JobUID)
	if err := executor.Build(ctx, command, true, emitter); err != nil {
		t.Fatalf("RECONCILE返回错误: %v", err)
	}
	events := emitter.buildEvents()
	if len(events) != 1 || events[0].Kind != "SUCCEEDED" || events[0].Digest != testArtifactDigest || events[0].Seq != 1 {
		t.Fatalf("恢复时终态证据应如实上报: %+v", events)
	}
}

func TestBuildRunNeverRecreatesAcknowledgedJob(t *testing.T) {
	executor, _, _, emitter := newExecutorFixture(t)
	command := buildCommand("RUN")
	command.JobUid = "uid-vanished"
	err := executor.Build(context.Background(), command, true, emitter)
	if !errors.Is(err, kube.ErrJobLost) {
		t.Fatalf("已确认Job消失必须拒绝补跑: %v", err)
	}
	if len(emitter.buildEvents()) != 0 {
		t.Fatal("失败执行不得发射伪造事件")
	}
}

func TestBuildRejectsUnknownAction(t *testing.T) {
	executor, _, _, emitter := newExecutorFixture(t)
	command := buildCommand("RESTART")
	if err := executor.Build(context.Background(), command, false, emitter); err == nil {
		t.Fatal("未知动作必须被拒绝")
	}
}

func TestNewExecutorRequiresExplicitNamespaces(t *testing.T) {
	adapter, client := newKubeFixture(t)
	options := Options{Adapter: adapter, Client: client, Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, Generation: &stubGeneration{}}
	if _, err := NewExecutor(options); err == nil {
		t.Fatal("缺少运行命名空间必须拒绝")
	}
	options.RuntimeNamespace = testRuntimeNamespace
	if _, err := NewExecutor(options); err != nil {
		t.Fatalf("完整参数应可构造: %v", err)
	}
}

type stubGenerationClient struct {
	request *platformv1.WorkerGenerationRequest
}

func (c *stubGenerationClient) CheckGeneration(_ context.Context, request *platformv1.WorkerGenerationRequest) error {
	c.request = request
	return nil
}

func TestGenerationGuardForwardsRuntimeChange(t *testing.T) {
	client := &stubGenerationClient{}
	change := kube.RuntimeChange{Release: kube.Release{ID: "rel-1", WorkspaceID: testWorkspace, ApplicationID: testApplication, Placement: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}}, OperationID: "op-1", ExpectedGeneration: 3}
	if err := GenerationGuard(client).Check(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	request := client.request
	if request == nil || request.WorkspaceId != change.Release.WorkspaceID || request.ApplicationId != change.Release.ApplicationID || request.OperationId != change.OperationID || request.Generation != change.ExpectedGeneration || request.ReleaseId != change.Release.ID || request.ClusterNodeUid != string(change.Release.Placement.UID) {
		t.Fatalf("守卫字段翻译不完整: %+v", request)
	}
}
