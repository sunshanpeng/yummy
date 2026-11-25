package kube

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sunshanpeng/yummy/internal/infra/buildrunner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testFrontend = "ghcr.io/railwayapp/railpack-frontend:v0.99.0@sha256:"

func railpackTask(t *testing.T) (*Adapter, *fake.Clientset, BuildTask) {
	t.Helper()
	a, client, task := fixture(t)
	a.config.Installed.RailpackFrontendImage = testFrontend + strings.Repeat("a", 64)
	task.Method = MethodRailpack
	task.Dockerfile, task.Target = "", ""
	task.BuildArgs = map[string]string{"NODE_ENV": "production"}
	task.Secrets = []BuildSecret{{ID: "NPM_TOKEN", Version: "v1", Value: []byte("BUILD-SECRET")}}
	return a, client, task
}

// Railpack 构建的 Job：识别在 build 容器内先于 buildctl 运行，密钥值只在 Secret 文件里，
// buildctl 参数仍通过 runner 的固定校验器。
func TestRailpackBuildObjects(t *testing.T) {
	a, _, task := railpackTask(t)
	job, secret, err := a.buildObjects(task)
	if err != nil {
		t.Fatal(err)
	}
	spec := job.Spec.Template.Spec
	build := spec.Containers[0]
	for _, want := range []string{"--railpack-context=.", "--railpack-arg=NODE_ENV=production", "--railpack-secret=NPM_TOKEN"} {
		if !slices.Contains(build.Args, want) {
			t.Fatalf("build 容器缺少 %q: %v", want, build.Args)
		}
	}
	separator := slices.Index(build.Args, "--")
	if separator < 0 {
		t.Fatalf("runner 参数与 buildctl 参数之间缺少 --: %v", build.Args)
	}
	for _, runnerArg := range build.Args[:separator] {
		if strings.Contains(runnerArg, "BUILD-SECRET") {
			t.Fatalf("密钥值不应出现在 runner 参数: %s", runnerArg)
		}
	}
	if err := buildrunner.ValidateBuildArgs(build.Args[separator+1:]); err != nil {
		t.Fatalf("buildctl 参数被固定校验器拒绝: %v\n%v", err, build.Args)
	}
	for _, arg := range build.Args {
		if strings.Contains(arg, "BUILD-SECRET") {
			t.Fatalf("密钥值泄露到容器参数: %s", arg)
		}
	}
	if string(secret.Data["secret-0"]) != "BUILD-SECRET" || string(secret.Data["arg-0"]) != "production" {
		t.Fatalf("Secret 应保存密钥与构建参数文件: %v", secret.Data)
	}
	var items []string
	for _, volume := range spec.Volumes {
		if volume.Name == "auth" {
			for _, item := range volume.Secret.Items {
				items = append(items, item.Key)
			}
		}
	}
	for _, want := range []string{"config.json", "secret-0", "arg-0"} {
		if !slices.Contains(items, want) {
			t.Fatalf("auth 卷缺少 %q: %v", want, items)
		}
	}
	// 准备容器只下载并校验源码，不要求 Dockerfile。
	if prepare := spec.InitContainers[0]; !slices.Contains(prepare.Args, "--dockerfile=") {
		t.Fatalf("Railpack 构建的 prepare 不应指定 Dockerfile: %v", prepare.Args)
	}
	assertRestricted(t, spec)
}

func TestDockerfileTaskIsUnchangedByRailpackSupport(t *testing.T) {
	a, _, task := fixture(t)
	data, err := json.Marshal(task)
	if err != nil || strings.Contains(string(data), "Method") {
		t.Fatalf("Dockerfile 任务的快照指纹不应出现 Method 字段: %s %v", data, err)
	}
	job, secret, err := a.buildObjects(task)
	if err != nil {
		t.Fatal(err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	for _, arg := range args {
		if strings.HasPrefix(arg, "--railpack-") {
			t.Fatalf("Dockerfile 构建不应带 Railpack 参数: %s", arg)
		}
	}
	if _, ok := secret.Data["arg-0"]; ok {
		t.Fatal("Dockerfile 构建的构建参数继续走 --opt=build-arg，不写 arg 文件")
	}
}

func TestRailpackRunFailsClosedOnUnsafeTasks(t *testing.T) {
	cases := map[string]func(*Adapter, *BuildTask){
		"同时指定 Dockerfile": func(_ *Adapter, task *BuildTask) { task.Dockerfile = "Dockerfile" },
		"指定了目标阶段":         func(_ *Adapter, task *BuildTask) { task.Target = "prod" },
		"构建参数名含点号":        func(_ *Adapter, task *BuildTask) { task.BuildArgs = map[string]string{"a.b": "1"} },
		"构建密钥名含连字符":       func(_ *Adapter, task *BuildTask) { task.Secrets[0].ID = "npm-token" },
		"参数与密钥重名":         func(_ *Adapter, task *BuildTask) { task.BuildArgs = map[string]string{"NPM_TOKEN": "x"} },
		"上下文越界":           func(_ *Adapter, task *BuildTask) { task.Context = "../x" },
		"未配置前端镜像":         func(a *Adapter, _ *BuildTask) { a.config.Installed.RailpackFrontendImage = "" },
		"前端镜像只用 tag":      func(a *Adapter, _ *BuildTask) { a.config.Installed.RailpackFrontendImage = "ghcr.io/x/y:v1" },
		"未知构建方式":          func(_ *Adapter, task *BuildTask) { task.Method = "NIXPACKS" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, client, task := railpackTask(t)
			mutate(a, &task)
			if _, err := a.Run(ctx, task); !errors.Is(err, ErrInvalid) {
				t.Fatalf("应失败关闭: %v", err)
			}
			jobs, err := client.BatchV1().Jobs(a.buildNamespace(task)).List(ctx, metav1.ListOptions{})
			if err != nil || len(jobs.Items) != 0 {
				t.Fatalf("失败关闭时不应创建 Job: %v %v", jobs, err)
			}
		})
	}
}

// 前端镜像是安装配置的一部分：Job 创建后配置被改动，同一 attempt 的核对必须拒绝而不是接管。
func TestRailpackJobRejectsFrontendDrift(t *testing.T) {
	a, _, task := railpackTask(t)
	if _, err := a.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, task); err != nil {
		t.Fatalf("同一配置重复 Run 应幂等: %v", err)
	}
	a.config.Installed.RailpackFrontendImage = testFrontend + strings.Repeat("b", 64)
	if _, err := a.Run(ctx, task); !errors.Is(err, ErrOwnership) {
		t.Fatalf("前端镜像变化后应拒绝既有 Job: %v", err)
	}
}
