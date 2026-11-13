package sourcearchive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// cleanPath 接受常见 tar 的 ./ 前缀，但拒绝任何上级、绝对、跨平台盘符和模糊路径。
func cleanPath(name string, directory bool) (string, error) {
	original := name
	if len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return "", &PathError{original, ErrInvalidPath}
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || name == "." {
		if directory {
			return ".", nil
		}
		return "", &PathError{original, ErrInvalidPath}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", &PathError{original, ErrInvalidPath}
		}
	}
	if !fs.ValidPath(name) {
		return "", &PathError{original, ErrInvalidPath}
	}
	return name, nil
}

func platformReason(name string) string {
	for _, part := range strings.Split(strings.ToLower(name), "/") {
		if part == ".git" || part == ".ssh" {
			return "平台排除版本库或 SSH 目录"
		}
		if part == ".env" || (strings.HasPrefix(part, ".env.") && part != ".env.example") {
			return "平台排除环境秘密文件"
		}
		switch part {
		case "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "identity", "private.pem", "key.pem", "private_key":
			return "平台排除常见私钥文件"
		}
		if strings.HasSuffix(part, ".key") || strings.HasSuffix(part, ".p12") || strings.HasSuffix(part, ".pfx") ||
			(strings.HasSuffix(part, ".pem") && strings.Contains(part, "private")) {
			return "平台排除常见私钥文件"
		}
	}
	return ""
}

var privateMarkers = [][]byte{
	[]byte("-----BEGIN PRIVATE KEY-----"), []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----"),
	[]byte("-----BEGIN RSA PRIVATE KEY-----"), []byte("-----BEGIN DSA PRIVATE KEY-----"),
	[]byte("-----BEGIN EC PRIVATE KEY-----"), []byte("-----BEGIN OPENSSH PRIVATE KEY-----"),
	[]byte("-----BEGIN PGP PRIVATE KEY BLOCK-----"),
}

// privateScanner 在块边界保留足够后缀；不缓存整个源码文件。
type privateScanner struct {
	tail  []byte
	found bool
}

func (s *privateScanner) Write(p []byte) (int, error) {
	n := len(p)
	b := append(s.tail, p...)
	for _, marker := range privateMarkers {
		if bytes.Contains(b, marker) {
			s.found = true
		}
	}
	if len(b) > 64 {
		b = b[len(b)-64:]
	}
	s.tail = append(s.tail[:0], b...)
	return n, nil
}

func normalizedMode(mode fs.FileMode, directory bool) fs.FileMode {
	if directory || mode.Perm()&0111 != 0 {
		return 0755
	}
	return 0644
}

func within(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func openAuthorizedRoot(root string, allowed []string) (*os.Root, string, error) {
	if root == "" || len(allowed) == 0 {
		return nil, "", ErrOutsideRoot
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, "", err
	}
	for _, candidate := range allowed {
		if candidate == "" {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		authorized, err := filepath.EvalSymlinks(abs)
		if err != nil || !within(authorized, real) {
			continue
		}
		// 从可信授权根的句柄打开子目录，避免先校验字符串再打开源码路径的逃逸竞态。
		base, err := os.OpenRoot(authorized)
		if err != nil {
			return nil, "", err
		}
		relative, err := filepath.Rel(authorized, real)
		if err != nil {
			base.Close()
			return nil, "", err
		}
		opened, err := base.OpenRoot(relative)
		base.Close()
		if err != nil {
			return nil, "", err
		}
		return opened, real, nil
	}
	return nil, "", ErrOutsideRoot
}

// checkComponents 不允许用户配置通过目录链接改变 context/Dockerfile 的身份。
func checkComponents(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		p := path.Join(parts[:i+1]...)
		info, err := root.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return &PathError{p, ErrUnsupported}
		}
	}
	return nil
}

// contextReader 只在读取之间检查取消；不会另开 goroutine 或接管调用者的 reader 生命周期。
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// budgetReader 多读取至多一个字节来区分“恰好达到限制”和“超过限制”。
type budgetReader struct {
	r        io.Reader
	limit, n int64
}

func (r *budgetReader) Read(p []byte) (int, error) {
	if int64(len(p)) > r.limit-r.n+1 {
		p = p[:max(0, r.limit-r.n+1)]
	}
	if len(p) == 0 {
		return 0, ErrLimit
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.limit {
		return n, ErrLimit
	}
	return n, err
}

type budgetWriter struct {
	w        io.Writer
	limit, n int64
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.n {
		return 0, ErrLimit
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func invalidArchive(err error) error {
	return fmt.Errorf("%w: %w", ErrInvalidArchive, err)
}
