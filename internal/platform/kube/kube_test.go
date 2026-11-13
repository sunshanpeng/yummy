package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

var ctx = context.Background()

const image = "registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func fixture(t *testing.T) (*Adapter, *fake.Clientset, BuildTask) {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewClientset(node)
	// fake不会运行API admission/controller；UID只为适配逻辑测试显式模拟，不是集群验收。
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, kruntime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject()
		if m, ok := object.(metav1.Object); ok {
			m.SetUID(types.UID("uid-" + m.GetName()))
		}
		return false, nil, nil
	})
	a, err := New(client, Config{Node: NodeTarget{"worker", "node-uid"}, BuildNamespace: "build", RuntimeNamespace: "runtime", BuildDeadline: time.Minute, StorageClass: "local-path", IngressClass: "traefik", Installed: InstalledCapabilities{Build: true, Runtime: true, Ingress: true, Metrics: true, QualificationID: "test-only-not-real-qualification", RunnerImage: image, BuildKitImage: image}})
	if err != nil {
		t.Fatal(err)
	}
	task := BuildTask{AttemptID: "attempt-1", WorkspaceID: "ws-1", ApplicationID: "app-1", Node: a.config.Node, Platform: Platform, Source: SourceGrant{URI: "https://source.example/grants/one", ArchiveSHA256: "sha256:" + strings.Repeat("b", 64), MaxBytes: 1024, Token: []byte("SOURCE-TOKEN")}, ImageTag: "registry.example/app:build-1", Dockerfile: "Dockerfile", Context: ".", RegistryDockerConfig: []byte(`{"auths":{"registry.example":{"auth":"REGISTRY-CREDENTIAL"}}}`), Secrets: []BuildSecret{{ID: "npm", Version: "v1", Value: []byte("BUILD-SECRET")}}, BuildArgs: map[string]string{"MODE": "value; not shell"}}
	task.JobName = JobName(task.AttemptID)
	task.StableJobName = task.JobName
	return a, client, task
}
func change(a *Adapter) RuntimeChange {
	return RuntimeChange{Release: Release{ID: "release-1", WorkspaceID: "ws-1", ApplicationID: "app-1", Image: image, ConfigRevision: 1, Placement: a.config.Node}, OperationID: "operation-1", ExpectedGeneration: 1}
}
func materialized(c RuntimeChange, http bool) runtimeconfig.Materialized {
	input := runtimeconfig.DefaultInput()
	if !http {
		input.Type = runtimeconfig.TypeWorker
		input.Port = 0
		input.Probe = runtimeconfig.Probe{}
	}
	return runtimeconfig.Materialized{View: runtimeconfig.View{WorkspaceID: c.Release.WorkspaceID, ApplicationID: c.Release.ApplicationID, Revision: 1, Confirmed: true, Type: input.Type, Port: input.Port, Probe: input.Probe, Resources: input.Resources, Command: []string{"/app/server"}, Args: []string{"literal; echo no-shell"}, Variables: []runtimeconfig.Variable{{Name: "PUBLIC", Value: "public", Configured: true}, {Name: "PASSWORD", Sensitive: true, Configured: true}}, Files: []runtimeconfig.ConfigFile{{Path: "/etc/app/config", Configured: true}, {Path: "/etc/app/token", Sensitive: true, Configured: true}}}, Variables: map[string]string{"PUBLIC": "public", "PASSWORD": "RUNTIME-SECRET"}, Files: map[string]string{"/etc/app/config": "ordinary", "/etc/app/token": "FILE-SECRET"}}
}

type guardFunc func(context.Context, RuntimeChange) error

func (f guardFunc) Check(ctx context.Context, c RuntimeChange) error { return f(ctx, c) }
func runtimeFixture(t *testing.T) (*Runtime, *fake.Clientset, RuntimeChange, runtimeconfig.Materialized) {
	a, client, _ := fixture(t)
	r, err := a.Runtime(guardFunc(func(context.Context, RuntimeChange) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	c := change(a)
	return r, client, c, materialized(c, false)
}
func assertRestricted(t *testing.T, p corev1.PodSpec) {
	t.Helper()
	if p.HostNetwork || p.HostPID || p.HostIPC || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.SecurityContext == nil || p.SecurityContext.RunAsNonRoot == nil || !*p.SecurityContext.RunAsNonRoot {
		t.Fatal("Pod隔离约束缺失")
	}
	for _, v := range p.Volumes {
		if v.HostPath != nil {
			t.Fatal("禁止hostPath")
		}
		if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Sign() <= 0) {
			t.Fatal("emptyDir必须有界")
		}
	}
	for _, c := range append(p.InitContainers, p.Containers...) {
		s := c.SecurityContext
		if s == nil || s.Privileged == nil || *s.Privileged || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || s.Capabilities == nil || len(s.Capabilities.Drop) != 1 || s.Capabilities.Drop[0] != "ALL" || s.AppArmorProfile.Type != corev1.AppArmorProfileTypeRuntimeDefault {
			t.Fatal("容器安全约束缺失")
		}
		if len(c.Resources.Limits) < 3 {
			t.Fatal("资源上限缺失")
		}
	}
}
func TestBuildRunSpecSecretsAndIdempotency(t *testing.T) {
	a, client, task := fixture(t)
	e, err := a.Run(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Found || e.JobUID == "" {
		t.Fatal(e)
	}
	job, err := client.BatchV1().Jobs("build").Get(ctx, task.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertRestricted(t, job.Spec.Template.Spec)
	if *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever || *job.Spec.ActiveDeadlineSeconds != 60 || job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("Job生命周期配置")
	}
	data, _ := json.Marshal(job)
	for _, secret := range []string{"SOURCE-TOKEN", "REGISTRY-CREDENTIAL", "BUILD-SECRET"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("秘密进入Pod对象")
		}
	}
	build := job.Spec.Template.Spec.Containers[0]
	if build.Command[0] != "/usr/local/bin/yummy-runner" || !strings.Contains(strings.Join(build.Args, "|"), "--secret=id=npm,src=/run/build-auth/secret-0") {
		t.Fatal(build.Args)
	}
	for _, mount := range build.VolumeMounts {
		if mount.Name == "source-auth" {
			t.Fatal("构建容器不得访问源码下载令牌")
		}
	}
	secret, err := client.CoreV1().Secrets("build").Get(ctx, task.JobName+"-auth", metav1.GetOptions{})
	if err != nil || string(secret.Data["source-token"]) != "SOURCE-TOKEN" || string(secret.Data["secret-0"]) != "BUILD-SECRET" {
		t.Fatal(secret, err)
	}
	client.ClearActions()
	task.JobUID = e.JobUID
	task.Acknowledged = true
	if _, err := a.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			t.Fatal("重投重复创建")
		}
	}
}
func TestBuildOwnershipLostAndUnknown(t *testing.T) {
	for _, mode := range []string{"foreign-app", "foreign-uid", "different-spec", "lost", "unknown", "query-token", "not-qualified"} {
		t.Run(mode, func(t *testing.T) {
			a, client, task := fixture(t)
			want, _ := a.buildObjects(task)
			want.UID = "original"
			want.Spec.Template.Spec.Containers[0].Image = image
			switch mode {
			case "foreign-app":
				want.Labels[LabelApplication] = "other"
				_ = client.Tracker().Add(want)
			case "foreign-uid":
				task.JobUID = "expected"
				_ = client.Tracker().Add(want)
			case "different-spec":
				want.Spec.Template.Spec.HostNetwork = true
				_ = client.Tracker().Add(want)
			case "lost":
				task.Acknowledged = true
			case "unknown":
				client.PrependReactor("get", "jobs", func(ktesting.Action) (bool, kruntime.Object, error) { return true, nil, errors.New("network unknown") })
			case "query-token":
				task.Source.URI += "?token=secret"
			case "not-qualified":
				a.config.Installed.RunnerImage = "runner:latest"
			}
			_, err := a.Run(ctx, task)
			if err == nil {
				t.Fatal("危险输入未拒绝")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" {
					t.Fatal("拒绝后不应创建")
				}
			}
		})
	}
}
func TestBuildAlreadyExistsAdoption(t *testing.T) {
	a, client, task := fixture(t)
	want, _ := a.buildObjects(task)
	want.UID = "adopted"
	client.PrependReactor("create", "jobs", func(ktesting.Action) (bool, kruntime.Object, error) {
		if err := client.Tracker().Add(want); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, task.JobName)
	})
	e, err := a.Run(ctx, task)
	if err != nil || e.JobUID != "adopted" {
		t.Fatal(e, err)
	}
}
func buildPod(task BuildTask, uid types.UID) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "build-pod", Namespace: "build", UID: "pod-uid", Labels: buildLabels(task), OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: task.JobName, UID: uid, Controller: ptr(true)}}}}
}
func TestBuildMetadataNotLogs(t *testing.T) {
	a, client, task := fixture(t)
	j, _ := a.buildObjects(task)
	j.UID = "job-uid"
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	p := buildPod(task, j.UID)
	task.JobUID, task.Acknowledged = j.UID, true
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "build", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "build succeeded"}}}}
	_ = client.Tracker().Add(j)
	_ = client.Tracker().Add(p)
	e, err := a.Reconcile(ctx, task)
	if err != nil || !e.Complete || e.Digest != "" || e.Reason == "" {
		t.Fatal(e, err)
	}
	p.Status.ContainerStatuses[0].State.Terminated.Message = `{"containerimage.digest":"sha256:` + strings.Repeat("a", 64) + `"}`
	_, _ = client.CoreV1().Pods("build").UpdateStatus(ctx, p, metav1.UpdateOptions{})
	e, err = a.Reconcile(ctx, task)
	if err != nil || e.Digest == "" {
		t.Fatal(e, err)
	}
}
func TestReconcileUsesPersistedUIDWithoutExpiringSecrets(t *testing.T) {
	a, client, task := fixture(t)
	original, err := a.Run(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	task.JobUID, task.Acknowledged = original.JobUID, true
	task.Source = SourceGrant{}
	task.RegistryDockerConfig, task.Secrets = nil, nil
	client.ClearActions()
	found, err := a.Reconcile(ctx, task)
	if err != nil || !found.Found || found.JobUID != original.JobUID {
		t.Fatalf("不带秘密恢复原任务失败: %+v %v", found, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "update" {
			t.Fatal("恢复查询不得创建或修改资源")
		}
	}
	task.JobUID = "wrong"
	if _, err := a.Reconcile(ctx, task); !errors.Is(err, ErrOwnership) {
		t.Fatalf("错 UID 恢复=%v", err)
	}
	task.JobUID = ""
	if _, err := a.Reconcile(ctx, task); !errors.Is(err, ErrOwnership) {
		t.Fatalf("无 UID 恢复=%v", err)
	}
}

func TestCancelConfirmsAbsenceNotAPIError(t *testing.T) {
	for _, mode := range []string{"delete-complete", "still-deleting", "unknown-get", "unknown-pods", "foreign-pod", "node-not-ready"} {
		t.Run(mode, func(t *testing.T) {
			a, client, task := fixture(t)
			j, _ := a.buildObjects(task)
			j.UID = "job-uid"
			task.JobUID = j.UID
			_ = client.Tracker().Add(j)
			p := buildPod(task, j.UID)
			_ = client.Tracker().Add(p)
			// 取消不再需要凭据，清空后也必须能停止。
			task.Source = SourceGrant{}
			task.RegistryDockerConfig = nil
			switch mode {
			case "still-deleting":
				client.PrependReactor("delete", "jobs", func(ktesting.Action) (bool, kruntime.Object, error) { return true, nil, nil })
			case "unknown-get":
				client.PrependReactor("get", "jobs", func(ktesting.Action) (bool, kruntime.Object, error) { return true, nil, errors.New("unknown") })
			case "unknown-pods":
				client.PrependReactor("list", "pods", func(ktesting.Action) (bool, kruntime.Object, error) { return true, nil, errors.New("unknown") })
			case "foreign-pod":
				p.OwnerReferences[0].UID = "foreign-job"
				_, _ = client.CoreV1().Pods("build").Update(ctx, p, metav1.UpdateOptions{})
			case "node-not-ready":
				node, _ := client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
				node.Status.Conditions = nil
				_, _ = client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			}
			e, err := a.Cancel(ctx, task)
			expected := mode == "delete-complete" || mode == "node-not-ready"
			if e.Stopped != expected {
				t.Fatal(e, err)
			}
			if expected && err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRuntimeApplyResourcesAndSecrets(t *testing.T) {
	r, client, c, m := runtimeFixture(t)
	if _, err := r.Apply(ctx, c, m, Route{}); err != nil {
		t.Fatal(err)
	}
	d, err := client.AppsV1().Deployments("runtime").Get(ctx, AppName(c.Release.ApplicationID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertRestricted(t, d.Spec.Template.Spec)
	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || d.Spec.Strategy.RollingUpdate.MaxSurge.IntVal != 1 || d.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal != 0 {
		t.Fatal("单实例滚动策略")
	}
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), "RUNTIME-SECRET") || strings.Contains(string(data), "FILE-SECRET") {
		t.Fatal("秘密进入Deployment")
	}
	if d.Spec.Template.Spec.Containers[0].Env[1].ValueFrom.SecretKeyRef == nil {
		t.Fatal("秘密未使用引用")
	}
	for _, mount := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if !mount.ReadOnly {
			t.Fatal("配置文件必须只读")
		}
	}
	secrets, _ := client.CoreV1().Secrets("runtime").List(ctx, metav1.ListOptions{})
	if len(secrets.Items) != 1 || string(secrets.Items[0].Data["env-PASSWORD"]) != "RUNTIME-SECRET" {
		t.Fatal(secrets)
	}
	client.ClearActions()
	if _, err := r.Apply(ctx, c, m, Route{}); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			t.Fatal("重投不应创建配置副本")
		}
	}
}
func TestRuntimePVCAndIdempotentRestartStop(t *testing.T) {
	r, client, c, m := runtimeFixture(t)
	m.View.Volumes = []runtimeconfig.Volume{{Name: "data", MountPath: "/data", SizeGiB: 1}}
	if _, err := r.Apply(ctx, c, m, Route{}); err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments("runtime").Get(ctx, AppName(c.Release.ApplicationID), metav1.GetOptions{})
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("卷必须Recreate")
	}
	c.ExpectedGeneration = 2
	c.OperationID = "restart-2"
	if err := r.Restart(ctx, c); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := r.Restart(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "update" {
			t.Fatal("重投重复restart")
		}
	}
	c.ExpectedGeneration = 3
	c.OperationID = "stop-3"
	if err := r.Stop(ctx, c); err != nil {
		t.Fatal(err)
	}
	pvcs, _ := client.CoreV1().PersistentVolumeClaims("runtime").List(ctx, metav1.ListOptions{})
	if len(pvcs.Items) != 1 {
		t.Fatal("Stop删除了PVC")
	}
	c.ExpectedGeneration = 4
	c.OperationID = "start-4"
	if err := r.Start(ctx, c); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := r.Start(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "update" {
			t.Fatal("重投重复start")
		}
	}
}

// 已部署镜像的仓库凭据可能被轮换或移除，停止不能依赖这些历史明文。
// 停止依然只能作用于同一 release 和镜像，且必须等实际副本归零才报告停止。
func TestStopDoesNotRequireRegistryCredentials(t *testing.T) {
	r, client, c, m := runtimeFixture(t)
	c.Release.RegistryCredentialVersion = 3
	c.Release.RegistryDockerConfig = []byte(`{"auths":{"registry.example":{"auth":"cHJpdmF0ZQ=="}}}`)
	if _, err := r.Apply(ctx, c, m, Route{}); err != nil {
		t.Fatal(err)
	}
	stop := c
	stop.ExpectedGeneration, stop.OperationID = 2, "stop-2"
	stop.Release.RegistryCredentialVersion, stop.Release.RegistryDockerConfig = 0, nil
	other := stop
	other.Release.Image = "registry.example/app@sha256:" + strings.Repeat("b", 64)
	if err := r.Stop(ctx, other); !errors.Is(err, ErrOwnership) {
		t.Fatalf("不同镜像不得停止: %v", err)
	}
	if err := r.Stop(ctx, stop); err != nil {
		t.Fatal(err)
	}
	observed, err := r.Observe(ctx, stop, false)
	if err != nil || !observed.Stopped || !observed.CorrectGeneration || observed.KubernetesReady {
		t.Fatalf("停止确认错误: %+v %v", observed, err)
	}
	deployment, err := client.AppsV1().Deployments("runtime").Get(ctx, AppName(c.Release.ApplicationID), metav1.GetOptions{})
	if err != nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("停止未缩容: %+v %v", deployment, err)
	}
}

func TestRuntimeGenerationAndOwnership(t *testing.T) {
	for _, mode := range []string{"guard", "guard-between-writes", "foreign-app", "future-generation", "node-replaced", "config-mismatch", "mutable-image", "pvc-size-change"} {
		t.Run(mode, func(t *testing.T) {
			r, client, c, m := runtimeFixture(t)
			switch mode {
			case "guard":
				r.guard = guardFunc(func(context.Context, RuntimeChange) error { return ErrGeneration })
			case "guard-between-writes":
				count := 0
				r.guard = guardFunc(func(context.Context, RuntimeChange) error {
					count++
					if count > 2 {
						return ErrGeneration
					}
					return nil
				})
			case "foreign-app", "future-generation":
				objects, _ := r.objects(c, m, Route{})
				d := objects.deployment
				d.UID = "d"
				if mode == "foreign-app" {
					d.Labels[LabelApplication] = "other"
				} else {
					d.Labels[LabelGeneration] = "99"
				}
				_ = client.Tracker().Add(d)
			case "node-replaced":
				node, _ := client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
				node.UID = "replaced"
				_, _ = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			case "config-mismatch":
				m.View.Revision = 2
			case "mutable-image":
				c.Release.Image = "registry/app:latest"
			case "pvc-size-change":
				m.View.Volumes = []runtimeconfig.Volume{{Name: "data", MountPath: "/data", SizeGiB: 1}}
				if _, err := r.Apply(ctx, c, m, Route{}); err != nil {
					t.Fatal(err)
				}
				m.View.Volumes[0].SizeGiB = 2
			}
			_, err := r.Apply(ctx, c, m, Route{})
			if err == nil {
				t.Fatal("未拒绝危险操作")
			}
		})
	}
}
func TestQualificationNotAPIReachability(t *testing.T) {
	a, _, _ := fixture(t)
	a.config.Installed = InstalledCapabilities{}
	e, err := a.Qualify(ctx)
	if err != nil || !e.APIReachable || e.Build || e.Runtime || e.Ingress || e.Metrics || len(e.Reasons) != 4 {
		t.Fatal(e, err)
	}
}
func TestObserveRequiresGenerationDigestOldPodAndEndpoints(t *testing.T) {
	r, client, c, m := runtimeFixture(t)
	m = materialized(c, true)
	route := Route{Hosts: []string{"app.example.com"}}
	if _, err := r.Apply(ctx, c, m, route); err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments("runtime").Get(ctx, AppName(c.Release.ApplicationID), metav1.GetOptions{})
	d.Generation = 2
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	_, _ = client.AppsV1().Deployments("runtime").UpdateStatus(ctx, d, metav1.UpdateOptions{})
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "runtime", UID: "rs-uid", Labels: runtimeLabels(c), OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: ptr(true)}}}}
	_ = client.Tracker().Add(rs)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "runtime", UID: "app-pod-uid", Labels: runtimeLabels(c), Annotations: d.Spec.Template.Annotations, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr(true)}}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ImageID: image}}}}
	_ = client.Tracker().Add(pod)
	e, err := r.Observe(ctx, c, true)
	if err != nil || e.KubernetesReady || e.ServiceEndpoints {
		t.Fatal(e, err)
	}
	service, _ := client.CoreV1().Services("runtime").Get(ctx, d.Name, metav1.GetOptions{})
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "slice", Namespace: "runtime", Labels: map[string]string{discoveryv1.LabelServiceName: d.Name}, OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: service.UID}}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "runtime", UID: pod.UID}}}}
	_ = client.Tracker().Add(slice)
	e, err = r.Observe(ctx, c, true)
	if err != nil || !e.KubernetesReady || !e.RequiresIngressProbe {
		t.Fatal(e, err)
	}
	old := pod.DeepCopy()
	old.Name = "old"
	old.UID = "old-uid"
	old.Labels = copyLabels(old.Labels)
	old.Labels[LabelOperation] = "old-operation"
	_ = client.Tracker().Add(old)
	e, err = r.Observe(ctx, c, true)
	if err != nil || e.KubernetesReady || e.OldPodsExited {
		t.Fatal(e, err)
	}
	_ = client.CoreV1().Pods("runtime").Delete(ctx, old.Name, metav1.DeleteOptions{})
	pod.Status.ContainerStatuses[0].ImageID = "containerd://sha256:" + strings.Repeat("f", 64)
	_, _ = client.CoreV1().Pods("runtime").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	e, err = r.Observe(ctx, c, true)
	if err != nil || e.CorrectDigest || e.KubernetesReady {
		t.Fatal(e, err)
	}
}
func TestLogsRejectWrongUIDBeforeRequest(t *testing.T) {
	a, client, task := fixture(t)
	j, _ := a.buildObjects(task)
	j.UID = "job"
	p := buildPod(task, j.UID)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "build", RestartCount: 1}}
	_ = client.Tracker().Add(j)
	_ = client.Tracker().Add(p)
	_, err := a.FollowLogs(ctx, task, LogRequest{Namespace: "build", PodName: p.Name, PodUID: "wrong", Container: "build", RestartCount: 1, TailLines: 100})
	if !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
}
