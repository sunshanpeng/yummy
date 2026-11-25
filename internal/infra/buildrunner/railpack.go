package buildrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Railpack 构建路径（2026-11-23 构建方式自动选择）：源码没有 Dockerfile 时，固定版本的 Railpack
// 在构建任务内分析源码并生成计划文件，BuildKit 再通过 Railpack 前端镜像执行该计划。
// 计划生成只读取源码并联网解析工具版本，不运行项目脚本；项目的构建命令只在 BuildKit 沙箱内运行，
// 与 Dockerfile 构建处于同一隔离边界。

const (
	// RailpackPlanDir 是计划文件的固定目录，BuildKit 的 dockerfile 本地源指向这里。
	RailpackPlanDir = "/workspace/railpack"
	// RailpackPlanFile 是 Railpack 前端默认读取的计划文件名。
	RailpackPlanFile = "railpack-plan.json"
	// PodRailpackBinary 是 runner 镜像内 Railpack 的固定路径；宿主构建由安装配置给出路径。
	PodRailpackBinary = "/usr/local/bin/railpack"
	// 只允许已核对 Mise 安全模式的官方版本，与 runner.Dockerfile 固定值一致。
	railpackVersionOutput = "railpack version 0.43.0"

	railpackInfoFile = "railpack-info.json"
	// railpack 退出码 75（EX_TEMPFAIL）表示下载工具等暂时性失败，可重试；其余失败重试结果不会改变。
	railpackExitTransient = 75
	railpackMaxAttempts   = 3
	maxRailpackPlanBytes  = 4 << 20
	// railpackSecretPlaceholder 只让计划记录密钥名称；真实值在构建阶段经 BuildKit secret 提供。
	railpackSecretPlaceholder = "yummy-secret-placeholder"
	maxRailpackArgValue       = 8192
)

var (
	// ErrRailpackPlan 表示 Railpack 无法为该源码生成计划（无法识别、缺少启动命令、配置文件无效等），重试不会改变结果。
	ErrRailpackPlan = errors.New("Railpack 无法为该源码生成构建计划")
	// ErrRailpackTransient 表示重试多次后仍遇到网络等暂时性失败。
	ErrRailpackTransient = errors.New("Railpack 生成计划时持续遇到暂时性失败")
	// ErrRailpackVersion 表示节点未提供通过审核的规划器版本。
	ErrRailpackVersion = errors.New("Railpack 版本校验失败")

	// railpackEnvName 是 Railpack 能正确解析的变量名：它按 [A-Za-z0-9_+-] 切分 --env，
	// 带点号的名称会被截断，所以比 Dockerfile 的 build-arg 名称更严格。
	railpackEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// railpackImagePattern 限定 Railpack 前端镜像形态：必须以 digest 钉住，不接受 tag 或 latest。
	railpackImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,254}@sha256:[a-f0-9]{64}$`)
	cacheKeyPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

	// railpackRetryDelay 是暂时性失败的重试间隔，测试中缩短。
	railpackRetryDelay = 3 * time.Second
)

// ValidRailpackEnvName 判断名称能否作为 Railpack 构建参数或密钥名称。
func ValidRailpackEnvName(name string) bool { return railpackEnvName.MatchString(name) }

// ValidRailpackFrontendImage 判断前端镜像引用是否以 digest 钉住。
func ValidRailpackFrontendImage(image string) bool { return railpackImagePattern.MatchString(image) }

// PlanOptions 描述一次 Railpack 计划生成。
type PlanOptions struct {
	// Binary 是 Railpack 可执行文件的绝对路径，来自安装配置或 runner 镜像，不接受构建输入。
	Binary string
	// SourceDir 是构建上下文目录（源码根下的相对上下文），只读取不修改。
	SourceDir string
	// Args 是非敏感构建参数：作为计划配置（如 RAILPACK_START_CMD）与构建环境变量。
	Args map[string]string
	// SecretNames 是构建密钥名称：计划只记录名称，值在构建阶段经 BuildKit secret 提供。
	SecretNames []string
	// DropToUID/DropToGID 非零时降权运行 Railpack。宿主构建的 worker 以 root 运行，
	// 分析不可信源码的子进程不应继承 root。
	DropToUID, DropToGID uint32
	// Output 接收 Railpack 的识别结果与失败原因，会进入构建日志；nil 表示丢弃。
	Output io.Writer
}

// PlanResult 是计划生成的可审计事实，由调用方写入构建日志。
type PlanResult struct {
	// Version 是通过审核的 railpack --version 输出。
	Version string
	// PlanSHA256 是计划文件的 sha256（十六进制，无前缀）。
	PlanSHA256 string
}

// PlanRailpack 在固定目录生成 Railpack 计划文件。暂时性失败（退出码 75 或进程未能启动）最多重试 3 次，
// 其余失败立即返回 ErrRailpackPlan，调用方应据此直接失败构建而不是重试整个任务。
func PlanRailpack(ctx context.Context, o PlanOptions) (PlanResult, error) {
	return planRailpack(ctx, o, fixedPaths())
}

func planRailpack(ctx context.Context, o PlanOptions, p paths) (result PlanResult, err error) {
	source := filepath.Join(p.workspace, "source")
	sourceDir := filepath.Clean(o.SourceDir)
	if !filepath.IsAbs(o.Binary) || filepath.Clean(o.Binary) != o.Binary || strings.ContainsAny(o.Binary, "\x00\r\n") ||
		(sourceDir != source && !strings.HasPrefix(sourceDir, source+string(filepath.Separator))) {
		return result, ErrInvalid
	}
	out := o.Output
	if out == nil {
		out = io.Discard
	}
	envFlags, err := railpackEnvFlags(o.Args, o.SecretNames)
	if err != nil {
		return result, err
	}
	// 计划目录每次重建；降权运行时先把属主交给该身份，结束后收回，避免其他同身份进程改动计划。
	if err = os.RemoveAll(p.plan); err != nil {
		return result, err
	}
	if err = os.MkdirAll(p.plan, 0700); err != nil {
		return result, err
	}
	work, err := os.MkdirTemp(p.workspace, ".railpack-work-")
	if err != nil {
		return result, err
	}
	// work 由固定工作根内的 MkdirTemp 创建，清理目标不由构建输入计算。
	defer os.RemoveAll(work)
	dirs := map[string]string{"home": "", "cache": "", "data": "", "config": "", "tmp": ""}
	for name := range dirs {
		dirs[name] = filepath.Join(work, name)
		if err = os.Mkdir(dirs[name], 0700); err != nil {
			return result, err
		}
	}
	if o.DropToUID != 0 {
		for _, dir := range append([]string{work, p.plan}, mapValues(dirs)...) {
			if err = os.Chown(dir, int(o.DropToUID), int(o.DropToGID)); err != nil {
				return result, err
			}
		}
	}
	// 不继承 worker 或 Pod 的环境：构建输入只经 --env 进入 Railpack 的环境表，不会改变进程自身的 PATH 等变量。
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dirs["home"], "TMPDIR=" + dirs["tmp"], "XDG_CACHE_HOME=" + dirs["cache"], "XDG_DATA_HOME=" + dirs["data"], "XDG_CONFIG_HOME=" + dirs["config"], "LANG=C.UTF-8", "NO_COLOR=1"}
	result.Version = railpackVersion(ctx, o, work, env)
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if result.Version != railpackVersionOutput {
		return result, fmt.Errorf("%w：要求 %s", ErrRailpackVersion, railpackVersionOutput)
	}

	planPath := filepath.Join(p.plan, RailpackPlanFile)
	infoPath := filepath.Join(p.plan, railpackInfoFile)
	// 选项在前、目录在后；--env 值带等号，使用 --env=NAME=VALUE 单参数形式避免被拆成选项。
	flags := []string{"prepare", "--plan-out", planPath, "--info-out", infoPath}
	flags = append(flags, envFlags...)
	flags = append(flags, sourceDir)
	for attempt := 1; ; attempt++ {
		cmd := exec.CommandContext(ctx, o.Binary, flags...)
		cmd.Dir = work
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = out, out
		if o.DropToUID != 0 {
			setCredential(cmd, o.DropToUID, o.DropToGID)
		}
		runErr := runProcessGroup(ctx, cmd)
		if runErr == nil {
			break
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		var exit *exec.ExitError
		// 进程未能启动（没有退出码）按暂时性处理；被信号终止（如内存不足）重试不会改变结果。
		transient := !errors.As(runErr, &exit) || exit.ExitCode() == railpackExitTransient
		if !transient {
			return result, fmt.Errorf("%w（退出码 %d）", ErrRailpackPlan, exit.ExitCode())
		}
		if attempt >= railpackMaxAttempts {
			return result, fmt.Errorf("%w：已尝试 %d 次", ErrRailpackTransient, attempt)
		}
		fmt.Fprintf(out, "Railpack 遇到暂时性失败，%s 后重试（第 %d/%d 次）\n", railpackRetryDelay, attempt+1, railpackMaxAttempts)
		select {
		case <-time.After(railpackRetryDelay):
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}

	// 先收回属主与权限再读取，防止降权身份在校验后继续改写计划文件。
	for _, name := range []string{planPath, infoPath} {
		info, statErr := os.Lstat(name)
		if statErr != nil {
			if name == infoPath && errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return result, fmt.Errorf("%w：未生成计划文件", ErrRailpackPlan)
		}
		if !info.Mode().IsRegular() {
			return result, fmt.Errorf("%w：计划输出不是普通文件", ErrRailpackPlan)
		}
		if o.DropToUID != 0 {
			if err = os.Lchown(name, os.Getuid(), os.Getgid()); err != nil {
				return result, err
			}
		}
		if err = os.Chmod(name, 0644); err != nil {
			return result, err
		}
	}
	if o.DropToUID != 0 {
		if err = os.Chown(p.plan, os.Getuid(), os.Getgid()); err != nil {
			return result, err
		}
	}
	if err = os.Chmod(p.plan, 0755); err != nil {
		return result, err
	}
	data, err := readBoundedFile(planPath, maxRailpackPlanBytes)
	if err != nil {
		return result, fmt.Errorf("%w：计划文件无法读取或超过 %d 字节", ErrRailpackPlan, maxRailpackPlanBytes)
	}
	var plan map[string]json.RawMessage
	if json.Unmarshal(data, &plan) != nil || plan == nil {
		return result, fmt.Errorf("%w：计划文件不是合法的 JSON 对象", ErrRailpackPlan)
	}
	// info 文件即使规划失败也会写出；退出码为 0 却标记失败时同样按失败处理。
	if raw, readErr := readBoundedFile(infoPath, 8<<20); readErr == nil {
		var info struct {
			Success *bool `json:"success"`
		}
		if json.Unmarshal(raw, &info) == nil && info.Success != nil && !*info.Success {
			return result, fmt.Errorf("%w：识别结果标记为失败", ErrRailpackPlan)
		}
	}
	sum := sha256.Sum256(data)
	result.PlanSHA256 = hex.EncodeToString(sum[:])
	return result, nil
}

func mapValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// railpackEnvFlags 把构建参数与密钥名称转成 --env 参数，按名称排序保证输出稳定。
// 密钥只传占位值，真实值不进入 Railpack 进程的参数或环境。
func railpackEnvFlags(args map[string]string, secretNames []string) ([]string, error) {
	seen := make(map[string]bool, len(args)+len(secretNames))
	flags := make([]string, 0, len(args)+len(secretNames))
	for _, name := range sortedKeys(args) {
		value := args[name]
		if !railpackEnvName.MatchString(name) || seen[name] || len(value) > maxRailpackArgValue || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("%w: 构建参数", ErrInvalid)
		}
		seen[name] = true
		flags = append(flags, "--env="+name+"="+value)
	}
	for _, name := range slices.Sorted(slices.Values(secretNames)) {
		if !railpackEnvName.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("%w: 构建密钥名称", ErrInvalid)
		}
		seen[name] = true
		flags = append(flags, "--env="+name+"="+railpackSecretPlaceholder)
	}
	return flags, nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// railpackVersion 读取 Railpack 版本；无法识别时返回空串，由调用方拒绝规划。
func railpackVersion(ctx context.Context, o PlanOptions, dir string, env []string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.Binary, "--version")
	cmd.Dir = dir
	cmd.Env = env
	var buffer bytes.Buffer
	cmd.Stdout = &limitedBuffer{buffer: &buffer, limit: 512}
	if o.DropToUID != 0 {
		setCredential(cmd, o.DropToUID, o.DropToGID)
	}
	if err := runProcessGroup(ctx, cmd); err != nil {
		return ""
	}
	line := strings.TrimSpace(buffer.String())
	if len(line) > 128 || !isPrintable(line) {
		return ""
	}
	return line
}

type limitedBuffer struct {
	buffer *bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buffer.Len(); room > 0 {
		b.buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func isPrintable(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// PlanForBuild 是构建 Pod 内 runner 的 Railpack 识别步骤：使用镜像内固定的 Railpack，
// 上下文必须是源码根下的相对目录；rawArgs 是 NAME=VALUE 形式的非敏感构建参数，secretNames 只含名称。
// 识别输出与版本、计划摘要都写到 out，进入 build 容器日志。
func PlanForBuild(ctx context.Context, contextPath string, rawArgs, secretNames []string, out io.Writer) error {
	return planForBuild(ctx, contextPath, rawArgs, secretNames, out, PodRailpackBinary, fixedPaths())
}

func planForBuild(ctx context.Context, contextPath string, rawArgs, secretNames []string, out io.Writer, binary string, p paths) error {
	if !safeRelative(contextPath) {
		return ErrInvalid
	}
	args := make(map[string]string, len(rawArgs))
	for _, raw := range rawArgs {
		name, value, ok := strings.Cut(raw, "=")
		if _, duplicate := args[name]; !ok || duplicate {
			return ErrInvalid
		}
		args[name] = value
	}
	result, err := planRailpack(ctx, PlanOptions{Binary: binary, SourceDir: filepath.Join(p.workspace, "source", contextPath), Args: args, SecretNames: secretNames, Output: out}, p)
	if err != nil {
		_, _ = fmt.Fprintf(out, "Railpack 识别失败：%v\n", err)
		return err
	}
	_, _ = fmt.Fprintln(out, PlanSummary(result))
	return nil
}

// PlanSummary 是写入构建日志的计划摘要行：版本与计划摘要用于追溯本次构建使用的 Railpack。
// 宿主构建与 Pod 内 runner 使用同一格式。
func PlanSummary(result PlanResult) string {
	version := result.Version
	if version == "" {
		version = "未知"
	}
	return fmt.Sprintf("Railpack 计划已生成：版本 %s，计划摘要 sha256:%s", version, result.PlanSHA256)
}

// RailpackSecret 是一个构建密钥：ID 即 BuildKit secret 名称，Value 在构建阶段经文件提供。
type RailpackSecret struct {
	ID    string
	Value []byte
}

// RailpackMount 是交给 BuildKit 的一个 secret 挂载。
type RailpackMount struct {
	ID string
	// File 是 BuildAuthDir 下的文件名。构建密钥为 secret-<序号>（构建日志会脱敏其值）；
	// 非敏感构建参数为 arg-<序号>（值本就可见，不脱敏，避免把日志里的短值全部替换）。
	File      string
	Value     []byte
	Sensitive bool
}

// RailpackMounts 先排构建密钥（保持传入顺序），再排构建参数（按名称排序），两类的文件序号各自连续。
// Railpack 的计划把每个 --env 变量都当作 secret 名称，构建参数因此也要经 secret 提供给构建步骤。
func RailpackMounts(args map[string]string, secrets []RailpackSecret) []RailpackMount {
	mounts := make([]RailpackMount, 0, len(args)+len(secrets))
	for i, secret := range secrets {
		mounts = append(mounts, RailpackMount{ID: secret.ID, File: fmt.Sprintf("secret-%d", i), Value: secret.Value, Sensitive: true})
	}
	for i, name := range sortedKeys(args) {
		mounts = append(mounts, RailpackMount{ID: name, File: fmt.Sprintf("arg-%d", i), Value: []byte(args[name])})
	}
	return mounts
}

// RailpackBuildInput 汇总 Railpack 路径的 buildctl 参数输入。
type RailpackBuildInput struct {
	// FrontendImage 必须以 digest 钉住，只取 worker 安装配置，绝不接受构建请求内容。
	FrontendImage string
	// Context 是相对源码根的构建上下文目录。
	Context  string
	ImageTag string
	// ApplicationID 作为构建缓存键：不同应用的挂载缓存互不共享，不同应用也不会互相污染。
	ApplicationID string
	Args          map[string]string
	Secrets       []RailpackSecret
}

// RailpackBuildArgs 生成交给固定校验器的 buildctl 参数（args[0] 是子命令标记 build）及需要落盘的挂载文件。
func RailpackBuildArgs(in RailpackBuildInput) ([]string, []RailpackMount, error) {
	p := fixedPaths()
	context := filepath.Clean(filepath.Join(p.workspace, "source", filepath.FromSlash(in.Context)))
	source := filepath.Join(p.workspace, "source")
	if !railpackImagePattern.MatchString(in.FrontendImage) || !cacheKeyPattern.MatchString(in.ApplicationID) || (context != source && !strings.HasPrefix(context, source+string(filepath.Separator))) {
		return nil, nil, ErrInvalid
	}
	for name := range in.Args {
		if !railpackEnvName.MatchString(name) {
			return nil, nil, ErrInvalid
		}
	}
	for _, secret := range in.Secrets {
		if !railpackEnvName.MatchString(secret.ID) {
			return nil, nil, ErrInvalid
		}
	}
	mounts := RailpackMounts(in.Args, in.Secrets)
	args := []string{"build", "--frontend=gateway.v0",
		"--metadata-file=" + p.metadata,
		"--local=context=" + context,
		"--local=dockerfile=" + p.plan,
		"--opt=source=" + in.FrontendImage,
		"--opt=filename=" + RailpackPlanFile,
		"--opt=platform=linux/amd64",
		"--opt=build-arg:secrets-hash=" + RailpackSecretsHash(in.ApplicationID, mounts),
		"--opt=build-arg:cache-key=" + in.ApplicationID,
		"--output=type=image,name=" + in.ImageTag + ",push=true"}
	for _, mount := range mounts {
		args = append(args, "--secret=id="+mount.ID+",src="+filepath.Join(p.buildAuth, mount.File))
	}
	return args, mounts, nil
}

// RailpackSecretsHash 是 Railpack 前端用来在密钥或构建参数变化时使层缓存失效的摘要：
// 对应用标识与按名称排序、带长度前缀的名称和值求 sha256。摘要会出现在 buildctl 参数中；
// 以应用标识加盐，使其不能跨应用比对，但低熵的值仍可能被穷举，不要依赖它保密。
func RailpackSecretsHash(applicationID string, mounts []RailpackMount) string {
	sorted := slices.Clone(mounts)
	slices.SortFunc(sorted, func(a, b RailpackMount) int { return strings.Compare(a.ID, b.ID) })
	digest := sha256.New()
	digest.Write([]byte("yummy-railpack-secrets-v2\x00" + applicationID + "\x00"))
	for _, mount := range sorted {
		fmt.Fprintf(digest, "%d:%s%d:", len(mount.ID), mount.ID, len(mount.Value))
		digest.Write(mount.Value)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
