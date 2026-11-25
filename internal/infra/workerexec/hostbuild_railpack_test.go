package workerexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/buildrunner"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

var testRailpackFrontend = "ghcr.io/railwayapp/railpack-frontend:v0.99.0@sha256:" + strings.Repeat("e", 64)

func railpackCommand(action string) *platformv1.WorkerBuildCommand {
	command := buildCommand(action)
	command.Dockerfile = ""
	command.BuildMethod = "RAILPACK"
	return command
}

func TestBuildTaskMapsBuildMethod(t *testing.T) {
	executor, _, _, _ := newExecutorFixture(t)
	for method, want := range map[string]string{"": "", "DOCKERFILE": "", "RAILPACK": kube.MethodRailpack} {
		command := buildCommand("RUN")
		command.BuildMethod = method
		task, err := executor.buildTask(command)
		if err != nil || task.Method != want {
			t.Fatalf("%q 应映射为 %q: %+v %v", method, want, task.Method, err)
		}
	}
	command := buildCommand("RUN")
	command.BuildMethod = "NIXPACKS"
	if _, err := executor.buildTask(command); err == nil {
		t.Fatal("未知构建方式必须拒绝")
	}
}

// Pod 模式的节点没有配置 Railpack 前端镜像时失败关闭：日志说明原因，错误码明确，不创建 Job。
func TestPodBuildRailpackWithoutFrontendFailsClosed(t *testing.T) {
	executor, client, _, emitter := newExecutorFixture(t)
	if err := executor.Build(context.Background(), railpackCommand("RUN"), false, emitter); err != nil {
		t.Fatal(err)
	}
	events := emitter.buildEvents()
	if len(events) != 1 || events[0].Kind != "FAILED" || events[0].ErrorCode != "RAILPACK_UNAVAILABLE" || !events[0].LogsComplete || events[0].JobUid == "" {
		t.Fatalf("应只上报明确的失败事件: %+v", events)
	}
	logs := emitter.buildLogs()
	if len(logs) != 1 || !strings.Contains(string(logs[0].Content), "Railpack") {
		t.Fatalf("日志应说明原因: %+v", logs)
	}
	jobs, err := client.BatchV1().Jobs(testBuildNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 0 {
		t.Fatalf("不应创建 Job: %v %v", jobs, err)
	}
}

// Pod 模式配置了前端镜像后，Railpack 构建创建带 runner 识别参数的 Job。
func TestPodBuildRailpackCreatesJob(t *testing.T) {
	adapter, client, capabilities := newKubeFixtureWithRailpack(t)
	executor, err := NewExecutor(Options{Adapter: adapter, Client: client, Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, RuntimeNamespace: testRuntimeNamespace, Generation: &stubGeneration{}, Installed: capabilities, BuildPollInterval: 100 * time.Millisecond, ObserveInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	emitter := newRecordingEmitter()
	command := railpackCommand("RUN")
	command.BuildArgs = map[string]string{"NODE_ENV": "production"}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- executor.Build(ctx, command, false, emitter) }()
	waitFor(t, 5*time.Second, func() bool { return len(emitter.buildEvents()) >= 1 })
	cancel()
	<-done
	job, err := client.BatchV1().Jobs(testBuildNamespace).Get(context.Background(), kube.JobName(testAttempt), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	for _, want := range []string{"--railpack-context=.", "--railpack-arg=NODE_ENV=production", "--frontend=gateway.v0", "--opt=source=" + testRailpackFrontend} {
		if !strings.Contains(args, want) {
			t.Fatalf("Job 缺少 %q:\n%s", want, args)
		}
	}
	if events := emitter.buildEvents(); events[0].Kind != "ACCEPTED" {
		t.Fatalf("第一个事件应是 ACCEPTED: %+v", events)
	}
}

// newKubeFixtureWithRailpack 复用同一个 fake 客户端，用配置了 Railpack 前端镜像的资格重建适配器；
// 生产装配把同一份资格同时交给适配器与 Options.Installed，测试也必须一致。
func newKubeFixtureWithRailpack(t *testing.T) (*kube.Adapter, *fake.Clientset, kube.InstalledCapabilities) {
	t.Helper()
	_, client := newKubeFixture(t)
	capabilities := kube.InstalledCapabilities{Build: true, Runtime: true, Ingress: true, Metrics: true, QualificationID: "test-only-not-real-qualification", RunnerImage: testRunnerImage, BuildKitImage: testRunnerImage, RailpackFrontendImage: testRailpackFrontend}
	adapter, err := kube.New(client, kube.Config{Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, RuntimeNamespace: testRuntimeNamespace, BuildDeadline: time.Minute, StorageClass: "local-path", IngressClass: "traefik", Installed: capabilities})
	if err != nil {
		t.Fatal(err)
	}
	return adapter, client, capabilities
}

// 宿主模式缺少 Railpack 二进制或前端镜像时同样失败关闭：先 ACCEPTED，再以明确错误码 FAILED。
func TestHostBuildRailpackWithoutConfigurationFailsClosed(t *testing.T) {
	for name, configure := range map[string]func(*Options){
		"两者都缺":   func(*Options) {},
		"缺二进制路径": func(o *Options) { o.Installed.RailpackFrontendImage = testRailpackFrontend },
		"缺前端镜像":  func(o *Options) { o.RailpackPath = "/usr/local/bin/railpack" },
		"前端镜像只用 tag": func(o *Options) {
			o.RailpackPath = "/usr/local/bin/railpack"
			o.Installed.RailpackFrontendImage = "ghcr.io/x/y:v1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			adapter, client := newKubeFixture(t)
			options := Options{Adapter: adapter, Client: client, Node: kube.NodeTarget{Name: testNodeName, UID: types.UID(testNodeUID)}, BuildNamespace: testBuildNamespace, RuntimeNamespace: testRuntimeNamespace, Generation: &stubGeneration{}, BuildKitAddr: "unix:///run/buildkit/buildkitd.sock", HostBuildCtl: "/usr/local/bin/buildctl", Installed: kube.InstalledCapabilities{HostBuild: true}}
			configure(&options)
			executor, err := NewExecutor(options)
			if err != nil {
				t.Fatal(err)
			}
			emitter := newRecordingEmitter()
			if err := executor.Build(context.Background(), railpackCommand("RUN"), false, emitter); err != nil {
				t.Fatal(err)
			}
			events := emitter.buildEvents()
			if len(events) != 2 || events[0].Kind != "ACCEPTED" || events[1].Kind != "FAILED" || events[1].ErrorCode != "RAILPACK_UNAVAILABLE" || events[1].JobUid != events[0].JobUid {
				t.Fatalf("应先 ACCEPTED 再以 RAILPACK_UNAVAILABLE 失败: %+v", events)
			}
		})
	}
}

func TestHostBuildCancelWaitsForExecutionBeforeBuildctl(t *testing.T) {
	executor, _, _, _ := newExecutorFixture(t)
	executor.options.Installed.HostBuild = true
	executor.options.BuildKitAddr = "unix:///run/buildkit/buildkitd.sock"
	executor.options.HostBuildCtl = "/usr/local/bin/buildctl"
	emitter := &blockingHostBuildEmitter{recordingEmitter: newRecordingEmitter(), kind: "ACCEPTED", entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanup()
	done := make(chan error, 1)
	go func() { done <- executor.Build(ctx, railpackCommand("RUN"), false, emitter) }()
	select {
	case <-emitter.entered:
	case <-ctx.Done():
		t.Fatal("宿主构建未受理")
	}

	// 任务已经受理但尚未启动 buildctl。即使取消请求先超时，也必须停止这次执行并等待真实退出。
	command := railpackCommand("CANCEL")
	command.JobUid = emitter.buildEvents()[0].JobUid
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	cancelErr := executor.Build(cancelCtx, command, false, emitter)
	beforeRelease := emitter.buildEvents()
	close(emitter.release)
	runErr := <-done
	if !errors.Is(cancelErr, context.Canceled) || len(beforeRelease) != 1 {
		t.Fatalf("构建退出前不能确认停止: %v，事件 %v", cancelErr, beforeRelease)
	}
	if !errors.Is(runErr, context.Canceled) || len(emitter.buildEvents()) != 1 {
		t.Fatalf("取消后仍执行了构建: %v，事件 %v", runErr, emitter.buildEvents())
	}
	if err := executor.Build(ctx, command, false, emitter); err != nil {
		t.Fatal(err)
	}
	if events := emitter.buildEvents(); len(events) != 2 || events[1].Kind != "STOPPED" || events[1].JobUid != command.JobUid {
		t.Fatalf("构建实际退出后才应确认停止: %v", events)
	}
}

type blockingHostBuildEmitter struct {
	*recordingEmitter
	kind             string
	entered, release chan struct{}
}

func (e *blockingHostBuildEmitter) BuildEvent(event *platformv1.WorkerBuildEvent) error {
	err := e.recordingEmitter.BuildEvent(event)
	if event.Kind == e.kind {
		close(e.entered)
		<-e.release
	}
	return err
}

func TestHostBuildSerializesDifferentApplicationsAndCancelsWaitingBuild(t *testing.T) {
	executor, _, _, _ := newExecutorFixture(t)
	executor.options.Installed.HostBuild = true
	executor.options.BuildKitAddr = "unix:///run/buildkit/buildkitd.sock"
	executor.options.HostBuildCtl = "/usr/local/bin/buildctl"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 第一个任务在执行末尾阻塞，避免测试触碰真实的宿主路径。
	first := &blockingHostBuildEmitter{recordingEmitter: newRecordingEmitter(), kind: "FAILED", entered: make(chan struct{}), release: make(chan struct{})}
	releaseFirst := sync.OnceFunc(func() { close(first.release) })
	defer releaseFirst()
	firstDone := make(chan error, 1)
	go func() { firstDone <- executor.Build(ctx, railpackCommand("RUN"), false, first) }()
	select {
	case <-first.entered:
	case <-ctx.Done():
		t.Fatal("第一个任务未进入宿主构建")
	}

	start := func(attempt, application string) (*platformv1.WorkerBuildCommand, *recordingEmitter, <-chan error) {
		command := railpackCommand("RUN")
		command.AttemptId, command.StableJobName, command.ApplicationId = attempt, kube.JobName(attempt), application
		emitter := newRecordingEmitter()
		done := make(chan error, 1)
		go func() { done <- executor.Build(ctx, command, false, emitter) }()
		waitFor(t, time.Second, func() bool { return len(emitter.buildEvents()) > 0 })
		return command, emitter, done
	}
	waiting, waitingEmitter, waitingDone := start("attempt-waiting", "app_waiting")
	select {
	case err := <-waitingDone:
		t.Fatalf("前一个任务未退出时，其他应用不应进入共享执行路径: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelWaiting := railpackCommand("CANCEL")
	cancelWaiting.AttemptId, cancelWaiting.StableJobName, cancelWaiting.ApplicationId = waiting.AttemptId, waiting.StableJobName, waiting.ApplicationId
	cancelWaiting.JobUid = waitingEmitter.buildEvents()[0].JobUid
	if err := executor.Build(ctx, cancelWaiting, false, waitingEmitter); err != nil {
		t.Fatal(err)
	}
	if err := <-waitingDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("排队中的构建应可取消: %v", err)
	}
	if events := waitingEmitter.buildEvents(); len(events) != 2 || events[1].Kind != "STOPPED" || len(waitingEmitter.buildLogs()) != 0 {
		t.Fatalf("已取消任务不得进入共享执行路径: %v", events)
	}

	_, next, nextDone := start("attempt-next", "app_next")
	releaseFirst()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-nextDone; err != nil {
		t.Fatal(err)
	}
	if events := next.buildEvents(); len(events) != 2 || events[1].ErrorCode != "RAILPACK_UNAVAILABLE" {
		t.Fatalf("前一个任务退出后应允许后续任务执行: %v", events)
	}
}

func TestHostRailpackArgsPassFixedValidator(t *testing.T) {
	executor, _, _, _ := newExecutorFixture(t)
	executor.options.RailpackPath = "/usr/local/bin/railpack"
	executor.options.Installed.RailpackFrontendImage = testRailpackFrontend
	if !executor.hostRailpackReady() {
		t.Fatal("两项配置齐全时应就绪")
	}
	task := kube.BuildTask{
		AttemptID: testAttempt, ApplicationID: "app_one", Platform: kube.Platform, Method: kube.MethodRailpack,
		ImageTag: "registry.example.com/app:build-1", Context: "services/web",
		BuildArgs: map[string]string{"NODE_ENV": "production"},
		Secrets:   []kube.BuildSecret{{ID: "NPM_TOKEN", Value: []byte("npm-secret")}},
	}
	args, mounts, err := executor.hostRailpackArgs(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := buildrunner.ValidateBuildArgs(args); err != nil {
		t.Fatalf("宿主 Railpack 参数被固定校验器拒绝: %v\n%v", err, args)
	}
	if len(mounts) != 2 || mounts[0].File != "secret-0" || mounts[1].File != "arg-0" {
		t.Fatalf("挂载文件不符: %+v", mounts)
	}
	for _, arg := range args {
		if strings.Contains(arg, "npm-secret") {
			t.Fatalf("密钥值不应出现在参数中: %s", arg)
		}
	}
	executor.options.Installed.RailpackFrontendImage = ""
	if executor.hostRailpackReady() {
		t.Fatal("缺前端镜像时不应就绪")
	}
}

func TestWriteHostMountsSeparatesSecretFilesFromArgFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "build-auth")
	mounts := buildrunner.RailpackMounts(map[string]string{"NODE_ENV": "production"}, []buildrunner.RailpackSecret{{ID: "NPM_TOKEN", Value: []byte("npm-secret")}})
	if err := writeHostMounts(dir, mounts); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("目录应为 0700: %v %v", info, err)
	}
	for file, want := range map[string]string{"secret-0": "npm-secret", "arg-0": "production"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s 内容不符: %q %v", file, data, err)
		}
		if info, _ := os.Stat(filepath.Join(dir, file)); info.Mode().Perm() != 0600 {
			t.Fatalf("%s 应为 0600", file)
		}
	}
	// 构建参数不在脱敏表里：只有密钥值会被替换，避免把日志里的短参数值全部替换成 [REDACTED]。
	known, err := hostRedactionSecrets(kube.BuildTask{Secrets: []kube.BuildSecret{{ID: "NPM_TOKEN", Value: []byte("npm-secret")}}, BuildArgs: map[string]string{"NODE_ENV": "production"}})
	if err != nil || len(known) != 1 || string(known[0]) != "npm-secret" {
		t.Fatalf("脱敏表只应含密钥值: %q %v", known, err)
	}
}

func TestRailpackFailureCodeAndDropIdentity(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w（退出码 1）", buildrunner.ErrRailpackPlan):       "RAILPACK_PLAN_FAILED",
		fmt.Errorf("%w：已尝试 3 次", buildrunner.ErrRailpackTransient): "RAILPACK_PLAN_TRANSIENT",
		errors.New("mkdir /workspace/railpack: permission denied"): "",
	} {
		if got := railpackFailureCode(err); got != want {
			t.Fatalf("%v 应映射为 %q，实际 %q", err, want, got)
		}
	}
	uid, gid := railpackDropIdentity()
	if os.Geteuid() != 0 && (uid != 0 || gid != 0) {
		t.Fatalf("非 root 运行不应降权: %d/%d", uid, gid)
	}
	if os.Geteuid() == 0 && uid == 0 {
		t.Fatal("root 运行必须降权到非特权身份")
	}
}

func TestSecretIDsKeepsOrder(t *testing.T) {
	ids := secretIDs([]kube.BuildSecret{{ID: "B"}, {ID: "A"}})
	if strings.Join(ids, ",") != "B,A" {
		t.Fatalf("顺序不符: %v", ids)
	}
}
