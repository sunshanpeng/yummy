package sourcearchive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

const maxIgnoreBytes = 1 << 20

// Preview 实际读取将上传的普通文件并检查私钥内容，但不生成归档。
// Archive 会重新遍历和校验；调用者不能用本结果的文件统计代替最终归档事实。
func Preview(ctx context.Context, options Options) (Result, error) {
	return pack(ctx, nil, options)
}

// Archive 把选定上下文写为规范化 tar.gz。若返回错误，writer 中可能已有部分内容，
// 必须丢弃；不能作为 READY 上传或输出给不可信接收方。调用者应使用临时文件。
func Archive(ctx context.Context, writer io.Writer, options Options) (Result, error) {
	if writer == nil {
		return Result{}, errors.New("归档输出不能为空")
	}
	return pack(ctx, writer, options)
}

func pack(ctx context.Context, writer io.Writer, options Options) (result Result, err error) {
	limits, err := options.Limits.normalized()
	if err != nil {
		return result, err
	}
	root, real, err := openAuthorizedRoot(options.Root, options.AllowRoots)
	if err != nil {
		return result, err
	}
	defer root.Close()
	contextPath := options.Context
	if contextPath == "" {
		contextPath = "."
	}
	contextPath, err = cleanPath(contextPath, true)
	if err != nil {
		return result, err
	}
	dockerfile := options.Dockerfile
	if dockerfile == "" {
		dockerfile = path.Join(contextPath, "Dockerfile")
	}
	dockerfile, err = cleanPath(dockerfile, false)
	if err != nil {
		return result, err
	}
	relative, err := filepath.Rel(filepath.FromSlash(contextPath), filepath.FromSlash(dockerfile))
	if err != nil || !within(filepath.Join(real, filepath.FromSlash(contextPath)), filepath.Join(real, filepath.FromSlash(dockerfile))) {
		return result, ErrDockerfile
	}
	dockerfile, err = cleanPath(filepath.ToSlash(relative), false)
	if err != nil {
		return result, ErrDockerfile
	}
	result.Dockerfile = dockerfile
	if err = checkComponents(root, contextPath); err != nil {
		return result, err
	}
	contextRoot, err := root.OpenRoot(filepath.FromSlash(contextPath))
	if err != nil {
		return result, err
	}
	defer contextRoot.Close()
	if err = checkComponents(contextRoot, dockerfile); err != nil {
		return result, fmt.Errorf("%w: %w", ErrDockerfile, err)
	}
	info, err := contextRoot.Lstat(filepath.FromSlash(dockerfile))
	if err != nil || !info.Mode().IsRegular() {
		return result, ErrDockerfile
	}
	if platformReason(dockerfile) != "" {
		return result, &PathError{dockerfile, ErrSensitive}
	}
	matcher, ignorePath, err := loadIgnore(contextRoot, dockerfile)
	if err != nil {
		return result, err
	}

	var tarWriter *tar.Writer
	var gzipWriter *gzip.Writer
	var compressed, expanded *budgetWriter
	digest := sha256.New()
	if writer != nil {
		compressed = &budgetWriter{w: io.MultiWriter(writer, digest), limit: limits.MaxCompressedBytes}
		gzipWriter = gzip.NewWriter(compressed)
		expanded = &budgetWriter{w: gzipWriter, limit: limits.MaxExpandedBytes}
		tarWriter = tar.NewWriter(expanded)
	}
	foundDockerfile := false
	var visited int64
	err = fs.WalkDir(contextRoot.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		visited++
		// 包括被 ignore 的遍历结果也有上限，避免预览排除清单无界增长。
		if visited > limits.MaxEntries {
			return ErrLimit
		}
		clean, err := cleanPath(name, entry.IsDir())
		if err != nil {
			return err
		}
		if reason := platformReason(clean); reason != "" {
			result.Excluded = append(result.Excluded, Exclusion{clean, reason})
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := contextRoot.Lstat(filepath.FromSlash(clean))
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return &PathError{clean, ErrUnsupported}
		}
		if info.Mode().IsRegular() && hasMultipleLinks(info) {
			return &PathError{clean, ErrUnsupported}
		}
		ignored, err := matcher.MatchesOrParentMatches(clean)
		if err != nil {
			return err
		}
		// Docker 构建必须获得所选 Dockerfile 与实际使用的 ignore 文件，即使规则排除了它们。
		if ignored && clean != dockerfile && clean != ignorePath {
			result.Excluded = append(result.Excluded, Exclusion{clean, "Docker ignore 规则排除"})
			// 不能跳过目录：后续 ! 规则可能重新包含其中的文件。
			return nil
		}
		if int64(len(result.Entries)) >= limits.MaxEntries {
			return ErrLimit
		}
		mode := normalizedMode(info.Mode(), info.IsDir())
		item := Entry{Path: clean, Mode: mode, Directory: info.IsDir()}
		if info.Mode().IsRegular() {
			if info.Size() > limits.MaxExpandedBytes-result.FileBytes {
				return ErrLimit
			}
			item.Size = info.Size()
		}
		if tarWriter != nil {
			header := &tar.Header{Name: clean, Mode: int64(mode), Size: item.Size, ModTime: time.Unix(0, 0), Format: tar.FormatPAX, Typeflag: tar.TypeReg}
			if item.Directory {
				header.Typeflag = tar.TypeDir
			}
			if err := tarWriter.WriteHeader(header); err != nil {
				return err
			}
		}
		if !item.Directory {
			file, err := openLocalFile(contextRoot, filepath.FromSlash(clean))
			if err != nil {
				return err
			}
			defer file.Close()
			actual, err := file.Stat()
			if err != nil {
				return err
			}
			if !actual.Mode().IsRegular() || hasMultipleLinks(actual) || !os.SameFile(info, actual) || actual.Size() != item.Size {
				return &PathError{clean, ErrUnsupported}
			}
			if err := checkComponents(contextRoot, clean); err != nil {
				return err
			}
			scanner := &privateScanner{}
			var output io.Writer = scanner
			if tarWriter != nil {
				output = io.MultiWriter(scanner, tarWriter)
			}
			count, err := io.Copy(output, io.LimitReader(contextReader{ctx, file}, item.Size+1))
			if err != nil {
				return err
			}
			if count != item.Size {
				return &PathError{clean, ErrInvalidArchive}
			}
			if scanner.found {
				return &PathError{clean, ErrSensitive}
			}
			result.FileBytes += count
			if clean == dockerfile {
				foundDockerfile = true
			}
		}
		result.Entries = append(result.Entries, item)
		return nil
	})
	if err != nil {
		return result, err
	}
	if !foundDockerfile {
		return result, ErrDockerfile
	}
	if tarWriter != nil {
		if err := tarWriter.Close(); err != nil {
			return result, err
		}
		if err := gzipWriter.Close(); err != nil {
			return result, err
		}
		result.CompressedBytes = compressed.n
		result.ExpandedBytes = expanded.n
		result.SHA256 = hex.EncodeToString(digest.Sum(nil))
	}
	return result, nil
}

func loadIgnore(root *os.Root, dockerfile string) (*patternmatcher.PatternMatcher, string, error) {
	ignorePath := dockerfile + ".dockerignore"
	info, err := root.Lstat(filepath.FromSlash(ignorePath))
	if errors.Is(err, os.ErrNotExist) {
		ignorePath = ".dockerignore"
		info, err = root.Lstat(ignorePath)
	}
	if errors.Is(err, os.ErrNotExist) {
		matcher, err := patternmatcher.New(nil)
		return matcher, "", err
	}
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || hasMultipleLinks(info) {
		return nil, "", &PathError{ignorePath, ErrUnsupported}
	}
	if info.Size() > maxIgnoreBytes {
		return nil, "", &PathError{ignorePath, ErrLimit}
	}
	if err := checkComponents(root, ignorePath); err != nil {
		return nil, "", err
	}
	file, err := openLocalFile(root, filepath.FromSlash(ignorePath))
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !actual.Mode().IsRegular() || hasMultipleLinks(actual) || !os.SameFile(info, actual) {
		return nil, "", &PathError{ignorePath, ErrUnsupported}
	}
	bounded := &budgetReader{r: file, limit: maxIgnoreBytes}
	patterns, err := ignorefile.ReadAll(bounded)
	if err != nil {
		return nil, "", err
	}
	matcher, err := patternmatcher.New(patterns)
	return matcher, ignorePath, err
}
