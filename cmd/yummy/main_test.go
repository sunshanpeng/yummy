package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sunshanpeng/yummy/internal/transport/clientcli"
)

// 子进程直接运行 CLI 入口，验证调用方实际收到的退出码和输出。
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "workspace" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestUnauthenticatedExitCode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "workspace", "list", "--profile", "default",
		"--config-dir", filepath.Join(t.TempDir(), "config"), "--server", "https://example.invalid")
	cmd.Env = append(os.Environ(), "YUMMY_TOKEN=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("未认证应以退出码 3 结束，实际为 %v，stderr 为 %q", err, stderr.String())
	}
	if len(stdout) != 0 {
		t.Fatalf("命令失败时 stdout 应为空，实际为 %q", stdout)
	}
	var diagnostic clientcli.Error
	if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
		t.Fatalf("stderr 应为 JSON 错误，实际为 %q：%v", stderr.String(), err)
	}
	if diagnostic.Code != "UNAUTHENTICATED" || diagnostic.Exit != 3 {
		t.Fatalf("错误类型或退出码有误：%+v", diagnostic)
	}
}
