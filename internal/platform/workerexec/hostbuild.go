// 宿主构建执行路径：worker 进程直接驱动节点级 rootless BuildKit 守护。
// 背景（2026-11-13 实机验收发现）：k8s pod 内嵌套 rootless 被 containerd 锁定
// 挂载与默认 seccomp 双重封锁，privileged 方案违背安全不变量；经负责人确认，
// 构建改由宿主 rootless BuildKit 常驻守护承担（专用系统用户 + systemd user 服务）。
// 隔离语义如实声明：依赖 Linux userns rootless 隔离与专用守护用户，弱于 pod
// 容器边界；部署仍走 Kubernetes pod（restricted PSA），不随本路径放宽。
// 事件/日志/摘要协议与 pod 路径完全一致：ACCEPTED（宿主稳定 UID）→
// 分块日志（MaxLogChunk）→ SUCCEEDED（metadata digest）或 FAILED。
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

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/buildrunner"
	"github.com/sunshanpeng/yummy/internal/platform/kube"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
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

// hostBuildMu 保护在飞构建进程登记；CANCEL 据此终止并等待真实退出。
var hostBuildMu sync.Mutex
var hostBuildProcs = map[string]*exec.Cmd{}

func (e *Executor) hostJobUID(task kube.BuildTask) string {
	return hostJobUIDPrefix + task.StableJobName
}

func (e *Executor) registerHostProc(attemptID string, cmd *exec.Cmd) {
	hostBuildMu.Lock()
	hostBuildProcs[attemptID] = cmd
	hostBuildMu.Unlock()
}

func (e *Executor) unregisterHostProc(attemptID string) {
	hostBuildMu.Lock()
	delete(hostBuildProcs, attemptID)
	hostBuildMu.Unlock()
}

// runHostBuild 驱动一次宿主构建：ACCEPTED → 源码准备（限界下载+SHA 校验）→
// buildctl 提交 → 分块日志 → SUCCEEDED/FAILED。RECONCILE 复用同一路径：
// BuildKit 层缓存使重放廉价，事件由控制面按稳定 UID 去重。
func (e *Executor) runHostBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, emitter workergateway.Emitter) error {
	if e.options.BuildKitAddr == "" || e.options.HostBuildCtl == "" {
		return errors.New("workerexec: 宿主构建缺少 --buildkit-addr/--buildctl-path（失败关闭）")
	}
	jobUID := e.hostJobUID(task)
	if command.Action == "RECONCILE" && command.JobUid != "" {
		// 控制面已持久化宿主稳定 UID：沿用，绝不发明新值。
		jobUID = command.JobUid
	}
	if command.Action == "RUN" {
		if err := emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "ACCEPTED", JobUid: jobUID}); err != nil {
			return err
		}
	}
	err := e.hostBuildRun(ctx, command, task, jobUID, emitter)
	if err != nil {
		// 可运维性：错误携带 attempt 标识，区分并发/重试中的多个构建。
		return fmt.Errorf("workerexec: 宿主构建 %s: %w", command.AttemptId, err)
	}
	return nil
}

func (e *Executor) hostBuildRun(ctx context.Context, command *platformv1.WorkerBuildCommand, task kube.BuildTask, jobUID string, emitter workergateway.Emitter) error {
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
	// 与 pod 路径同一密钥键名契约（secret-N）：校验器只接受该形态的 src 基名。
	for i, secret := range task.Secrets {
		args = append(args, "--secret=id="+secret.ID+",src=/workspace/secrets/secret-"+strconv.Itoa(i))
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

	// buildctl 提交：构建在守护进程侧执行；stdout/stderr 合流后分块上报。
	// args[0] 是固定校验契约的子命令标记；buildctl 实际参数从 args[1:] 起，
	// metadata 由 buildctl --metadata-file 写入固定路径（与 pod 路径同契约）。
	cmdArgs := []string{"--addr", e.options.BuildKitAddr,
		"build", "--metadata-file=" + HostBuildFixedMetadataPath}
	cmdArgs = append(cmdArgs, args[1:]...)
	cmd := exec.CommandContext(ctx, e.options.HostBuildCtl, cmdArgs...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "DOCKER_CONFIG=" + credDir, "TMPDIR=" + credDir}
	e.registerHostProc(task.AttemptID, cmd)
	defer e.unregisterHostProc(task.AttemptID)

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
		if len(chunk) > build.MaxLogChunk {
			chunk = chunk[:build.MaxLogChunk]
		}
		_ = emitter.BuildLog(&platformv1.WorkerBuildLog{AttemptId: command.AttemptId, Seq: int64(seq), Content: chunk})
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer := make([]byte, build.MaxLogChunk)
		for {
			n, readErr := pr.Read(buffer)
			if n > 0 {
				emitChunk(buffer[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()
	err = cmd.Wait()
	pw.Close()
	wg.Wait()
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

// cancelHostBuild 终止在飞构建进程并等待真实退出后才发射 STOPPED；
// 没有在飞进程时按"已消失"处理（RECONCILE 重跑自会恢复一致）。
func (e *Executor) cancelHostBuild(ctx context.Context, command *platformv1.WorkerBuildCommand, emitter workergateway.Emitter) error {
	hostBuildMu.Lock()
	cmd, ok := hostBuildProcs[command.AttemptId]
	hostBuildMu.Unlock()
	if ok && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "STOPPED", JobUid: command.JobUid, LogsComplete: true})
}

func writeHostTempFile(pattern string, content []byte) (string, error) {
	file, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), file.Close()
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
