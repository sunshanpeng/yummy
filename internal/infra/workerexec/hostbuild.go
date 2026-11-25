// 宿主构建执行路径：worker 进程直接驱动节点级 rootless BuildKit 守护。
// 背景（2026-11-13 实机验收发现）：k8s pod 内嵌套 rootless 被 containerd 锁定
// 挂载与默认 seccomp 双重封锁，privileged 方案违背安全不变量；经负责人确认，
// 构建改由宿主 rootless BuildKit 常驻守护承担（专用系统用户 + systemd user 服务）。
// 隔离语义如实声明：依赖 Linux userns rootless 隔离与专用守护用户，弱于 pod
// 容器边界；部署仍走 Kubernetes pod（restricted PSA），不随本路径放宽。
// 事件/日志/摘要协议与 pod 路径完全一致：ACCEPTED（宿主稳定 UID）→
// 分块日志（maxLogChunk，与 domains/build.MaxLogChunk 保持一致）→ SUCCEEDED（metadata digest）或 FAILED。
package workerexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/buildrunner"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
)

// hostJobUIDPrefix 标记宿主构建的稳定 UID：同一 attempt 的 RECONCILE 复用同值，
// 控制面据此去重；它不对应任何 Kubernetes UID，仅是协议内的稳定标识。
const hostJobUIDPrefix = "host-"

// HostBuildFixedMetadataPath 是宿主构建的 metadata 固定路径：与 buildrunner
// 固定契约同值，构建在守护进程（专用用户）侧写入，worker（root）读取。
const HostBuildFixedMetadataPath = "/workspace/buildkit-metadata.json"

// hostBuildWorkspace 是宿主构建的固定工作根：源码由 worker(root) 准备
// （世界可读），metadata 由守护（专用用户）写入。
const hostBuildWorkspace = "/workspace"

type hostBuildExecution struct {
	cancel context.CancelFunc
	done   chan struct{}
	jobUID string
}

// 宿主构建从准备源码到日志收尾都属于同一次执行，取消需要等整条执行退出。
var hostBuildMu sync.Mutex
var hostBuildExecutions = map[string]*hostBuildExecution{}

// 不同宿主构建共用源码、计划、凭据和 metadata 路径，进程内一次只允许一个任务使用。
var hostBuildSlot = make(chan struct{}, 1)

func (e *Executor) hostJobUID(task kube.BuildTask) string {
	return hostJobUIDPrefix + task.StableJobName
}

// runHostBuild 驱动一次宿主构建：ACCEPTED → 源码准备（限界下载+SHA 校验）→
// buildctl 提交 → 分块日志 → SUCCEEDED/FAILED。RECONCILE 复用同一路径：
// BuildKit 层缓存使重放廉价，事件由控制面按稳定 UID 去重。
func (e *Executor) runHostBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, emitter Emitter) error {
	if e.options.BuildKitAddr == "" || e.options.HostBuildCtl == "" {
		return errors.New("workerexec: 宿主构建缺少 --buildkit-addr/--buildctl-path（失败关闭）")
	}
	jobUID := e.hostJobUID(task)
	if command.Action == "RECONCILE" && command.JobUid != "" {
		// 控制面已持久化宿主稳定 UID：沿用，绝不发明新值。
		jobUID = command.JobUid
	}
	hostBuildMu.Lock()
	// 受理后控制面可能同时下发 RECONCILE，已有执行会自行上报，不重复操作固定工作目录。
	if hostBuildExecutions[task.AttemptID] != nil {
		hostBuildMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	execution := &hostBuildExecution{cancel: cancel, done: make(chan struct{}), jobUID: jobUID}
	hostBuildExecutions[task.AttemptID] = execution
	hostBuildMu.Unlock()
	defer func() {
		cancel()
		hostBuildMu.Lock()
		delete(hostBuildExecutions, task.AttemptID)
		close(execution.done)
		hostBuildMu.Unlock()
	}()
	if command.Action == "RUN" {
		if err := emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "ACCEPTED", JobUid: jobUID}); err != nil {
			return err
		}
	}
	select {
	case hostBuildSlot <- struct{}{}:
		defer func() { <-hostBuildSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := e.hostBuildRun(ctx, command, task, jobUID, emitter)
	if err != nil {
		// 可运维性：错误携带 attempt 标识，区分并发/重试中的多个构建。
		return fmt.Errorf("workerexec: 宿主构建 %s: %w", command.AttemptId, err)
	}
	return nil
}

// hostSecretPath 是第 i 个构建密钥的文件路径。与 pod 路径同一契约：
// 固定目录 buildrunner.BuildAuthDir 下的 secret-<序号>，参数校验器只接受该形态。
func hostSecretPath(i int) string {
	return filepath.Join(buildrunner.BuildAuthDir, "secret-"+strconv.Itoa(i))
}

// hostBuildArgs 生成交给固定校验器的 buildctl 参数；args[0] 是校验契约的子命令标记。
func hostBuildArgs(task kube.BuildTask) []string {
	args := []string{"build", "--frontend=dockerfile.v0",
		"--metadata-file=" + HostBuildFixedMetadataPath,
		"--local=context=/workspace/source/" + task.Context,
		"--local=dockerfile=/workspace/source/" + filepath.Dir(task.Dockerfile),
		"--opt=filename=" + filepath.Base(task.Dockerfile),
		"--opt=platform=" + task.Platform,
		"--output=type=image,name=" + task.ImageTag + ",push=true"}
	if task.Target != "" {
		args = append(args, "--opt=target="+task.Target)
	}
	for _, key := range sortedBuildArgKeys(task.BuildArgs) {
		args = append(args, "--opt=build-arg:"+key+"="+task.BuildArgs[key])
	}
	for i, secret := range task.Secrets {
		args = append(args, "--secret=id="+secret.ID+",src="+hostSecretPath(i))
	}
	return args
}

// writeHostSecrets 把构建密钥写成 dir 下的 secret-<序号> 文件。目录先整体清掉
// （上次异常退出可能残留旧密钥），再以 0700 重建，文件 0600；没有密钥时不创建目录。
// buildctl 在客户端读取这些文件并经会话交给守护进程，守护用户不需要读取权限。
func writeHostSecrets(dir string, secrets []kube.BuildSecret) error {
	files := make([]hostMountFile, 0, len(secrets))
	for i, secret := range secrets {
		files = append(files, hostMountFile{name: "secret-" + strconv.Itoa(i), value: secret.Value})
	}
	return writeHostFiles(dir, files)
}

// writeHostMounts 写 Railpack 路径的挂载文件：构建密钥为 secret-<序号>，非敏感构建参数为 arg-<序号>，
// 目录与权限规则和 writeHostSecrets 相同。
func writeHostMounts(dir string, mounts []buildrunner.RailpackMount) error {
	files := make([]hostMountFile, 0, len(mounts))
	for _, mount := range mounts {
		files = append(files, hostMountFile{name: mount.File, value: mount.Value})
	}
	return writeHostFiles(dir, files)
}

type hostMountFile struct {
	name  string
	value []byte
}

func writeHostFiles(dir string, files []hostMountFile) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// MkdirAll 受 umask 影响，显式设置才能保证 0700。
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.name), file.value, 0600); err != nil {
			return err
		}
	}
	return nil
}

// hostRedactionSecrets 汇总需要从构建输出脱敏的已知明文：仓库凭据（与 Pod 路径同一规则，
// 含 base64 解码形态）与全部构建密钥值。仓库凭据形态不合规时失败关闭。
func hostRedactionSecrets(task kube.BuildTask) ([][]byte, error) {
	var known [][]byte
	if len(task.RegistryDockerConfig) > 0 {
		registry, err := buildrunner.RegistrySecrets(task.RegistryDockerConfig)
		if err != nil {
			return nil, err
		}
		known = append(known, registry...)
	}
	for _, secret := range task.Secrets {
		known = append(known, secret.Value)
	}
	return known, nil
}

// hostLogWriter 把写入按协议块上限切分后上报。脱敏器会补回上次扣留的尾部，
// [REDACTED] 占位也可能比原文长，单次写入可能超过 maxLogChunk；
// 直接交给 emitChunk 会被截断而丢字节，所以在这里切分。
type hostLogWriter struct{ emit func([]byte) }

func (w hostLogWriter) Write(p []byte) (int, error) {
	for rest := p; len(rest) > 0; {
		n := min(len(rest), maxLogChunk)
		w.emit(rest[:n])
		rest = rest[n:]
	}
	return len(p), nil
}

func (e *Executor) hostBuildRun(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, jobUID string, emitter Emitter) error {
	railpack := task.Method == kube.MethodRailpack
	if railpack && !e.hostRailpackReady() {
		return e.railpackUnavailable(command, jobUID, emitter)
	}
	var args []string
	var mounts []buildrunner.RailpackMount
	if railpack {
		var err error
		if args, mounts, err = e.hostRailpackArgs(task); err != nil {
			return fmt.Errorf("workerexec: Railpack 构建参数无效: %w", err)
		}
	} else {
		args = hostBuildArgs(task)
	}
	if err := buildrunner.ValidateBuildArgs(args); err != nil {
		// 可运维性：只输出参数键名（不含值，防泄漏 build-arg 敏感值）。
		keys := make([]string, 0, len(args))
		for _, arg := range args {
			key := arg
			for i := 0; i < len(key); i++ {
				if key[i] == '=' {
					key = key[:i]
					break
				}
			}
			keys = append(keys, key)
		}
		return fmt.Errorf("workerexec: 构建参数校验失败 %w（键名序列: %v）", err, keys)
	}
	// 尽早汇总脱敏表：仓库凭据形态不合规时在写任何文件、启动任何进程之前失败关闭。
	redactionSecrets, err := hostRedactionSecrets(task)
	if err != nil {
		return fmt.Errorf("workerexec: 构建输出脱敏表生成失败: %w", err)
	}

	// 固定工作根：worker(root) 准备源码（世界可读供守护读取），守护写 metadata。
	if err := os.MkdirAll(hostBuildWorkspace, 0755); err != nil {
		return err
	}
	_ = os.Remove(HostBuildFixedMetadataPath)

	// RECONCILE 可能不携带源码授权令牌（如 worker 重启后的恢复重投）：
	// 无在飞进程且无已落盘 metadata 时无法恢复证据，如实报错，不伪造终态。
	if task.Source.Token == nil || len(task.Source.Token) == 0 {
		if _, err := os.Stat(HostBuildFixedMetadataPath); err == nil {
			data, err := os.ReadFile(HostBuildFixedMetadataPath)
			if err == nil {
				if digest, err := buildrunner.HostBuildDigest(data); err == nil && digest != "" {
					return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "SUCCEEDED", JobUid: jobUID, Digest: digest, LogsComplete: true})
				}
			}
		}
		return fmt.Errorf("workerexec: RECONCILE 缺少源码授权令牌且无可信 metadata")
	}
	// 清理上一次构建残留：PrepareSource 要求解包目标为空目录。
	if err := os.RemoveAll("/workspace/source"); err != nil {
		return err
	}
	// 源码准备：令牌必须落在固定契约路径（prepareSource 强校验 TokenFile ==
	// /run/source-auth/source-token）；目录由 worker(root) 创建，0700 保护令牌。
	if err := os.MkdirAll("/run/source-auth", 0700); err != nil {
		return err
	}
	const hostTokenPath = "/run/source-auth/source-token"
	if err := os.WriteFile(hostTokenPath, task.Source.Token, 0600); err != nil {
		return err
	}
	defer os.Remove(hostTokenPath)
	tokenFile := hostTokenPath
	if err := buildrunner.PrepareSource(ctx, buildrunner.PrepareOptions{
		URI: task.Source.URI, TokenFile: tokenFile,
		ArchiveSHA256: task.Source.ArchiveSHA256,
		MaxBytes:      task.Source.MaxBytes,
		Destination:   "/workspace/source",
		Dockerfile:    task.Dockerfile,
	}); err != nil {
		return err
	}
	// 守护（专用用户）需读取源码上下文：目录 0755、文件 0644。
	if err := hostWorldReadable("/workspace/source"); err != nil {
		return err
	}

	// registry 凭据：0600 临时目录，仅 buildctl 进程经 DOCKER_CONFIG 读取。
	credDir, err := os.MkdirTemp("", "yummy-cred-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(credDir)
	if err := os.WriteFile(filepath.Join(credDir, "config.json"), task.RegistryDockerConfig, 0600); err != nil {
		return err
	}

	// 构建密钥文件：固定目录、0700/0600，构建结束（含失败、取消）无条件清理。
	// defer 先于写入登记，写到一半失败也不会残留。
	defer os.RemoveAll(buildrunner.BuildAuthDir)
	if railpack {
		err = writeHostMounts(buildrunner.BuildAuthDir, mounts)
	} else {
		err = writeHostSecrets(buildrunner.BuildAuthDir, task.Secrets)
	}
	if err != nil {
		return err
	}

	// 日志脱敏与 Pod 路径同一实现：仓库凭据与构建密钥的已知明文替换为 [REDACTED]。
	// 脱敏器会扣留最长秘密长度的尾部，输出读完后必须 Close 才会冲出；识别输出与构建输出共用同一条日志序列。
	logSeq := 0
	var logMu sync.Mutex
	emitChunk := func(content []byte) {
		if len(content) == 0 {
			return
		}
		logMu.Lock()
		logSeq++
		seq := logSeq
		logMu.Unlock()
		chunk := content
		if len(chunk) > maxLogChunk {
			chunk = chunk[:maxLogChunk]
		}
		_ = emitter.BuildLog(&platformv1.WorkerBuildLog{AttemptId: command.AttemptId, Seq: int64(seq), Content: chunk})
	}
	redactor := buildrunner.NewRedactingWriter(hostLogWriter{emit: emitChunk}, redactionSecrets)
	// 提前返回的路径也要冲出被扣留的尾部；重复 Close 无副作用。
	defer redactor.Close()

	if railpack {
		// 识别阶段：Railpack 只读取源码并生成计划，不运行项目脚本；失败原因进入构建日志。
		if err := e.hostStage(command, jobUID, emitter, "PLANNING"); err != nil {
			return err
		}
		uid, gid := railpackDropIdentity()
		result, planErr := buildrunner.PlanRailpack(ctx, buildrunner.PlanOptions{
			Binary: e.options.RailpackPath, SourceDir: filepath.Join(hostBuildWorkspace, "source", filepath.FromSlash(task.Context)),
			Args: task.BuildArgs, SecretNames: secretIDs(task.Secrets), DropToUID: uid, DropToGID: gid, Output: redactor,
		})
		if planErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			code := railpackFailureCode(planErr)
			if code == "" {
				// 目录不可写等宿主环境问题不是用户可修复的失败，交给重投而不是伪造构建失败。
				return planErr
			}
			_, _ = fmt.Fprintf(redactor, "Railpack 识别失败：%v\n", planErr)
			_ = redactor.Close()
			return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "FAILED", JobUid: jobUID, ErrorCode: code, LogsComplete: true})
		}
		_, _ = fmt.Fprintln(redactor, buildrunner.PlanSummary(result))
		if err := e.hostStage(command, jobUID, emitter, "BUILDING"); err != nil {
			return err
		}
	}

	// buildctl 提交：构建在守护进程侧执行；stdout/stderr 合流后分块上报。
	// args[0] 是固定校验契约的子命令标记，buildctl 的 build 子命令参数就是 args 本身；
	// metadata 由 buildctl --metadata-file 写入固定路径（与 pod 路径同契约）。
	cmdArgs := append([]string{"--addr", e.options.BuildKitAddr}, args...)
	cmd := exec.CommandContext(ctx, e.options.HostBuildCtl, cmdArgs...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "DOCKER_CONFIG=" + credDir, "TMPDIR=" + credDir}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = redactor
	cmd.Stderr = redactor
	if err := cmd.Start(); err != nil {
		return err
	}
	err = cmd.Wait()
	// 先冲出被扣留的尾部，终态事件携带 LogsComplete 时日志必须已经完整上报。
	_ = redactor.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "FAILED", JobUid: jobUID, ErrorCode: "BUILD_EXEC_FAILED", LogsComplete: true})
	}

	// 摘要：只取守护写入 metadata 的可信 digest，绝不由日志文本推断。
	data, err := os.ReadFile(HostBuildFixedMetadataPath)
	if err != nil {
		return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "FAILED", JobUid: jobUID, ErrorCode: "ARTIFACT_METADATA_MISSING", LogsComplete: true})
	}
	digest, err := buildrunner.HostBuildDigest(data)
	if err != nil {
		return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "FAILED", JobUid: jobUID, ErrorCode: "ARTIFACT_METADATA_MISSING", LogsComplete: true})
	}
	return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "SUCCEEDED", JobUid: jobUID, Digest: digest, LogsComplete: true})
}

// cancelHostBuild 取消在飞执行并等待源码准备、规划、构建和清理全部退出后才发射 STOPPED。
func (e *Executor) cancelHostBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, emitter Emitter) error {
	hostBuildMu.Lock()
	execution := hostBuildExecutions[command.AttemptId]
	hostBuildMu.Unlock()
	jobUID := command.JobUid
	if execution != nil {
		execution.cancel()
		select {
		case <-execution.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if jobUID == "" {
			jobUID = execution.jobUID
		}
	}
	return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "STOPPED", JobUid: jobUID, LogsComplete: true})
}

// hostWorldReadable 让守护用户读取 worker(root) 准备的源码上下文。
func hostWorldReadable(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0755)
		}
		return os.Chmod(path, 0644)
	})
}

func sortedBuildArgKeys(args map[string]string) []string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
