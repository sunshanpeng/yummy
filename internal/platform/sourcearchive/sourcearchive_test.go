package sourcearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (string, Options) {
	t.Helper()
	root := t.TempDir()
	put(t, root, "Dockerfile", "FROM scratch\nCOPY . /app\n", 0644)
	return root, Options{Root: root, AllowRoots: []string{root}}
}

func put(t *testing.T, root, name, content string, mode fs.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func paths(result Result) []string {
	var names []string
	for _, item := range result.Entries {
		names = append(names, item.Path)
	}
	return names
}

func contains(result Result, name string) bool {
	for _, entry := range result.Entries {
		if entry.Path == name {
			return true
		}
	}
	return false
}

func archive(t *testing.T, options Options) ([]byte, Result) {
	t.Helper()
	var buffer bytes.Buffer
	result, err := Archive(context.Background(), &buffer, options)
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), result
}

func TestRoundTripNormalizedAndDeterministic(t *testing.T) {
	root, options := fixture(t)
	put(t, root, "bin/run", "#!/bin/sh\necho hello\n", 0711)
	put(t, root, "data.txt", "hello", 0600)
	first, created := archive(t, options)
	if err := os.Chtimes(filepath.Join(root, "data.txt"), time.Now(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	second, _ := archive(t, options)
	if !bytes.Equal(first, second) {
		t.Fatal("修改时间不应改变规范归档")
	}
	digest := sha256.Sum256(first)
	if created.SHA256 != hex.EncodeToString(digest[:]) || created.CompressedBytes != int64(len(first)) {
		t.Fatalf("实际摘要或字节统计错误: %+v", created)
	}
	inspected, err := Inspect(context.Background(), bytes.NewReader(first), ArchiveOptions{ExpectedSHA256: created.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created, inspected) {
		t.Fatalf("打包与校验事实不同: %+v / %+v", created, inspected)
	}
	destination := t.TempDir()
	if _, err := Extract(context.Background(), bytes.NewReader(first), destination, ArchiveOptions{ExpectedSHA256: created.SHA256}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "data.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("解包内容错误: %q %v", content, err)
	}
	for name, expectedMode := range map[string]fs.FileMode{"data.txt": 0644, "bin/run": 0755} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || info.Mode().Perm() != expectedMode {
			t.Fatalf("权限未归一: %s %v", name, err)
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || !header.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatalf("所有者与时间未归一: %+v", header)
		}
	}
}

func TestDockerIgnoreCompatibility(t *testing.T) {
	t.Run("根规则、双星与反向包含", func(t *testing.T) {
		root, options := fixture(t)
		put(t, root, ".dockerignore", "\ufeff# 注释\n.\n**/*.log\nignored\n!ignored/keep.txt\nDockerfile\n.dockerignore\n", 0644)
		put(t, root, "nested/debug.log", "ignored", 0644)
		put(t, root, "ignored/drop.txt", "ignored", 0644)
		put(t, root, "ignored/keep.txt", "keep", 0644)
		preview, err := Preview(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(preview, "Dockerfile") || !contains(preview, ".dockerignore") || !contains(preview, "ignored/keep.txt") {
			t.Fatalf("必要文件或否定规则未生效: %v", paths(preview))
		}
		if contains(preview, "nested/debug.log") || contains(preview, "ignored/drop.txt") {
			t.Fatalf("ignore未生效: %v", paths(preview))
		}
		if preview.SHA256 != "" || preview.CompressedBytes != 0 {
			t.Fatal("预览不应伪造最终摘要")
		}
		packed, result := archive(t, options)
		if !reflect.DeepEqual(paths(preview), paths(result)) {
			t.Fatal("预览与实际文件清单不一致")
		}
		if _, err := Inspect(context.Background(), bytes.NewReader(packed), ArchiveOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("Dockerfile专用文件替代根规则", func(t *testing.T) {
		root, options := fixture(t)
		put(t, root, ".dockerignore", "root.txt\n", 0644)
		put(t, root, "Dockerfile.dockerignore", "special.txt\n", 0644)
		put(t, root, "root.txt", "included", 0644)
		put(t, root, "special.txt", "ignored", 0644)
		result, err := Preview(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(result, "root.txt") || contains(result, "special.txt") {
			t.Fatalf("专用规则未覆盖根规则: %v", paths(result))
		}
	})
}

func TestPlatformExclusionsCannotBeNegated(t *testing.T) {
	root, options := fixture(t)
	put(t, root, ".dockerignore", "!.env\n!.git/config\n!id_rsa\n", 0644)
	for _, name := range []string{".env", ".env.production", ".git/config", ".ssh/config", "id_rsa", "private.pem", "nested/server.key"} {
		put(t, root, name, "secret", 0644)
	}
	put(t, root, ".env.example", "EXAMPLE=value\n", 0644)
	preview, err := Preview(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(preview, ".env.example") {
		t.Fatal("缺少示例环境文件例外")
	}
	for _, excluded := range preview.Excluded {
		if excluded.Reason == "" {
			t.Fatal("排除必须附原因")
		}
		if contains(preview, excluded.Path) {
			t.Fatalf("平台排除被绕过: %s", excluded.Path)
		}
	}
	if len(preview.Excluded) < 7 {
		t.Fatalf("缺少平台排除: %+v", preview.Excluded)
	}
}

func TestContentPrivateKeyDetection(t *testing.T) {
	for _, name := range []string{".env.example", "unknown.txt", "public.pem"} {
		t.Run(name, func(t *testing.T) {
			root, options := fixture(t)
			// 让标记横跨常用复制块的边界，验证不能只检查每块内部。
			put(t, root, name, strings.Repeat("x", 32*1024-8)+"-----BEGIN OPENSSH PRIVATE KEY-----\nsecret", 0644)
			if _, err := Preview(context.Background(), options); !errors.Is(err, ErrSensitive) {
				t.Fatalf("预览未拒绝私钥: %v", err)
			}
			var buffer bytes.Buffer
			if _, err := Archive(context.Background(), &buffer, options); !errors.Is(err, ErrSensitive) {
				t.Fatalf("打包未拒绝私钥: %v", err)
			}
		})
	}
}

func TestRootContextAndDockerfileBoundaries(t *testing.T) {
	root, options := fixture(t)
	put(t, root, "sub/Custom.Dockerfile", "FROM scratch", 0644)
	put(t, root, "sub/app.txt", "app", 0644)
	options.Context, options.Dockerfile = "sub", "sub/Custom.Dockerfile"
	packed, result := archive(t, options)
	if result.Dockerfile != "Custom.Dockerfile" || contains(result, "Dockerfile") {
		t.Fatalf("上下文未相对化: %+v", result)
	}
	if _, err := Inspect(context.Background(), bytes.NewReader(packed), ArchiveOptions{Dockerfile: result.Dockerfile}); err != nil {
		t.Fatal(err)
	}
	options.Dockerfile = "Dockerfile"
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrDockerfile) {
		t.Fatalf("允许context外Dockerfile: %v", err)
	}
	options.Context = "../escape"
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("允许上级context: %v", err)
	}
	options = Options{Root: root}
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("默认授权目录: %v", err)
	}
	options.AllowRoots = []string{t.TempDir()}
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("允许未授权根: %v", err)
	}
	options.AllowRoots = []string{filepath.Dir(root)}
	options.Context = "/absolute"
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("允许绝对context: %v", err)
	}
}

func TestPreviewIsNotFinalDigest(t *testing.T) {
	root, options := fixture(t)
	put(t, root, "file", "old", 0644)
	if _, err := Preview(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	first, _ := archive(t, options)
	put(t, root, "file", "new actual content", 0644)
	second, result := archive(t, options)
	if bytes.Equal(first, second) {
		t.Fatal("复用了旧预览内容")
	}
	digest := sha256.Sum256(second)
	if result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("摘要不是实际归档字节")
	}
}

type archiveItem struct {
	name, body string
	kind       byte
	link       string
}

func maliciousArchive(t *testing.T, items []archiveItem) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, item := range items {
		kind := item.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: item.name, Typeflag: kind, Mode: 07777, Linkname: item.link}
		if kind == tar.TypeReg || kind == tar.TypeRegA {
			header.Size = int64(len(item.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := io.WriteString(tw, item.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRejectArchivePathsTypesAndDuplicates(t *testing.T) {
	for _, test := range []struct {
		name string
		item archiveItem
		kind error
	}{
		{"上级", archiveItem{name: "../escape", body: "bad"}, ErrInvalidPath},
		{"绝对", archiveItem{name: "/escape", body: "bad"}, ErrInvalidPath},
		{"盘符", archiveItem{name: "C:/escape", body: "bad"}, ErrInvalidPath},
		{"反斜线", archiveItem{name: "a\\..\\escape", body: "bad"}, ErrInvalidPath},
		{"中间上级", archiveItem{name: "a/../escape", body: "bad"}, ErrInvalidPath},
		{"空路径段", archiveItem{name: "a//escape", body: "bad"}, ErrInvalidPath},
		{"链接", archiveItem{name: "link", kind: tar.TypeSymlink, link: "/tmp"}, ErrUnsupported},
		{"硬链接", archiveItem{name: "link", kind: tar.TypeLink, link: "Dockerfile"}, ErrUnsupported},
		{"设备", archiveItem{name: "device", kind: tar.TypeChar}, ErrUnsupported},
		{"FIFO", archiveItem{name: "fifo", kind: tar.TypeFifo}, ErrUnsupported},
		{"秘密文件", archiveItem{name: ".env", body: "secret"}, ErrSensitive},
		{"秘密内容", archiveItem{name: "unknown", body: "-----BEGIN RSA PRIVATE KEY-----"}, ErrSensitive},
		{"重复Dockerfile", archiveItem{name: "./Dockerfile", body: "FROM scratch"}, ErrDuplicate},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}, test.item})
			if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{}); !errors.Is(err, test.kind) {
				t.Fatalf("错误类型 %v, 期望 %v", err, test.kind)
			}
			if _, err := Extract(context.Background(), bytes.NewReader(data), t.TempDir(), ArchiveOptions{}); !errors.Is(err, test.kind) {
				t.Fatalf("解包错误类型 %v, 期望 %v", err, test.kind)
			}
		})
	}
}

func TestLimitsExactBoundaryAndDigest(t *testing.T) {
	_, options := fixture(t)
	data, generated := archive(t, options)
	limits := Limits{MaxCompressedBytes: generated.CompressedBytes, MaxExpandedBytes: generated.ExpandedBytes, MaxEntries: int64(len(generated.Entries))}
	if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{Limits: limits}); err != nil {
		t.Fatalf("恰好边界被拒绝: %v", err)
	}
	for _, test := range []struct {
		name   string
		limits Limits
	}{
		{"压缩", Limits{MaxCompressedBytes: limits.MaxCompressedBytes - 1}},
		{"解压", Limits{MaxExpandedBytes: limits.MaxExpandedBytes - 1}},
		{"负数", Limits{MaxEntries: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{Limits: test.limits}); !errors.Is(err, ErrLimit) {
				t.Fatalf("未触发上限: %v", err)
			}
		})
	}
	for _, expected := range []string{strings.Repeat("0", 64), "bad"} {
		if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{ExpectedSHA256: expected}); !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("未拒绝错误摘要: %v", err)
		}
	}
	options.Limits = limits
	if _, err := Archive(context.Background(), io.Discard, options); err != nil {
		t.Fatalf("打包恰好边界失败: %v", err)
	}
	options.Limits.MaxCompressedBytes--
	if _, err := Archive(context.Background(), io.Discard, options); !errors.Is(err, ErrLimit) {
		t.Fatalf("打包压缩限制未生效: %v", err)
	}
}

func TestExpandedBombAndEntryLimit(t *testing.T) {
	data := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}, {name: "large", body: strings.Repeat("0", 1<<20)}})
	if len(data) > 5000 {
		t.Fatal("样本没有形成高压缩比")
	}
	if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{Limits: Limits{MaxExpandedBytes: 4096}}); !errors.Is(err, ErrLimit) {
		t.Fatalf("未拒绝压缩炸弹: %v", err)
	}
	if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{Limits: Limits{MaxEntries: 1}}); !errors.Is(err, ErrLimit) {
		t.Fatalf("条目数未受限: %v", err)
	}
	root, options := fixture(t)
	put(t, root, "large", strings.Repeat("0", 4096), 0644)
	options.Limits.MaxExpandedBytes = 2048
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrLimit) {
		t.Fatalf("预览文件体积未受限: %v", err)
	}
	options.Limits = Limits{MaxEntries: 1}
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrLimit) {
		t.Fatalf("预览条目未受限: %v", err)
	}
}

func TestArchiveIntegrityAndNoConcatenatedMembers(t *testing.T) {
	data := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}})
	corrupt := bytes.Clone(data)
	corrupt[len(corrupt)-8] ^= 1
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"空", nil}, {"非gzip", []byte("hello")}, {"截断", data[:len(data)-3]}, {"CRC", corrupt},
		{"尾部字节", append(bytes.Clone(data), 1)}, {"多gzip成员", append(bytes.Clone(data), data...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Inspect(context.Background(), bytes.NewReader(test.data), ArchiveOptions{}); err == nil {
				t.Fatal("接受损坏或附加归档")
			}
		})
	}
	missing := maliciousArchive(t, []archiveItem{{name: "hello", body: "no dockerfile"}})
	if _, err := Inspect(context.Background(), bytes.NewReader(missing), ArchiveOptions{}); !errors.Is(err, ErrDockerfile) {
		t.Fatalf("允许缺少Dockerfile: %v", err)
	}
}

func TestExtractionEmptyDestinationAndNoOverwrite(t *testing.T) {
	_, options := fixture(t)
	data, _ := archive(t, options)
	destination := t.TempDir()
	put(t, destination, "keep", "original", 0644)
	if _, err := Extract(context.Background(), bytes.NewReader(data), destination, ArchiveOptions{}); !errors.Is(err, ErrDestination) {
		t.Fatalf("允许写非空目标: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "keep"))
	if err != nil || string(content) != "original" {
		t.Fatal("破坏了既有文件")
	}
	conflict := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}, {name: "a", body: "file"}, {name: "a/b", body: "child"}})
	if _, err := Extract(context.Background(), bytes.NewReader(conflict), t.TempDir(), ArchiveOptions{}); err == nil {
		t.Fatal("允许把普通文件当目录")
	}
	if _, err := Inspect(context.Background(), bytes.NewReader(conflict), ArchiveOptions{}); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("校验器接受文件目录冲突: %v", err)
	}
	reverse := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}, {name: "a/b", body: "child"}, {name: "a", body: "file"}})
	if _, err := Inspect(context.Background(), bytes.NewReader(reverse), ArchiveOptions{}); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("校验器接受隐式父目录覆盖: %v", err)
	}
}

func TestImplicitDirectoriesConsumeEntryBudget(t *testing.T) {
	data := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}, {name: "a/b/c/d/file", body: "nested"}})
	if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{Limits: Limits{MaxEntries: 3}}); !errors.Is(err, ErrLimit) {
		t.Fatalf("隐式父目录未受限: %v", err)
	}
}

func TestPrivateMarkerAcrossWrites(t *testing.T) {
	scanner := &privateScanner{}
	for _, part := range []string{"prefix-----BEGIN OPEN", "SSH PRIVATE KEY-----suffix"} {
		if _, err := io.WriteString(scanner, part); err != nil {
			t.Fatal(err)
		}
	}
	if !scanner.found {
		t.Fatal("跨块私钥标记未识别")
	}
}

func TestDirectoryOrderAndLongPAXNames(t *testing.T) {
	longName := strings.Repeat("a", 180) + "/file.txt"
	data := maliciousArchive(t, []archiveItem{{name: "./", kind: tar.TypeDir}, {name: "Dockerfile", body: "FROM scratch"}, {name: longName, body: "long"}, {name: strings.Repeat("a", 180) + "/", kind: tar.TypeDir}})
	if _, err := Inspect(context.Background(), bytes.NewReader(data), ArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if _, err := Extract(context.Background(), bytes.NewReader(data), destination, ArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, longName))
	if err != nil || string(content) != "long" {
		t.Fatalf("PAX长路径丢失: %v", err)
	}
}

func TestExcludedPreviewHasBoundedEntries(t *testing.T) {
	root, options := fixture(t)
	put(t, root, ".dockerignore", "*.txt\n", 0644)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		put(t, root, name, "ignored", 0644)
	}
	options.Limits.MaxEntries = 3
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrLimit) {
		t.Fatalf("排除预览无限增长: %v", err)
	}
}

func FuzzInspectDoesNotPanic(f *testing.F) {
	f.Add([]byte("not gzip"))
	f.Add([]byte{0x1f, 0x8b, 0x08, 0})
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = Inspect(context.Background(), bytes.NewReader(input), ArchiveOptions{Limits: Limits{MaxCompressedBytes: 64 << 10, MaxExpandedBytes: 128 << 10, MaxEntries: 128}})
	})
}

func TestCancellationAndInvalidIgnore(t *testing.T) {
	root, options := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Archive(ctx, io.Discard, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("打包忽略取消: %v", err)
	}
	data := maliciousArchive(t, []archiveItem{{name: "Dockerfile", body: "FROM scratch"}})
	if _, err := Inspect(ctx, bytes.NewReader(data), ArchiveOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("检查忽略取消: %v", err)
	}
	put(t, root, ".dockerignore", "[\n", 0644)
	if _, err := Preview(context.Background(), options); err == nil {
		t.Fatal("忽略非法Docker模式")
	}
	put(t, root, ".dockerignore", strings.Repeat("x", maxIgnoreBytes+1), 0644)
	if _, err := Preview(context.Background(), options); !errors.Is(err, ErrLimit) {
		t.Fatalf("ignore文件未受限: %v", err)
	}
}
