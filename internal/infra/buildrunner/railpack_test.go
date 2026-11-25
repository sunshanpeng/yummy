package buildrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// railpackFixture 返回带 plan 目录的测试路径，源码目录已创建。
func railpackFixture(t *testing.T) (paths, string) {
	t.Helper()
	p := testPaths(t)
	p.plan = filepath.Join(p.workspace, "railpack")
	source := filepath.Join(p.workspace, "source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	return p, source
}

// fakeRailpack 生成可控的假 railpack：--version 输出固定版本；prepare 把参数追加到记录文件，
// 再执行 body（可用 $plan、$info 与 $count，count 是本次是第几次 prepare）。
func fakeRailpack(t *testing.T, body string) (binary, record string) {
	t.Helper()
	return fakeRailpackWithVersion(t, `echo "railpack version 0.43.0"`, body)
}

func fakeRailpackWithVersion(t *testing.T, version, body string) (binary, record string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "railpack")
	record = filepath.Join(dir, "record")
	script := fmt.Sprintf(`#!/bin/sh
RECORD=%q
if [ "$1" = "--version" ]; then %s; exit 0; fi
printf '%%s\n' "$*" >> "$RECORD"
count=$(wc -l < "$RECORD" | tr -d ' ')
plan=""; info=""
while [ $# -gt 0 ]; do
  case "$1" in
    --plan-out) plan="$2"; shift 2;;
    --info-out) info="$2"; shift 2;;
    *) shift;;
  esac
done
%s
`, record, version, body)
	if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return binary, record
}

const okBody = `echo "检测到 node 项目"; echo '{"steps":{},"deploy":{}}' > "$plan"; echo '{"success":true}' > "$info"`

func quickRetries(t *testing.T) {
	t.Helper()
	old := railpackRetryDelay
	railpackRetryDelay = time.Millisecond
	t.Cleanup(func() { railpackRetryDelay = old })
}

func TestPlanRailpackWritesPlanAndReportsFacts(t *testing.T) {
	p, source := railpackFixture(t)
	binary, record := fakeRailpack(t, okBody)
	var output bytes.Buffer
	result, err := planRailpack(context.Background(), PlanOptions{
		Binary: binary, SourceDir: filepath.Join(source, "app"),
		Args:        map[string]string{"RAILPACK_START_CMD": "node server.js", "NODE_ENV": "production"},
		SecretNames: []string{"NPM_TOKEN"}, Output: &output,
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(p.plan, RailpackPlanFile)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if result.PlanSHA256 != hex.EncodeToString(sum[:]) || result.Version != "railpack version 0.43.0" {
		t.Fatalf("计划摘要或版本不符: %+v", result)
	}
	if !strings.Contains(output.String(), "检测到 node 项目") {
		t.Fatalf("识别输出应进入日志: %q", output.String())
	}
	if info, err := os.Stat(p.plan); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("计划目录应为 0755: %v %v", info, err)
	}
	if info, err := os.Stat(planPath); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("计划文件应为 0644: %v %v", info, err)
	}
	recorded, _ := os.ReadFile(record)
	line := strings.TrimSpace(string(recorded))
	// 参数按名称排序；密钥只传占位值，真实值绝不进入 Railpack 的参数；目录是最后一个参数。
	for _, want := range []string{"prepare --plan-out " + planPath, "--env=NODE_ENV=production", "--env=RAILPACK_START_CMD=node server.js", "--env=NPM_TOKEN=" + railpackSecretPlaceholder} {
		if !strings.Contains(line, want) {
			t.Fatalf("缺少参数 %q: %s", want, line)
		}
	}
	if !strings.HasSuffix(line, filepath.Join(source, "app")) {
		t.Fatalf("源码目录应是最后一个参数: %s", line)
	}
	if work, _ := filepath.Glob(filepath.Join(p.workspace, ".railpack-work-*")); len(work) != 0 {
		t.Fatalf("临时工作目录未清理: %v", work)
	}
}

func TestPlanRailpackRejectsUnsupportedVersionBeforeInspectingSource(t *testing.T) {
	for name, version := range map[string]string{
		"缺少版本":    ":",
		"版本查询失败":  "exit 1",
		"版本未通过审核": `echo "railpack version 0.31.0"`,
		"预发布版本":   `echo "railpack version 0.43.0-rc.1"`,
	} {
		t.Run(name, func(t *testing.T) {
			p, source := railpackFixture(t)
			binary, record := fakeRailpackWithVersion(t, version, okBody)
			if _, err := planRailpack(context.Background(), PlanOptions{Binary: binary, SourceDir: source}, p); err == nil {
				t.Fatal("未经审核的 Railpack 版本不得分析项目")
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("版本校验失败后运行了 prepare: %v", err)
			}
		})
	}
}

func TestPlanForBuildAcceptsContextStartingWithTwoDots(t *testing.T) {
	p, source := railpackFixture(t)
	if err := os.Mkdir(filepath.Join(source, "..service"), 0755); err != nil {
		t.Fatal(err)
	}
	binary, record := fakeRailpack(t, okBody)
	var output bytes.Buffer
	if err := planForBuild(context.Background(), "..service", nil, nil, &output, binary, p); err != nil {
		t.Fatalf("上下文 ..service 合法: %v", err)
	}
	data, err := os.ReadFile(record)
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(data)), filepath.Join(source, "..service")) {
		t.Fatalf("规划器未使用原目录: %q, %v", data, err)
	}
	for _, contextPath := range []string{"..", "../service"} {
		if err := planForBuild(context.Background(), contextPath, nil, nil, &output, binary, p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("应拒绝越出源码根的路径 %q: %v", contextPath, err)
		}
	}
}

func TestPlanRailpackRetriesOnlyTransientFailures(t *testing.T) {
	quickRetries(t)
	t.Run("暂时性失败后重试成功", func(t *testing.T) {
		p, source := railpackFixture(t)
		binary, record := fakeRailpack(t, `if [ "$count" -lt 2 ]; then echo "下载 mise 失败" >&2; exit 75; fi
`+okBody)
		var output bytes.Buffer
		if _, err := planRailpack(context.Background(), PlanOptions{Binary: binary, SourceDir: source, Output: &output}, p); err != nil {
			t.Fatal(err)
		}
		recorded, _ := os.ReadFile(record)
		if n := strings.Count(string(recorded), "\n"); n != 2 {
			t.Fatalf("应重试一次共两次: %d", n)
		}
		if !strings.Contains(output.String(), "暂时性失败") {
			t.Fatalf("日志应说明重试: %q", output.String())
		}
	})
	t.Run("持续暂时性失败最多三次", func(t *testing.T) {
		p, source := railpackFixture(t)
		binary, record := fakeRailpack(t, `exit 75`)
		_, err := planRailpack(context.Background(), PlanOptions{Binary: binary, SourceDir: source}, p)
		if !errors.Is(err, ErrRailpackTransient) {
			t.Fatalf("应返回暂时性失败: %v", err)
		}
		recorded, _ := os.ReadFile(record)
		if n := strings.Count(string(recorded), "\n"); n != railpackMaxAttempts {
			t.Fatalf("应尝试 %d 次: %d", railpackMaxAttempts, n)
		}
	})
	t.Run("确定性失败不重试且保留原因", func(t *testing.T) {
		p, source := railpackFixture(t)
		binary, record := fakeRailpack(t, `echo "无法识别项目类型" >&2; exit 1`)
		var output bytes.Buffer
		_, err := planRailpack(context.Background(), PlanOptions{Binary: binary, SourceDir: source, Output: &output}, p)
		if !errors.Is(err, ErrRailpackPlan) {
			t.Fatalf("应返回识别失败: %v", err)
		}
		recorded, _ := os.ReadFile(record)
		if n := strings.Count(string(recorded), "\n"); n != 1 {
			t.Fatalf("确定性失败不应重试: %d", n)
		}
		if !strings.Contains(output.String(), "无法识别项目类型") {
			t.Fatalf("失败原因应保留在日志: %q", output.String())
		}
	})
	t.Run("取消优先于重试", func(t *testing.T) {
		p, source := railpackFixture(t)
		binary, _ := fakeRailpack(t, `exit 75`)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := planRailpack(ctx, PlanOptions{Binary: binary, SourceDir: source}, p); !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回取消: %v", err)
		}
	})
}

func TestPlanRailpackRejectsUnusablePlans(t *testing.T) {
	cases := map[string]string{
		"没有生成计划":         `exit 0`,
		"计划不是合法 JSON":    `echo "not json" > "$plan"`,
		"计划不是 JSON 对象":   `echo '[1,2]' > "$plan"`,
		"info 标记失败":      `echo '{}' > "$plan"; echo '{"success":false}' > "$info"`,
		"计划是符号链接":        `ln -s /etc/hosts "$plan"`,
		"计划超过上限":         `head -c 4300000 /dev/zero | tr '\0' 'a' > "$plan"`,
		"退出码为 0 但计划为空文件": `: > "$plan"`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, source := railpackFixture(t)
			binary, _ := fakeRailpack(t, body)
			if _, err := planRailpack(context.Background(), PlanOptions{Binary: binary, SourceDir: source}, p); !errors.Is(err, ErrRailpackPlan) {
				t.Fatalf("应拒绝并返回识别失败: %v", err)
			}
		})
	}
}

func TestPlanRailpackValidatesOptionsBeforeRunning(t *testing.T) {
	p, source := railpackFixture(t)
	binary, record := fakeRailpack(t, okBody)
	base := PlanOptions{Binary: binary, SourceDir: source}
	cases := map[string]func(*PlanOptions){
		"二进制不是绝对路径":   func(o *PlanOptions) { o.Binary = "railpack" },
		"源码目录在工作区之外":  func(o *PlanOptions) { o.SourceDir = t.TempDir() },
		"源码目录越过上级":    func(o *PlanOptions) { o.SourceDir = filepath.Join(source, "..", "other") },
		"参数名含点号":      func(o *PlanOptions) { o.Args = map[string]string{"A.B": "1"} },
		"参数名以数字开头":    func(o *PlanOptions) { o.Args = map[string]string{"1A": "1"} },
		"参数值含空字符":     func(o *PlanOptions) { o.Args = map[string]string{"A": "x\x00y"} },
		"密钥名不合法":      func(o *PlanOptions) { o.SecretNames = []string{"npm-token"} },
		"参数与密钥重名":     func(o *PlanOptions) { o.Args = map[string]string{"TOKEN": "1"}; o.SecretNames = []string{"TOKEN"} },
		"密钥名重复":       func(o *PlanOptions) { o.SecretNames = []string{"TOKEN", "TOKEN"} },
		"参数值超过 8 KiB": func(o *PlanOptions) { o.Args = map[string]string{"A": strings.Repeat("x", maxRailpackArgValue+1)} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if _, err := planRailpack(context.Background(), options, p); !errors.Is(err, ErrInvalid) {
				t.Fatalf("应返回参数无效: %v", err)
			}
		})
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("参数无效时不应启动 Railpack")
	}
}

func TestPlanRailpackEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("SECRET_FROM_PARENT", "leak-me")
	p, source := railpackFixture(t)
	binary, _ := fakeRailpack(t, `env > "$plan.env"; `+okBody)
	if _, err := planRailpack(context.Background(), PlanOptions{Binary: binary, SourceDir: source}, p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(p.plan, RailpackPlanFile+".env"))
	if err != nil {
		t.Fatal(err)
	}
	environment := string(data)
	if strings.Contains(environment, "SECRET_FROM_PARENT") || strings.Contains(environment, "leak-me") {
		t.Fatalf("不得继承父进程环境: %s", environment)
	}
	for _, want := range []string{"PATH=/usr/local/bin:/usr/bin:/bin", "NO_COLOR=1"} {
		if !strings.Contains(environment, want) {
			t.Fatalf("缺少固定环境变量 %q: %s", want, environment)
		}
	}
	// 工作目录是清理前的临时目录，HOME 与缓存目录都在其中，不写 worker 或用户的真实家目录。
	for _, line := range strings.Split(environment, "\n") {
		if strings.HasPrefix(line, "HOME=") && !strings.HasPrefix(strings.TrimPrefix(line, "HOME="), p.workspace) {
			t.Fatalf("HOME 应在工作区内: %s", line)
		}
	}
}

func railpackInput() RailpackBuildInput {
	return RailpackBuildInput{
		FrontendImage: "ghcr.io/railwayapp/railpack-frontend:v0.99.0@sha256:" + strings.Repeat("a", 64),
		Context:       "services/web", ImageTag: "registry.example.com/team/app:build-1", ApplicationID: "app_one",
		Args:    map[string]string{"NODE_ENV": "production", "RAILPACK_START_CMD": "node server.js"},
		Secrets: []RailpackSecret{{ID: "NPM_TOKEN", Value: []byte("npm-secret")}, {ID: "PIP_TOKEN", Value: []byte("pip-secret")}},
	}
}

func TestRailpackBuildArgsPassFixedValidator(t *testing.T) {
	args, mounts, err := RailpackBuildArgs(railpackInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBuildArgs(args); err != nil {
		t.Fatalf("Railpack 参数被固定校验器拒绝: %v\n%v", err, args)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"--frontend=gateway.v0", "--local=context=/workspace/source/services/web", "--local=dockerfile=" + RailpackPlanDir,
		"--opt=filename=" + RailpackPlanFile, "--opt=platform=linux/amd64", "--opt=build-arg:cache-key=app_one",
		"--output=type=image,name=registry.example.com/team/app:build-1,push=true",
		"--secret=id=NPM_TOKEN,src=" + BuildAuthDir + "/secret-0", "--secret=id=PIP_TOKEN,src=" + BuildAuthDir + "/secret-1",
		"--secret=id=NODE_ENV,src=" + BuildAuthDir + "/arg-0", "--secret=id=RAILPACK_START_CMD,src=" + BuildAuthDir + "/arg-1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少参数 %q:\n%s", want, joined)
		}
	}
	// 密钥值与构建参数值都只在挂载文件里，不进入 buildctl 参数。
	for _, leaked := range []string{"npm-secret", "pip-secret", "node server.js"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("参数泄露了 %q", leaked)
		}
	}
	if len(mounts) != 4 || !mounts[0].Sensitive || !mounts[1].Sensitive || mounts[2].Sensitive || mounts[3].Sensitive {
		t.Fatalf("挂载排序或敏感标记错误: %+v", mounts)
	}
}

func TestRailpackBuildArgsRejectsUnsafeInput(t *testing.T) {
	cases := map[string]func(*RailpackBuildInput){
		"前端镜像未钉 digest": func(in *RailpackBuildInput) { in.FrontendImage = "ghcr.io/railwayapp/railpack-frontend:latest" },
		"前端镜像为空":        func(in *RailpackBuildInput) { in.FrontendImage = "" },
		"上下文越出源码根":      func(in *RailpackBuildInput) { in.Context = "../etc" },
		"应用标识含逗号":       func(in *RailpackBuildInput) { in.ApplicationID = "app,one" },
		"参数名含点号":        func(in *RailpackBuildInput) { in.Args = map[string]string{"a.b": "1"} },
		"密钥名含连字符":       func(in *RailpackBuildInput) { in.Secrets = []RailpackSecret{{ID: "npm-token"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := railpackInput()
			mutate(&input)
			if _, _, err := RailpackBuildArgs(input); err == nil {
				t.Fatal("应拒绝")
			}
		})
	}
}

// 校验器必须只接受 Railpack 的固定形态：篡改任一参数都应被拒绝，Dockerfile 形态也不能借用 arg- 挂载。
func TestValidatorRejectsTamperedRailpackArgs(t *testing.T) {
	good, _, err := RailpackBuildArgs(railpackInput())
	if err != nil {
		t.Fatal(err)
	}
	replace := func(prefix, with string) []string {
		out := append([]string(nil), good...)
		for i, arg := range out {
			if strings.HasPrefix(arg, prefix) {
				out[i] = with
				return out
			}
		}
		t.Fatalf("没有以 %q 开头的参数", prefix)
		return nil
	}
	without := func(prefix string) []string {
		var out []string
		for _, arg := range good {
			if !strings.HasPrefix(arg, prefix) {
				out = append(out, arg)
			}
		}
		return out
	}
	digest := "@sha256:" + strings.Repeat("b", 64)
	cases := map[string][]string{
		"前端换成 tag":       replace("--opt=source=", "--opt=source=ghcr.io/evil/frontend:latest"),
		"前端 digest 形态错":  replace("--opt=source=", "--opt=source=ghcr.io/x/y@sha256:abc"),
		"计划目录指向源码":       replace("--local=dockerfile=", "--local=dockerfile=/workspace/source"),
		"计划文件名被改":        replace("--opt=filename=", "--opt=filename=other.json"),
		"平台被改":           replace("--opt=platform=", "--opt=platform=linux/arm64"),
		"摘要形态错":          replace("--opt=build-arg:secrets-hash=", "--opt=build-arg:secrets-hash=zzz"),
		"缓存键含空格":         replace("--opt=build-arg:cache-key=", "--opt=build-arg:cache-key=a b"),
		"输出到 latest":     replace("--output=", "--output=type=image,name=registry.example.com/app:latest,push=true"),
		"元数据路径被改":        replace("--metadata-file=", "--metadata-file=/tmp/x"),
		"密钥文件越出目录":       replace("--secret=id=NPM_TOKEN", "--secret=id=NPM_TOKEN,src=/etc/passwd"),
		"密钥文件序号越界":       replace("--secret=id=NPM_TOKEN", "--secret=id=NPM_TOKEN,src="+BuildAuthDir+"/secret-100"),
		"额外目标阶段":         append(append([]string(nil), good...), "--opt=target=prod"),
		"额外任意 build-arg": append(append([]string(nil), good...), "--opt=build-arg:X=1"),
		"额外缓存导入":         append(append([]string(nil), good...), "--import-cache=type=registry,ref=evil"),
		"重复前端镜像":         append(append([]string(nil), good...), "--opt=source=ghcr.io/other/frontend"+digest),
		"缺少前端镜像":         without("--opt=source="),
		"缺少摘要":           without("--opt=build-arg:secrets-hash="),
		"缺少缓存键":          without("--opt=build-arg:cache-key="),
		"缺少计划目录":         without("--local=dockerfile="),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateBuildArgs(args); err == nil {
				t.Fatalf("应拒绝: %v", args)
			}
		})
	}
	// Dockerfile 形态下 arg- 文件不是合法的密钥挂载，也不能出现 gateway 前端。
	dockerfile := validArgs(fixedPaths())
	dockerfile[len(dockerfile)-1] = "--secret=id=npm,src=" + BuildAuthDir + "/arg-0"
	if err := ValidateBuildArgs(dockerfile); err == nil {
		t.Fatal("Dockerfile 形态不应接受 arg- 挂载文件")
	}
	reordered := []string{"build"}
	for i := len(good) - 1; i >= 1; i-- {
		reordered = append(reordered, good[i])
	}
	if err := ValidateBuildArgs(reordered); err != nil {
		t.Fatalf("参数顺序不影响合法形态: %v", err)
	}
	if err := ValidateBuildArgs([]string{"build", "--frontend=something.v0"}); err == nil {
		t.Fatal("未知前端应拒绝")
	}
}

func TestRailpackSecretsHashTracksValuesAndScope(t *testing.T) {
	base := railpackInput()
	_, mounts, _ := RailpackBuildArgs(base)
	hash := RailpackSecretsHash("app_one", mounts)
	if hash != RailpackSecretsHash("app_one", mounts) || len(hash) != 64 {
		t.Fatal("同输入应得到稳定的 64 位十六进制摘要")
	}
	if hash == RailpackSecretsHash("app_two", mounts) {
		t.Fatal("摘要应按应用加盐")
	}
	changedSecret := base
	changedSecret.Secrets = []RailpackSecret{{ID: "NPM_TOKEN", Value: []byte("rotated")}, {ID: "PIP_TOKEN", Value: []byte("pip-secret")}}
	_, secretMounts, _ := RailpackBuildArgs(changedSecret)
	changedArg := base
	changedArg.Args = map[string]string{"NODE_ENV": "development", "RAILPACK_START_CMD": "node server.js"}
	_, argMounts, _ := RailpackBuildArgs(changedArg)
	for name, other := range map[string][]RailpackMount{"密钥值变化": secretMounts, "构建参数值变化": argMounts} {
		if RailpackSecretsHash("app_one", other) == hash {
			t.Fatalf("%s必须使缓存失效", name)
		}
	}
	// 顺序无关：同一组挂载以不同顺序传入得到同一摘要。
	reversed := []RailpackMount{mounts[3], mounts[2], mounts[1], mounts[0]}
	if RailpackSecretsHash("app_one", reversed) != hash {
		t.Fatal("摘要不应依赖挂载顺序")
	}
}

func TestRailpackSecretsHashDistinguishesMultilineMountValues(t *testing.T) {
	first := []RailpackMount{{ID: "A", Value: []byte("x\nB=y")}, {ID: "B", Value: []byte("z")}}
	second := []RailpackMount{{ID: "A", Value: []byte("x")}, {ID: "B", Value: []byte("y\nB=z")}}
	if RailpackSecretsHash("app_one", first) == RailpackSecretsHash("app_one", second) {
		t.Fatal("合法多行挂载每一项都有独立边界，值变化后必须使缓存失效")
	}
}

func TestRailpackMountsSeparateSecretsFromArgs(t *testing.T) {
	mounts := RailpackMounts(map[string]string{"B": "2", "A": "1"}, []RailpackSecret{{ID: "S1", Value: []byte("x")}, {ID: "S0", Value: []byte("y")}})
	var files []string
	for _, mount := range mounts {
		files = append(files, mount.File+"="+mount.ID)
	}
	if got := strings.Join(files, ","); got != "secret-0=S1,secret-1=S0,arg-0=A,arg-1=B" {
		t.Fatalf("挂载顺序或命名不符: %s", got)
	}
}

func TestRailpackNameAndImageHelpers(t *testing.T) {
	for _, name := range []string{"A", "_x", "NODE_ENV", "a1_b2"} {
		if !ValidRailpackEnvName(name) {
			t.Fatalf("%q 应合法", name)
		}
	}
	for _, name := range []string{"", "1A", "a-b", "a.b", "a b", strings.Repeat("x", 129)} {
		if ValidRailpackEnvName(name) {
			t.Fatalf("%q 应非法", name)
		}
	}
	pinned := "ghcr.io/railwayapp/railpack-frontend@sha256:" + strings.Repeat("c", 64)
	if !ValidRailpackFrontendImage(pinned) || ValidRailpackFrontendImage("ghcr.io/railwayapp/railpack-frontend:v1") || ValidRailpackFrontendImage(pinned+",x") {
		t.Fatal("前端镜像必须以 digest 钉住")
	}
	if got := PlanSummary(PlanResult{PlanSHA256: "abc"}); !strings.Contains(got, "版本 未知") || !strings.Contains(got, "sha256:abc") {
		t.Fatalf("摘要行不符: %s", got)
	}
}
