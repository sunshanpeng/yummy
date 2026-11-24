package workerexec

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/buildrunner"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
)

// 宿主构建的真实执行需要 root、/workspace 与节点级 BuildKit 守护，单元测试只覆盖
// 其中可隔离的部分：参数契约、密钥文件、日志切块与脱敏。

func hostTestTask(secrets ...kube.BuildSecret) kube.BuildTask {
	return kube.BuildTask{
		AttemptID:  testAttempt,
		Platform:   kube.Platform,
		ImageTag:   "registry.example.com/app:build-1",
		Dockerfile: "Dockerfile",
		Context:    ".",
		Secrets:    secrets,
	}
}

// 回归：宿主路径曾把密钥写成 /workspace/secrets/secret-N，被固定校验器拒绝，
// 导致配置了构建密钥的应用在宿主模式下无法构建。
func TestHostBuildArgsPassFixedValidator(t *testing.T) {
	secrets := []kube.BuildSecret{{ID: "npm", Value: []byte("npm-token")}, {ID: "pip_index", Value: []byte("pip-token")}}
	cases := map[string]kube.BuildTask{
		"无密钥":  hostTestTask(),
		"两个密钥": hostTestTask(secrets...),
	}
	withExtras := hostTestTask(secrets...)
	withExtras.Target = "release"
	withExtras.BuildArgs = map[string]string{"MODE": "literal; not-shell", "A_FIRST": "1"}
	withExtras.Context = "services/api"
	withExtras.Dockerfile = "services/api/build/Dockerfile"
	cases["目标阶段、构建参数与子目录上下文"] = withExtras

	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			args := hostBuildArgs(task)
			if err := buildrunner.ValidateBuildArgs(args); err != nil {
				t.Fatalf("宿主构建参数被固定校验器拒绝: %v\n%v", err, args)
			}
			var secretArgs []string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--secret=") {
					secretArgs = append(secretArgs, arg)
				}
			}
			if len(secretArgs) != len(task.Secrets) {
				t.Fatalf("密钥参数数量不符: %v", secretArgs)
			}
			for i, arg := range secretArgs {
				want := "--secret=id=" + task.Secrets[i].ID + ",src=" + buildrunner.BuildAuthDir + "/secret-" + string(rune('0'+i))
				if arg != want {
					t.Fatalf("密钥参数 = %q，期望 %q", arg, want)
				}
			}
		})
	}
}

func TestWriteHostSecretsPrivateFilesAndCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "build-auth")
	secrets := []kube.BuildSecret{{ID: "npm", Value: []byte("npm-token")}, {ID: "pip", Value: []byte("pip-token")}}

	// 上次异常退出可能残留旧文件：写入前必须整体清掉，旧密钥不能留在目录里。
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret-9"), []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeHostSecrets(dir, secrets); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("目录应为 0700: %v %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("应只含本次的两个密钥文件: %v %v", entries, err)
	}
	for i, secret := range secrets {
		path := filepath.Join(dir, "secret-"+string(rune('0'+i)))
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, secret.Value) {
			t.Fatalf("%s 内容不符: %q %v", path, data, err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s 应为 0600: %v %v", path, info, err)
		}
	}

	// 本次没有密钥：清掉残留并且不创建目录。
	if err := writeHostSecrets(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("无密钥时目录应已清理: %v", err)
	}
}

func TestHostLogWriterSplitsOversizedWrites(t *testing.T) {
	var chunks [][]byte
	writer := hostLogWriter{emit: func(content []byte) { chunks = append(chunks, append([]byte(nil), content...)) }}
	input := bytes.Repeat([]byte("0123456789"), (3*maxLogChunk+5)/10+1)
	input = input[:3*maxLogChunk+5]

	n, err := writer.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	var joined []byte
	for _, chunk := range chunks {
		if len(chunk) == 0 || len(chunk) > maxLogChunk {
			t.Fatalf("块大小越界: %d", len(chunk))
		}
		joined = append(joined, chunk...)
	}
	if !bytes.Equal(joined, input) {
		t.Fatalf("切块后丢失或错位了字节: got %d want %d", len(joined), len(input))
	}
}

// 用生产同款的 dockerConfigJSON 构造凭据，覆盖 auth 的 base64 形态与解码后的密码。
func hostTestRegistryConfig(t *testing.T, password string) []byte {
	t.Helper()
	config, err := dockerConfigJSON(&platformv1.WorkerRegistryCredential{Origin: "https://registry.example.com:5000", Username: "robot", Password: []byte(password)})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestHostBuildLogRedaction(t *testing.T) {
	const password = "registry-PASSWORD-123"
	task := hostTestTask(kube.BuildSecret{ID: "npm", Value: []byte("npm-TOKEN-456")})
	task.RegistryDockerConfig = hostTestRegistryConfig(t, password)
	basic := base64.StdEncoding.EncodeToString([]byte("robot:" + password))

	known, err := hostRedactionSecrets(task)
	if err != nil {
		t.Fatal(err)
	}
	// 秘密分布在日志的任意位置，且大于一个协议块：脱敏补回扣留尾部后单次输出会超过 maxLogChunk，
	// 此时不能被截断丢字节。
	filler := strings.Repeat("build log line\n", maxLogChunk/len("build log line\n")+10)
	input := filler + "login " + password + " and " + basic + "\n" + filler + "token=npm-TOKEN-456 end"

	want := strings.NewReplacer(password, "[REDACTED]", basic, "[REDACTED]", "npm-TOKEN-456", "[REDACTED]").Replace(input)

	// 7919 字节的不规则小块使秘密跨越写入边界；整段一次写入则让脱敏器单次输出远超 maxLogChunk，
	// 验证切块后既不超过协议上限也不丢字节。
	for _, blockSize := range []int{7919, len(input)} {
		var chunks [][]byte
		sink := hostLogWriter{emit: func(content []byte) { chunks = append(chunks, append([]byte(nil), content...)) }}
		writer := buildrunner.NewRedactingWriter(sink, known)
		for rest := []byte(input); len(rest) > 0; {
			n := min(len(rest), blockSize)
			if _, err := writer.Write(rest[:n]); err != nil {
				t.Fatal(err)
			}
			rest = rest[n:]
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		var output []byte
		for _, chunk := range chunks {
			if len(chunk) > maxLogChunk {
				t.Fatalf("块大小 %d：块超过协议上限: %d", blockSize, len(chunk))
			}
			output = append(output, chunk...)
		}
		for _, leaked := range []string{password, basic, "npm-TOKEN-456"} {
			if bytes.Contains(output, []byte(leaked)) {
				t.Fatalf("块大小 %d：输出泄露了 %q", blockSize, leaked)
			}
		}
		if string(output) != want {
			t.Fatalf("块大小 %d：脱敏结果与期望不符（长度 got=%d want=%d）", blockSize, len(output), len(want))
		}
	}
}

func TestHostRedactionSecretsWithoutRegistryConfig(t *testing.T) {
	known, err := hostRedactionSecrets(hostTestTask(kube.BuildSecret{ID: "npm", Value: []byte("only-secret")}))
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 1 || string(known[0]) != "only-secret" {
		t.Fatalf("没有仓库凭据时只应脱敏构建密钥: %q", known)
	}
	// 仓库凭据形态不合规（含凭据 helper）必须失败关闭，不能带着未脱敏的日志继续构建。
	task := hostTestTask()
	task.RegistryDockerConfig = []byte(`{"auths":{},"credsStore":"evil"}`)
	if _, err := hostRedactionSecrets(task); err == nil {
		t.Fatal("凭据 helper 形态未被拒绝")
	}
}
