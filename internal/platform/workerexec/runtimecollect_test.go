package workerexec

// E 期运行日志跟随与指标采集的旁路测试：全部使用 fake clientset 与本地 TLS 回环，
// 不访问真实 k3s/Kubelet/控制面。验证目标：分块与流内序号连续、restart 变化开新流、
// 日志流失败不影响 OBSERVED 收敛、MetricsEnabled=false 零发射、首样本跳过且第二次
// 采样发射差值字段、Deploy 返回后无 goroutine 泄漏。

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// sidecarExecutor 构造带旁路参数的执行器：观察间隔取校验下限 100ms，
// LogChunkBytes 缩小为 4 以便用固定内容断言分块与流内序号。
func sidecarExecutor(t *testing.T, adjust func(*Options)) (*Executor, *fake.Clientset, *stubGeneration, *recordingEmitter) {
	t.Helper()
	adapter, client := newKubeFixture(t)
	generation := &stubGeneration{}
	options := Options{Adapter: adapter, Client: client, Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, RuntimeNamespace: testRuntimeNamespace, Generation: generation, ObserveInterval: 100 * time.Millisecond, LogChunkBytes: 4}
	if adjust != nil {
		adjust(&options)
	}
	executor, err := NewExecutor(options)
	if err != nil {
		t.Fatal(err)
	}
	return executor, client, generation, newRecordingEmitter()
}

// startRuntimePod 模拟Deployment控制器为当前发布创建受管Pod；ready 控制PodReady
// 条件，restarts 控制app容器重启次数，标签/注解与 Apply 产出的模板完全一致。
func startRuntimePod(t *testing.T, client *fake.Clientset, name string, ready bool, restarts int32) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	deployment, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(ctx, kube.AppName(testApplication), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replicaSet, err := client.AppsV1().ReplicaSets(testRuntimeNamespace).Create(ctx, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name + "-rs", Namespace: testRuntimeNamespace, Labels: copyLabelsForTest(deployment.Spec.Template.Labels), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: kube.AppName(testApplication), UID: deployment.UID, Controller: boolPtr(true)}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var conditions []corev1.PodCondition
	if ready {
		conditions = append(conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
	}
	pod, err := client.CoreV1().Pods(testRuntimeNamespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testRuntimeNamespace, Labels: copyLabelsForTest(deployment.Spec.Template.Labels), Annotations: copyLabelsForTest(deployment.Spec.Template.Annotations), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name, UID: replicaSet.UID, Controller: boolPtr(true)}}}, Spec: corev1.PodSpec{NodeName: testNodeName, Containers: []corev1.Container{{Name: "app", Image: deployment.Spec.Template.Spec.Containers[0].Image}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: conditions, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ImageID: "containerd://" + testAppDigest, RestartCount: restarts, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return pod
}

func setPodRestarts(t *testing.T, client *fake.Clientset, name string, restarts int32) {
	t.Helper()
	pod, err := client.CoreV1().Pods(testRuntimeNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].RestartCount = restarts
	}
	if _, err := client.CoreV1().Pods(testRuntimeNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func setPodReady(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	pod, err := client.CoreV1().Pods(testRuntimeNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
	if _, err := client.CoreV1().Pods(testRuntimeNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// completeWorkerGate 补齐WORKER部署的Deployment状态证据，让观察收敛为OBSERVED。
func completeWorkerGate(t *testing.T, client *fake.Clientset) {
	t.Helper()
	deployment, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if _, err := client.AppsV1().Deployments(testRuntimeNamespace).UpdateStatus(context.Background(), deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// completeHTTPGate 为HTTP部署补齐Service端点与Deployment状态；pod 须为当前发布
// 的就绪 Pod，端点唯一匹配后 ServiceEndpoints 证据成立，观察收敛为OBSERVED。
func completeHTTPGate(t *testing.T, client *fake.Clientset, pod *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	deploymentName := kube.AppName(testApplication)
	service, err := client.CoreV1().Services(testRuntimeNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DiscoveryV1().EndpointSlices(testRuntimeNamespace).Create(ctx, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slice-sidecar", Namespace: testRuntimeNamespace, Labels: map[string]string{"kubernetes.io/service-name": deploymentName}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: deploymentName, UID: service.UID, Controller: boolPtr(true)}}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: testRuntimeNamespace, Name: pod.Name, UID: pod.UID}, Addresses: []string{"10.1.2.4"}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	completeWorkerGate(t, client)
}

// sidecarMetricText 伪造本节点 kubelet /metrics/resource 的 Prometheus 文本，
// 指标身份限定为运行命名空间内指定 Pod 的 app 容器。
func sidecarMetricText(namespace, pod string, cpu, start, memory float64) []byte {
	return []byte(fmt.Sprintf(`# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{namespace=%q,pod=%q,container="app"} %g
# TYPE container_start_time_seconds gauge
container_start_time_seconds{namespace=%q,pod=%q,container="app"} %g
# TYPE container_memory_working_set_bytes gauge
container_memory_working_set_bytes{namespace=%q,pod=%q,container="app"} %g
`, namespace, pod, cpu, namespace, pod, start, namespace, pod, memory))
}

// runtimeLogCounts 把已发射日志块按 restart 流分组，便于断言两个流的内容与序号。
func runtimeLogCounts(logs []*platformv1.WorkerRuntimeLog) map[int32]int {
	counts := map[int32]int{}
	for _, value := range logs {
		counts[value.RestartCount]++
	}
	return counts
}

func TestRuntimeLogFollowsChunksAndRestarts(t *testing.T) {
	executor, client, _, emitter := sidecarExecutor(t, nil)
	command := deployCommand("DEPLOY", "operation-1", 1, false)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{})
		return err == nil
	})
	// 初始：存活但未就绪、restart=0；旁路应开始跟随该容器实例并按块发射。
	startRuntimePod(t, client, "pod-1", false, 0)
	waitFor(t, 2*time.Second, func() bool { return len(emitter.runtimeLogsList()) > 0 })
	// Pod 重启（restart_count 0→1）后必须开新流。14字节内容按4字节分块：
	// 每流3个满块+1个尾块；等待两个流都完整发射，避免 OBSERVED 收敛丢弃
	// 仍在通道中的尾块（切换流后旧流残块仍按旧身份顺序发射）。
	setPodRestarts(t, client, "pod-1", 1)
	waitFor(t, 3*time.Second, func() bool {
		counts := runtimeLogCounts(emitter.runtimeLogsList())
		return counts[0] >= 4 && counts[1] >= 4
	})
	setPodReady(t, client, "pod-1")
	completeWorkerGate(t, client)
	if err := <-done; err != nil {
		t.Fatalf("Deploy返回错误: %v", err)
	}
	logs := emitter.runtimeLogsList()
	if len(logs) == 0 {
		t.Fatal("旁路必须发射运行日志块")
	}
	contents := map[int32][]byte{}
	sequences := map[int32]int64{}
	for _, value := range logs {
		if value.WorkspaceId != testWorkspace || value.ApplicationId != testApplication || value.OperationId != "operation-1" || value.ReleaseId != "release-1" || value.Generation != 1 {
			t.Fatalf("身份字段必须取当前命令: %+v", value)
		}
		if value.PodUid != "uid-pod-1" || value.Container != "app" || value.Truncated {
			t.Fatalf("流身份必须来自运行证据且未自造截断: %+v", value)
		}
		if len(value.Content) == 0 || len(value.Content) > 4 {
			t.Fatalf("块大小不得超过LogChunkBytes: %d", len(value.Content))
		}
		if expected := sequences[value.RestartCount] + 1; value.Seq != expected {
			t.Fatalf("流内序号必须连续: restart=%d seq=%d expected=%d", value.RestartCount, value.Seq, expected)
		}
		sequences[value.RestartCount] = value.Seq
		contents[value.RestartCount] = append(contents[value.RestartCount], value.Content...)
	}
	if string(contents[0]) != "build log line" || string(contents[1]) != "build log line" {
		t.Fatalf("两个流都应完整排空固定内容: %q %q", contents[0], contents[1])
	}
	if sequences[0] < 4 || sequences[1] < 4 {
		t.Fatalf("重启前后必须各成一条流且均有分块: %d %d", sequences[0], sequences[1])
	}
	observations := emitter.observationsList()
	if last := observations[len(observations)-1]; last.Kind != "OBSERVED" {
		t.Fatalf("日志跟随不得影响状态机收敛: %+v", last)
	}
}

func TestRuntimeLogFailureDoesNotBlockObservation(t *testing.T) {
	executor, client, _, emitter := sidecarExecutor(t, func(options *Options) {
		// 闭包先于返回值构造，从已冻结的 Options 里取回 fake clientset 注入故障。
		fakeClient := options.Client.(*fake.Clientset)
		fakeClient.PrependReactor("get", "pods", func(action ktesting.Action) (bool, kruntime.Object, error) {
			if action.GetSubresource() == "log" {
				return true, nil, errors.New("日志流故障")
			}
			return false, nil, nil
		})
	})
	command := deployCommand("DEPLOY", "operation-1", 1, false)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{})
		return err == nil
	})
	// 存活但未就绪：旁路每轮尝试跟随都会失败，观察循环必须照常轮询。
	startRuntimePod(t, client, "pod-1", false, 0)
	waitFor(t, 2*time.Second, func() bool { return len(emitter.observationsList()) >= 2 })
	setPodReady(t, client, "pod-1")
	completeWorkerGate(t, client)
	if err := <-done; err != nil {
		t.Fatalf("日志流失败不得影响Deploy收敛: %v", err)
	}
	observations := emitter.observationsList()
	if last := observations[len(observations)-1]; last.Kind != "OBSERVED" {
		t.Fatalf("终态观察应为OBSERVED: %+v", last)
	}
	if logs := emitter.runtimeLogsList(); len(logs) != 0 {
		t.Fatalf("打开失败的日志流不得发射日志块: %+v", logs)
	}
}

func TestMetricsDisabledByDefaultNoEmission(t *testing.T) {
	executor, client, _, emitter := sidecarExecutor(t, nil)
	command := deployCommand("DEPLOY", "operation-1", 1, false)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{})
		return err == nil
	})
	startRuntimePod(t, client, "pod-1", true, 0)
	completeWorkerGate(t, client)
	if err := <-done; err != nil {
		t.Fatalf("Deploy返回错误: %v", err)
	}
	if metrics := emitter.runtimeMetricsList(); len(metrics) != 0 {
		t.Fatalf("MetricsEnabled=false 绝不发射指标: %+v", metrics)
	}
}

func TestMetricsSkipsFirstSampleAndEmitsDelta(t *testing.T) {
	var scrapes int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics/resource" {
			t.Errorf("Kubelet端点路径不正确: %s", r.URL.Path)
		}
		// 每次采集counter增加10秒：差值字段应反映真实的10 CPU秒/窗口。
		cpu := 100 + 10*float64(atomic.AddInt32(&scrapes, 1))
		_, _ = w.Write(sidecarMetricText(testRuntimeNamespace, "pod-1", cpu, 10, 2048))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	executor, client, _, emitter := sidecarExecutor(t, func(options *Options) {
		options.MetricsEnabled = true
		options.KubeletMetrics = &kube.MetricsConfig{Endpoint: server.URL + "/metrics/resource", CAData: ca, BearerToken: "restricted"}
	})
	command := deployCommand("DEPLOY", "operation-1", 1, true)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{})
		return err == nil
	})
	// Pod就绪但HTTP端点未补齐：观察停在WAITING_READY，指标按观察间隔采样。
	pod := startRuntimePod(t, client, "pod-1", true, 0)
	waitFor(t, 3*time.Second, func() bool { return len(emitter.runtimeMetricsList()) >= 2 })
	completeHTTPGate(t, client, pod)
	if err := <-done; err != nil {
		t.Fatalf("Deploy返回错误: %v", err)
	}
	metrics := emitter.runtimeMetricsList()
	if len(metrics) < 2 {
		t.Fatalf("至少应发射两次采样: %d", len(metrics))
	}
	// 首样本跳过：采集次数必须多于发射次数。
	if int(atomic.LoadInt32(&scrapes)) <= len(metrics) {
		t.Fatalf("首样本必须跳过: scrapes=%d metrics=%d", atomic.LoadInt32(&scrapes), len(metrics))
	}
	for _, value := range metrics {
		if value.WorkspaceId != testWorkspace || value.ApplicationId != testApplication || value.ReleaseId != "release-1" || value.Generation != 1 || value.PodUid != "uid-pod-1" {
			t.Fatalf("指标身份字段不正确: %+v", value)
		}
		if value.CollectedUnixMilli <= 0 || value.CpuWindowMillis <= 0 || value.CpuNanoCores <= 0 || value.MemoryWorkingSetBytes != 2048 {
			t.Fatalf("差值字段必须来自真实窗口: %+v", value)
		}
	}
	for i := 1; i < len(metrics); i++ {
		if metrics[i].CollectedUnixMilli <= metrics[i-1].CollectedUnixMilli {
			t.Fatalf("同一Pod的采集毫秒必须严格递增: %+v", metrics[i-1:i+1])
		}
	}
	observations := emitter.observationsList()
	if last := observations[len(observations)-1]; last.Kind != "OBSERVED" {
		t.Fatalf("指标采样不得影响状态机收敛: %+v", last)
	}
}

func TestDeployCancelStopsSidecarGoroutines(t *testing.T) {
	executor, client, _, emitter := sidecarExecutor(t, nil)
	command := deployCommand("DEPLOY", "operation-1", 1, false)
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(ctx, command, false, emitter) }()
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{})
		return err == nil
	})
	startRuntimePod(t, client, "pod-1", false, 0)
	waitFor(t, 2*time.Second, func() bool { return len(emitter.observationsList()) >= 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx取消应让Deploy返回取消错误: %v", err)
	}
	// 新生命周期：采集器挂部署本体，Deploy 返回（甚至 ctx 取消）后采集继续；
	// 显式停止（部署消失/STOP 语义）后 goroutine 才全部退出。
	waitFor(t, 2*time.Second, func() bool { return runtime.NumGoroutine() > before })
	executor.stopRuntimeCollector("release-1")
	waitFor(t, 2*time.Second, func() bool { return runtime.NumGoroutine() <= before })
}
