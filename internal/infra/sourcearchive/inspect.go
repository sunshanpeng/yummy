package sourcearchive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Inspect 流式验证整个 gzip、tar 与实际摘要，不运行源码、不信任客户端统计。
func Inspect(ctx context.Context, reader io.Reader, options ArchiveOptions) (Result, error) {
	return inspect(ctx, reader, nil, options)
}

// Extract 仅写入现有空目录，并借助 os.Root 限制所有路径。
// 调用者必须独占该暂存目录；失败可留下不完整文件，必须整体丢弃，不得复用为 READY。
func Extract(ctx context.Context, reader io.Reader, destination string, options ArchiveOptions) (Result, error) {
	info, err := os.Lstat(destination)
	if err != nil {
		return Result{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Result{}, ErrDestination
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	file, err := root.Open(".")
	if err != nil {
		return Result{}, err
	}
	entries, err := file.Readdirnames(1)
	file.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{}, err
	}
	if len(entries) != 0 {
		return Result{}, ErrDestination
	}
	return inspect(ctx, reader, root, options)
}

func inspect(ctx context.Context, reader io.Reader, destination *os.Root, options ArchiveOptions) (result Result, err error) {
	if reader == nil {
		return result, ErrInvalidArchive
	}
	limits, err := options.Limits.normalized()
	if err != nil {
		return result, err
	}
	// 空值表示未显式指定 Dockerfile：不要求归档含 Dockerfile，由构建受理时按
	// 上下文根目录是否有 Dockerfile 自动选择 Dockerfile 或 Railpack。显式路径必须存在。
	dockerfile := options.Dockerfile
	if dockerfile != "" {
		dockerfile, err = cleanPath(dockerfile, false)
		if err != nil {
			return result, ErrDockerfile
		}
	}
	result.Dockerfile = dockerfile
	expected := strings.ToLower(options.ExpectedSHA256)
	if expected != "" {
		value, err := hex.DecodeString(expected)
		if err != nil || len(value) != sha256.Size {
			return result, ErrDigestMismatch
		}
	}
	digest := sha256.New()
	compressed := &budgetReader{r: io.TeeReader(contextReader{ctx, reader}, digest), limit: limits.MaxCompressedBytes}
	buffered := bufio.NewReader(compressed)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return result, invalidArchive(err)
	}
	defer gz.Close()
	// 禁止追加第二个 gzip member，防止校验与后续解包工具对实际内容的理解不一致。
	gz.Multistream(false)
	expanded := &budgetReader{r: contextReader{ctx, gz}, limit: limits.MaxExpandedBytes}
	tarReader := tar.NewReader(expanded)
	seen := make(map[string]bool)
	// 包含隐式父目录，确保 Inspect 与 Extract 对文件/目录冲突给出相同结论。
	nodeDirectories := make(map[string]bool)
	foundDockerfile := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, invalidArchive(err)
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		directory := header.Typeflag == tar.TypeDir
		if !directory && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return result, &PathError{header.Name, ErrUnsupported}
		}
		if header.Linkname != "" {
			return result, &PathError{header.Name, ErrUnsupported}
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return result, &PathError{header.Name, ErrUnsupported}
			}
		}
		name, err := cleanPath(header.Name, directory)
		if err != nil {
			return result, err
		}
		if seen[name] {
			return result, &PathError{name, ErrDuplicate}
		}
		if int64(len(seen)) >= limits.MaxEntries {
			return result, ErrLimit
		}
		seen[name] = true
		if previousDirectory, exists := nodeDirectories[name]; exists && (!directory || !previousDirectory) {
			return result, &PathError{name, ErrInvalidArchive}
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if parentDirectory, exists := nodeDirectories[parent]; exists && !parentDirectory {
				return result, &PathError{parent, ErrInvalidArchive}
			}
			nodeDirectories[parent] = true
		}
		nodeDirectories[name] = directory
		// 未显式写入 tar 的父目录也消耗解包节点预算，避免深路径撑爆校验器内存。
		if int64(len(nodeDirectories)) > limits.MaxEntries {
			return result, ErrLimit
		}
		if platformReason(name) != "" {
			return result, &PathError{name, ErrSensitive}
		}
		if header.Size < 0 || (directory && header.Size != 0) {
			return result, ErrInvalidArchive
		}
		if header.Size > limits.MaxExpandedBytes-result.FileBytes {
			return result, ErrLimit
		}
		item := Entry{Path: name, Size: header.Size, Directory: directory, Mode: normalizedMode(os.FileMode(header.Mode), directory)}
		var output io.Writer = io.Discard
		var file *os.File
		if destination != nil {
			if directory {
				if err := makeDirectories(destination, name); err != nil {
					return result, err
				}
			} else {
				if err := makeDirectories(destination, path.Dir(name)); err != nil {
					return result, err
				}
				file, err = destination.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, item.Mode)
				if err != nil {
					return result, err
				}
				output = file
			}
		}
		scanner := &privateScanner{}
		count, copyErr := io.Copy(io.MultiWriter(output, scanner), contextReader{ctx, tarReader})
		if file != nil {
			modeErr := file.Chmod(item.Mode)
			closeErr := file.Close()
			if copyErr == nil {
				copyErr = modeErr
			}
			if copyErr == nil {
				copyErr = closeErr
			}
		}
		if copyErr != nil {
			return result, invalidArchive(copyErr)
		}
		if count != header.Size {
			return result, ErrInvalidArchive
		}
		if scanner.found {
			return result, &PathError{name, ErrSensitive}
		}
		result.FileBytes += count
		if dockerfile != "" && name == dockerfile && !directory {
			foundDockerfile = true
		}
		result.Entries = append(result.Entries, item)
	}
	// tar EOF 后仍读至 gzip EOF，以验证 CRC/长度和剩余 padding，并执行完整解压上限。
	padding := make([]byte, 32<<10)
	for {
		n, readErr := expanded.Read(padding)
		for _, value := range padding[:n] {
			if value != 0 {
				return result, ErrInvalidArchive
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return result, invalidArchive(readErr)
		}
	}
	if _, err := buffered.Peek(1); err == nil {
		return result, ErrInvalidArchive
	} else if !errors.Is(err, io.EOF) {
		return result, invalidArchive(err)
	}
	if dockerfile != "" && !foundDockerfile {
		return result, ErrDockerfile
	}
	result.SHA256 = hex.EncodeToString(digest.Sum(nil))
	result.CompressedBytes = compressed.n
	result.ExpandedBytes = expanded.n
	if expected != "" && expected != result.SHA256 {
		return result, ErrDigestMismatch
	}
	return result, nil
}

func makeDirectories(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		component := path.Join(parts[:i+1]...)
		info, err := root.Lstat(filepath.FromSlash(component))
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(filepath.FromSlash(component), 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return &PathError{component, ErrUnsupported}
		}
	}
	return nil
}
