package workerexec

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// deployCommand 构造控制面下发的部署命令；HTTP/WORKER 两种运行类型覆盖
// Service/Ingress 创建与纯缩容两条路径。
func deployCommand(action, operationID string, generation int64, http bool) *platformv1.WorkerDeploymentCommand {
	config := &platformv1.RuntimeConfig{WorkspaceId: testWorkspace, ApplicationId: testApplication, Revision: 1, Confirmed: true, Resources: &platformv1.RuntimeResources{CpuRequestMilli: 100, CpuLimitMilli: 500, MemoryRequestMib: 128, MemoryLimitMib: 512}}
	if http {
		config.Type = "HTTP"
		config.Port = 8080
		config.Probe = &platformv1.RuntimeProbe{Path: "/health", PeriodSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3}
		config.Domains = []string{"app.example.com"}
	} else {
		config.Type = "WORKER"
	}
	return &platformv1.WorkerDeploymentCommand{WorkspaceId: testWorkspace, ApplicationId: testApplication, OperationId: operationID, ReleaseId: "release-1", Action: action, Generation: generation, ClusterNodeUid: testNodeUID, Repository: "registry.example/app", Digest: testAppDigest, Platform: "linux/amd64", Config: config, MaterializedVariables: map[string]string{}, MaterializedFiles: map[string]string{}}
}

// completeHTTPReadiness 模拟Deployment控制器收敛：创建受管ReplicaSet、当前
// 发布的Pod、Service端点与状态，全部取自Apply产出的真实模板标签/注解。
func completeHTTPReadiness(t *testing.T, client *fake.Clientset) {
	t.Helper()
	ctx := context.Background()
	deploymentName := kube.AppName(testApplication)
	deployment, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replicaSet, err := client.AppsV1().ReplicaSets(testRuntimeNamespace).Create(ctx, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs-1", Namespace: testRuntimeNamespace, Labels: copyLabelsForTest(deployment.Spec.Template.Labels), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName, UID: deployment.UID, Controller: boolPtr(true)}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := client.CoreV1().Pods(testRuntimeNamespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: testRuntimeNamespace, Labels: copyLabelsForTest(deployment.Spec.Template.Labels), Annotations: copyLabelsForTest(deployment.Spec.Template.Annotations), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name, UID: replicaSet.UID, Controller: boolPtr(true)}}}, Spec: corev1.PodSpec{NodeName: testNodeName, Containers: []corev1.Container{{Name: "app", Image: deployment.Spec.Template.Spec.Containers[0].Image}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ImageID: "containerd://" + testAppDigest, RestartCount: 0}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := client.CoreV1().Services(testRuntimeNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DiscoveryV1().EndpointSlices(testRuntimeNamespace).Create(ctx, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slice-1", Namespace: testRuntimeNamespace, Labels: map[string]string{"kubernetes.io/service-name": deploymentName}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: deploymentName, UID: service.UID, Controller: boolPtr(true)}}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: testRuntimeNamespace, Name: pod.Name, UID: pod.UID}, Addresses: []string{"10.1.2.3"}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if _, err := client.AppsV1().Deployments(testRuntimeNamespace).UpdateStatus(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func copyLabelsForTest(source map[string]string) map[string]string {
	result := map[string]string{}
	for k, v := range source {
		result[k] = v
	}
	return result
}

func TestDeployHTTPCreatesDeploymentServiceAndObservation(t *testing.T) {
	executor, client, generation, emitter := newExecutorFixture(t)
	command := deployCommand("DEPLOY", "operation-1", 1, true)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	deploymentName := kube.AppName(testApplication)
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
		return err == nil
	})
	if generation.callCount() == 0 {
		t.Fatal("写入前必须调用generation守卫")
	}
	deployment, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatal("部署副本数应为1")
	}
	service, err := client.CoreV1().Services(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil || len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 80 {
		t.Fatalf("HTTP部署必须创建Service: %+v %v", service, err)
	}
	ingress, err := client.NetworkingV1().Ingresses(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil || len(ingress.Spec.Rules) != 1 || ingress.Spec.Rules[0].Host != "app.example.com" {
		t.Fatalf("HTTP部署必须创建入口: %+v %v", ingress, err)
	}
	completeHTTPReadiness(t, client)
	// Deploy 提前失败时观察永远不来；等待函数同时监视 done 并消费收尾。
	waitObservationOrDone(t, 2*time.Second, done, emitter, client)
	observations := emitter.observationsList()
	last := observations[len(observations)-1]
	if last.Kind != "OBSERVED" {
		t.Fatalf("终态观察应为OBSERVED: %+v", last)
	}
	if last.DeploymentUid != string(deployment.UID) || last.PodUid != "uid-pod-1" {
		t.Fatalf("UID必须来自运行证据: %+v", last)
	}
	if !last.CorrectGeneration || !last.OldPodsGone || !last.ServiceEndpoints || last.ReadyReplicas != 1 || last.Replicas != 1 {
		t.Fatalf("观察字段映射不正确: %+v", last)
	}
	if last.Digest != testAppDigest {
		t.Fatalf("digest只在镜像ID证据匹配时回传: %+v", last)
	}
	if last.WorkspaceId != testWorkspace || last.ApplicationId != testApplication || last.OperationId != "operation-1" || last.ReleaseId != "release-1" || last.Generation != 1 || last.ClusterNodeUid != testNodeUID {
		t.Fatalf("身份字段映射不正确: %+v", last)
	}
	if last.CollectedUnixMilli <= 0 || last.Seq != int64(len(observations)) {
		t.Fatalf("采集时间与序号必须由真实证据/发射器决定: %+v", last)
	}
}

func TestDeployStopOnlyScalesToZero(t *testing.T) {
	executor, client, generation, emitter := newExecutorFixture(t)
	ctx := context.Background()
	change, material, err := executor.runtimeChange(deployCommand("DEPLOY", "operation-1", 1, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.runtime.Apply(ctx, change, material, kube.Route{}); err != nil {
		t.Fatal(err)
	}
	stop := deployCommand("STOP", "operation-stop", 2, false)
	if err := executor.Deploy(ctx, stop, false, emitter); err != nil {
		t.Fatalf("STOP返回错误: %v", err)
	}
	deployment, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(ctx, kube.AppName(testApplication), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("STOP只应缩容为0: %+v", deployment.Spec.Replicas)
	}
	observations := emitter.observationsList()
	if len(observations) != 1 {
		t.Fatalf("停止应立即收敛为单条终态观察: %+v", observations)
	}
	last := observations[0]
	if last.Kind != "STOPPED" || last.Replicas != 0 || last.ReadyReplicas != 0 || !last.OldPodsGone || !last.CorrectGeneration || last.Generation != 2 || last.OperationId != "operation-stop" {
		t.Fatalf("停止观察字段不正确: %+v", last)
	}
	if last.DeploymentUid != string(deployment.UID) || last.CollectedUnixMilli <= 0 || last.Seq != 1 {
		t.Fatalf("停止观察证据不完整: %+v", last)
	}
	if generation.callCount() < 2 {
		t.Fatal("STOP写入与观察前都必须经过generation守卫")
	}
}

func TestDeployRejectsSupersededGeneration(t *testing.T) {
	executor, client, generation, emitter := newExecutorFixture(t)
	generation.fail = errors.New("已被新generation替代")
	if err := executor.Deploy(context.Background(), deployCommand("DEPLOY", "operation-1", 1, false), false, emitter); err == nil {
		t.Fatal("守卫失败必须中止写入")
	}
	if _, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{}); err == nil {
		t.Fatal("守卫失败后不得创建Deployment")
	}
	if len(emitter.observationsList()) != 0 {
		t.Fatal("失败执行不得发射观察")
	}
}

func TestDeployRejectsForeignNode(t *testing.T) {
	executor, client, generation, emitter := newExecutorFixture(t)
	command := deployCommand("DEPLOY", "operation-1", 1, false)
	command.ClusterNodeUid = "other-node-uid"
	if err := executor.Deploy(context.Background(), command, false, emitter); err == nil {
		t.Fatal("跨节点部署必须拒绝")
	}
	if generation.callCount() != 0 {
		t.Fatal("拒绝路径不得触发守卫写入")
	}
	if _, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), kube.AppName(testApplication), metav1.GetOptions{}); err == nil {
		t.Fatal("拒绝路径不得创建资源")
	}
}

func TestDeployRedeliveryDoesNotDuplicateTemplates(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	command := deployCommand("DEPLOY", "operation-1", 1, true)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, true, emitter) }()
	deploymentName := kube.AppName(testApplication)
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
		return err == nil
	})
	completeHTTPReadiness(t, client)
	waitObservationOrDone(t, 2*time.Second, done, emitter, client)
	before := len(emitter.observationsList())
	// RECONCILE重投：kube适配器幂等语义下复用同一组模板，不重复创建。
	// fake的Update会整体替换对象并清空status（真实API不会），这里由后台
	// 协程模拟控制器持续维持status收敛，直到重投执行结束。
	keepReady := make(chan struct{})
	go func() {
		for {
			select {
			case <-keepReady:
				return
			default:
			}
			deployment, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
			if err == nil {
				deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
				_, _ = client.AppsV1().Deployments(testRuntimeNamespace).UpdateStatus(context.Background(), deployment, metav1.UpdateOptions{})
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	client.ClearActions()
	done = make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, true, emitter) }()
	waitFor(t, 2*time.Second, func() bool { return len(emitter.observationsList()) > before })
	if err := <-done; err != nil {
		close(keepReady)
		t.Fatalf("重投Deploy失败: %v", err)
	}
	close(keepReady)
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			t.Fatalf("重投不得重复创建模板: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
	deployments, err := client.AppsV1().Deployments(testRuntimeNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(deployments.Items) != 1 {
		t.Fatalf("Deployment必须保持唯一: %d %v", len(deployments.Items), err)
	}
	services, err := client.CoreV1().Services(testRuntimeNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(services.Items) != 1 {
		t.Fatalf("Service必须保持唯一: %d %v", len(services.Items), err)
	}
	observations := emitter.observationsList()
	last := observations[len(observations)-1]
	if last.Kind != "OBSERVED" || last.Seq != int64(len(observations)) {
		t.Fatalf("重投也应收敛为OBSERVED且序号连续: %+v", last)
	}
}

// 控制面下发 https+tls_secret_name 时，入口必须携带 Ingress.Spec.TLS；
// Secret 名与域名为控制面汇聚后的当下绑定，worker 只如实装配。
func TestDeployTLSCommandWritesIngressTLS(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	command := deployCommand("DEPLOY", "operation-1", 1, true)
	command.Https = true
	command.TlsSecretName = "wildcard-example"
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	deploymentName := kube.AppName(testApplication)
	// Apply 按 Service→Ingress→Deployment 顺序落资源，先等 Deployment 避免窗口期。
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
		return err == nil
	})
	ingress, err := client.NetworkingV1().Ingresses(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ingress.Spec.TLS) != 1 || ingress.Spec.TLS[0].SecretName != "wildcard-example" || !slices.Contains(ingress.Spec.TLS[0].Hosts, "app.example.com") {
		t.Fatalf("TLS命令必须生成Ingress.Spec.TLS: %+v", ingress.Spec.TLS)
	}
	completeHTTPReadiness(t, client)
	waitObservationOrDone(t, 2*time.Second, done, emitter, client)
}

// 非 HTTPS 命令（存在未绑定或不一致域名）保持入口无 TLS，绝不猜测证书。
func TestDeployPlainCommandLeavesIngressWithoutTLS(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	command := deployCommand("DEPLOY", "operation-1", 1, true)
	done := make(chan error, 1)
	go func() { done <- executor.Deploy(context.Background(), command, false, emitter) }()
	deploymentName := kube.AppName(testApplication)
	// Apply 按 Service→Ingress→Deployment 顺序落资源，先等 Deployment 避免窗口期。
	waitFor(t, 2*time.Second, func() bool {
		_, err := client.AppsV1().Deployments(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
		return err == nil
	})
	ingress, err := client.NetworkingV1().Ingresses(testRuntimeNamespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ingress.Spec.TLS) != 0 {
		t.Fatalf("非HTTPS命令不得写入TLS: %+v", ingress.Spec.TLS)
	}
	completeHTTPReadiness(t, client)
	waitObservationOrDone(t, 2*time.Second, done, emitter, client)
}
