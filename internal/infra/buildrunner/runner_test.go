package buildrunner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testPaths(t *testing.T) paths {
	t.Helper()
	root := t.TempDir()
	p := paths{workspace: filepath.Join(root, "workspace"), sourceAuth: filepath.Join(root, "source-auth"), buildAuth: filepath.Join(root, "build-auth"), runtime: filepath.Join(root, "runtime"), result: filepath.Join(root, "result"), script: filepath.Join(root, "fixed-script")}
	p.sourceToken = filepath.Join(p.sourceAuth, "source-token")
	p.metadata = filepath.Join(p.workspace, "buildkit-metadata.json")
	for _, d := range []string{p.workspace, p.sourceAuth, p.buildAuth, p.runtime} {
		mode := os.FileMode(0700)
		if d == p.runtime {
			// 运行卷挂载根在实机为 root 属主 1777；测试以 0777 模拟世界可写挂载。
			mode = 0777
		}
		if err := os.Mkdir(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p.sourceToken, []byte("grant-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func validArgs(p paths) []string {
	return []string{"build", "--frontend=dockerfile.v0", "--metadata-file=" + p.metadata, "--local=context=" + filepath.Join(p.workspace, "source"), "--local=dockerfile=" + filepath.Join(p.workspace, "source"), "--opt=filename=Dockerfile", "--opt=platform=linux/amd64", "--output=type=image,name=registry.example/app:build-1,push=true", "--opt=build-arg:MODE=literal; not-shell", "--secret=id=npm,src=" + filepath.Join(p.buildAuth, "secret-0")}
}
func archiveBytes(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("FROM scratch\n")
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(content)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestPrepareVerifiedDownloadAndAtomicSource(t *testing.T) {
	data := archiveBytes(t, "Dockerfile")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer grant-secret" {
			t.Error("grant未通过header传递")
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client := sourceClient(roots)
	defer client.CloseIdleConnections()
	for _, mode := range []string{"valid", "bad-digest", "query-token", "traversal", "too-large", "redirect", "untrusted-ca"} {
		t.Run(mode, func(t *testing.T) {
			p := testPaths(t)
			sum := sha256.Sum256(data)
			o := PrepareOptions{URI: server.URL, TokenFile: p.sourceToken, ArchiveSHA256: "sha256:" + hex.EncodeToString(sum[:]), Destination: filepath.Join(p.workspace, "source"), Dockerfile: "Dockerfile", MaxBytes: int64(len(data) + 1)}
			useClient := client
			switch mode {
			case "bad-digest":
				o.ArchiveSHA256 = "sha256:" + strings.Repeat("0", 64)
			case "query-token":
				o.URI += "?token=bad"
			case "traversal":
				o.Dockerfile = "../outside"
			case "too-large":
				o.MaxBytes = 1
			case "redirect":
				redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, http.StatusFound) }))
				defer redirect.Close()
				o.URI = redirect.URL
			case "untrusted-ca":
				useClient = sourceClient(x509.NewCertPool())
				defer useClient.CloseIdleConnections()
			}
			err := prepareSource(context.Background(), o, p, useClient)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(filepath.Join(o.Destination, "Dockerfile"))
				if err != nil || string(content) != "FROM scratch\n" {
					t.Fatal(err)
				}
				if err := prepareSource(context.Background(), o, p, useClient); err == nil {
					t.Fatal("已有source不得复用")
				}
			} else {
				if err == nil {
					t.Fatal("无效下载未拒绝")
				}
				if _, err := os.Stat(o.Destination); !os.IsNotExist(err) {
					t.Fatal("失败后不能发布source目录")
				}
			}
		})
	}
}
func TestPrepareRejectsUnsafeArchive(t *testing.T) {
	data := archiveBytes(t, "../Dockerfile")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client := sourceClient(roots)
	defer client.CloseIdleConnections()
	p := testPaths(t)
	sum := sha256.Sum256(data)
	if err := prepareSource(context.Background(), PrepareOptions{URI: server.URL, TokenFile: p.sourceToken, ArchiveSHA256: "sha256:" + hex.EncodeToString(sum[:]), Destination: filepath.Join(p.workspace, "source"), Dockerfile: "Dockerfile", MaxBytes: 1 << 20}, p, client); err == nil {
		t.Fatal("路径穿越归档未拒绝")
	}
}
func TestStrictBuildctlArguments(t *testing.T) {
	p := testPaths(t)
	args := validArgs(p)
	if err := validateBuildArgs(args, p); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"--allow=network.host", "--addr=unix:///host/docker.sock", "--import-cache=type=local,src=/host", "--opt=platform=linux/arm64", "--opt=filename=../Dockerfile", "--local=context=/host", "--secret=id=x,src=/host/token", "--ssh=default", "--output=type=local,dest=/host"} {
		if err := validateBuildArgs(append(append([]string{}, args...), extra), p); err == nil {
			t.Fatalf("未拒绝%s", extra)
		}
	}
	args[3] = "--local=context=" + p.workspace + "/source/../../host"
	if err := validateBuildArgs(args, p); err == nil {
		t.Fatal("路径逃逸未拒绝")
	}
}
func TestRedactionCrossWriteAndFlush(t *testing.T) {
	for split := 0; split <= len("abcTOP-SECRETdef"); split++ {
		var output bytes.Buffer
		r := newRedactingWriter(&output, [][]byte{[]byte("TOP-SECRET"), []byte("SECRET")})
		input := []byte("abcTOP-SECRETdef")
		_, _ = r.Write(input[:split])
		_, _ = r.Write(input[split:])
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if output.String() != "abc[REDACTED]def" {
			t.Fatal(split, output.String())
		}
	}
}
func TestRegistryCredentials0600AndHelperForbidden(t *testing.T) {
	p := testPaths(t)
	config := []byte(`{"auths":{"registry.example":{"auth":"dXNlcjpwYXNzd29yZA=="}}}`)
	_ = os.WriteFile(filepath.Join(p.buildAuth, "config.json"), config, 0400)
	_ = os.WriteFile(filepath.Join(p.buildAuth, "secret-0"), []byte("buildsecret"), 0400)
	// 模拟实机形态：挂载根世界可写（root 属主 1777），私有运行目录由 ensureOwnedRuntime 创建。
	if err := os.Chmod(p.runtime, 0777); err != nil {
		t.Fatal(err)
	}
	runtime, err := ensureOwnedRuntime(p.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(runtime)
	dir, secrets, err := prepareCredentials(p, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	found := false
	for _, s := range secrets {
		if string(s) == "password" {
			found = true
		}
	}
	if !found {
		t.Fatal("registry密码未进入尽力脱敏表")
	}
	if _, err := registrySecrets([]byte(`{"auths":{},"credsStore":"evil"}`)); err == nil {
		t.Fatal("凭据helper执行器未拒绝")
	}
}
func TestMetadataOnlyDigestNotLog(t *testing.T) {
	for _, input := range []string{`{"containerimage.digest":"sha256:` + strings.Repeat("a", 64) + `","private":"not-output"}`, "build success", `{"containerimage.digest":"latest"}`, strings.Repeat("x", (1<<20)+1)} {
		metadata, err := resultMetadata([]byte(input))
		if strings.Contains(input, strings.Repeat("a", 64)) {
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if json.Unmarshal(metadata, &value) != nil || len(value) != 1 {
				t.Fatal(string(metadata))
			}
		} else if err == nil {
			t.Fatal("无digest元数据被接受")
		}
	}
}

// 测试进程使用当前Go测试二进制，不执行主机Shell/BuildKit/任意用户脚本。
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "build" {
		runnerChild()
		return
	}
	os.Exit(m.Run())
}
func runnerChild() {
	for _, arg := range os.Args[1:] {
		if arg == "--opt=build-arg:MODE=wait" {
			signal.Ignore(syscall.SIGTERM)
			time.Sleep(30 * time.Second)
			os.Exit(5)
		}
	}
	metadata := ""
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--metadata-file=") {
			metadata = strings.TrimPrefix(arg, "--metadata-file=")
		}
	}
	if metadata == "" {
		os.Exit(4)
	}
	fmt.Fprint(os.Stdout, "REGISTRY-password BUILD-secret")
	_ = os.WriteFile(metadata, []byte(`{"containerimage.digest":"sha256:`+strings.Repeat("a", 64)+`"}`), 0600)
	os.Exit(0)
}

var _ io.Writer = (*redactingWriter)(nil)

func TestBuildFixedProcessMetadataRedactionAndCancellation(t *testing.T) {
	p := testPaths(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p.script = executable
	_ = os.WriteFile(filepath.Join(p.buildAuth, "config.json"), []byte(`{"auths":{"registry.example":{"password":"REGISTRY-password"}}}`), 0400)
	_ = os.WriteFile(filepath.Join(p.buildAuth, "secret-0"), []byte("BUILD-secret"), 0400)
	var out, errOut bytes.Buffer
	if err := runBuildKit(context.Background(), validArgs(p), false, &out, &errOut, p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "REGISTRY-password") || strings.Contains(out.String(), "BUILD-secret") {
		t.Fatal("已知秘密未脱敏")
	}
	result, err := os.ReadFile(p.result)
	if err != nil || !strings.Contains(string(result), strings.Repeat("a", 64)) {
		t.Fatal(string(result), err)
	}
	entries, err := os.ReadDir(p.runtime)
	if err != nil || len(entries) != 0 {
		t.Fatal("临时凭据未清理", entries, err)
	}
	args := validArgs(p)
	args[8] = "--opt=build-arg:MODE=wait"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = runBuildKit(ctx, args, false, &out, &errOut, p)
	if err == nil || time.Since(started) > 8*time.Second {
		t.Fatal("取消未有界收敛", err, time.Since(started))
	}
}
