package buildrunner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func registrySecrets(data []byte) ([][]byte, error) {
	var config struct {
		Auths map[string]struct{ Auth, Username, Password, IdentityToken, RegistryToken string } `json:"auths"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil || len(config.Auths) == 0 {
		return nil, fmt.Errorf("registry auth只允许固定auths，不允许凭据helper执行器")
	}
	secrets := [][]byte{data}
	for _, a := range config.Auths {
		for _, value := range []string{a.Auth, a.Password, a.IdentityToken, a.RegistryToken} {
			if value != "" {
				secrets = append(secrets, []byte(value))
			}
		}
		if a.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, ErrInvalid
			}
			secrets = append(secrets, decoded)
			if _, password, ok := strings.Cut(string(decoded), ":"); ok && password != "" {
				secrets = append(secrets, []byte(password))
			}
		}
	}
	return secrets, nil
}

// ensureOwnedRuntime 在运行卷挂载内创建属主为当前用户的私有运行目录。
// 实机发现（2026-11-13）：kubelet 以 root 属主、1777 模式挂载 tmpfs 运行卷，
// fsGroup 不改变 tmpfs 属主，uid 1000 对挂载根的 chmod 必然 EPERM；
// 世界可写挂载内由当前用户创建的目录天然属主为 uid 1000，可安全 chmod 0700。
func ensureOwnedRuntime(mount string) (string, error) {
	dir, err := os.MkdirTemp(mount, "xdg-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func prepareCredentials(p paths, runtime string) (string, [][]byte, error) {
	data, err := readBoundedFile(filepath.Join(p.buildAuth, "config.json"), 256<<10)
	if err != nil {
		return "", nil, fmt.Errorf("缺少registry配置")
	}
	secrets, err := registrySecrets(data)
	if err != nil {
		return "", nil, err
	}
	credentials, err := os.MkdirTemp(runtime, "registry-")
	if err != nil {
		return "", nil, err
	}
	// credentials由固定tmpfs根内MkdirTemp产生；清理目标不由用户参数计算。
	if err = os.WriteFile(filepath.Join(credentials, "config.json"), data, 0600); err != nil {
		_ = os.RemoveAll(credentials)
		return "", nil, err
	}
	entries, err := os.ReadDir(p.buildAuth)
	if err != nil {
		_ = os.RemoveAll(credentials)
		return "", nil, err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "secret-") {
			continue
		}
		value, err := readBoundedFile(filepath.Join(p.buildAuth, entry.Name()), 64<<10)
		if err != nil {
			_ = os.RemoveAll(credentials)
			return "", nil, fmt.Errorf("读取BuildKit secret失败")
		}
		secrets = append(secrets, value)
	}
	return credentials, secrets, nil
}

// RunBuildKit只执行固定镜像内部的可信daemonless入口，绝不拼Shell或接受执行器路径。
// noProcessSandbox只能由已通过资格的安装配置显式开启；普通用户不能提供该开关。
func RunBuildKit(ctx context.Context, args []string, noProcessSandbox bool, stdout, stderr io.Writer) error {
	return runBuildKit(ctx, args, noProcessSandbox, stdout, stderr, fixedPaths())
}
func runBuildKit(ctx context.Context, args []string, noProcessSandbox bool, stdout, stderr io.Writer, p paths) error {
	if err := validateBuildArgs(args, p); err != nil {
		return err
	}
	if stdout == nil || stderr == nil {
		return ErrInvalid
	}
	// 运行卷挂载根不可 chmod（见 ensureOwnedRuntime 注释）：在挂载内取私有运行目录，
	// XDG_RUNTIME_DIR 与 TMPDIR 都指向该私有目录，退出时整体清理。
	runtime, err := ensureOwnedRuntime(p.runtime)
	if err != nil {
		return err
	}
	defer os.RemoveAll(runtime)
	credentials, secrets, err := prepareCredentials(p, runtime)
	if err != nil {
		return err
	}
	defer os.RemoveAll(credentials)
	out := newRedactingWriter(stdout, secrets)
	defer out.Close()
	errOut := newRedactingWriter(stderr, secrets)
	defer errOut.Close()
	cmd := exec.CommandContext(ctx, p.script, args...)
	// 不继承可能注入executor/自定义buildkitd flags的环境，私有CA安装在固定镜像系统信任库。
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/user", "USER=user", "XDG_RUNTIME_DIR=" + runtime, "DOCKER_CONFIG=" + credentials, "TMPDIR=" + runtime}
	if noProcessSandbox {
		cmd.Env = append(cmd.Env, "BUILDKITD_FLAGS=--oci-worker-no-process-sandbox")
	}
	cmd.Stdout = out
	cmd.Stderr = errOut
	err = runProcessGroup(ctx, cmd)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("BuildKit执行失败")
	}
	metadata, err := readBoundedFile(p.metadata, 1<<20)
	if err != nil {
		return fmt.Errorf("BuildKit未产生metadata")
	}
	compact, err := resultMetadata(metadata)
	if err != nil {
		return err
	}
	return os.WriteFile(p.result, compact, 0600)
}
func resultMetadata(data []byte) ([]byte, error) {
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("BuildKit metadata过大")
	}
	var metadata struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || !digestPattern.MatchString(metadata.Digest) {
		return nil, fmt.Errorf("BuildKit metadata缺少digest")
	}
	compact, _ := json.Marshal(metadata)
	return compact, nil
}

// HostBuildDigest 解析宿主构建 metadata 的可信 digest（containerimage.digest），
// 供 workerexec 宿主构建路径复用同一摘要契约；绝不从日志文本推断。
func HostBuildDigest(data []byte) (string, error) {
	compact, err := resultMetadata(data)
	if err != nil {
		return "", err
	}
	var metadata struct {
		Digest string `json:"containerimage.digest"`
	}
	_ = json.Unmarshal(compact, &metadata)
	return metadata.Digest, nil
}
