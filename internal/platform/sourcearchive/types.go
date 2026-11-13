// Package sourcearchive 实现不执行源码的有界 Docker 构建上下文打包、验证与安全解包。
// 上传者应写入临时文件或空暂存目录，只有成功返回后才能发布为 READY 快照。
package sourcearchive

import (
	"errors"
	"fmt"
	"io/fs"
)

var (
	ErrInvalidPath    = errors.New("源码路径不合法")
	ErrOutsideRoot    = errors.New("源码根目录不在授权范围内")
	ErrUnsupported    = errors.New("不支持的文件类型")
	ErrSensitive      = errors.New("源码包含禁止上传的敏感文件")
	ErrLimit          = errors.New("源码归档超过限制")
	ErrDuplicate      = errors.New("源码归档包含重复路径")
	ErrDockerfile     = errors.New("Dockerfile 缺失或不在构建上下文内")
	ErrDigestMismatch = errors.New("源码归档摘要不匹配")
	ErrDestination    = errors.New("解包目标必须是空目录")
	ErrInvalidArchive = errors.New("源码归档格式不合法")
)

// PathError 只携带路径与错误类别，不携带文件内容或敏感值。
type PathError struct {
	Path string
	Kind error
}

func (e *PathError) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Path) }
func (e *PathError) Unwrap() error { return e.Kind }

// Limits 的零字段使用平台默认值；负数不合法。解压上限包含 tar 头、填充和内容。
// MaxEntries 同时限制本地遍历清单、tar 显式条目和解包所需的隐式父目录节点。
type Limits struct {
	MaxCompressedBytes int64
	MaxExpandedBytes   int64
	MaxEntries         int64
}

func DefaultLimits() Limits {
	return Limits{MaxCompressedBytes: 256 << 20, MaxExpandedBytes: 1 << 30, MaxEntries: 50_000}
}

func (l Limits) normalized() (Limits, error) {
	if l.MaxCompressedBytes < 0 || l.MaxExpandedBytes < 0 || l.MaxEntries < 0 {
		return l, ErrLimit
	}
	d := DefaultLimits()
	if l.MaxCompressedBytes == 0 {
		l.MaxCompressedBytes = d.MaxCompressedBytes
	}
	if l.MaxExpandedBytes == 0 {
		l.MaxExpandedBytes = d.MaxExpandedBytes
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = d.MaxEntries
	}
	return l, nil
}

// Options 中 Context、Dockerfile 均相对 Root；空值分别为 . 和 Context/Dockerfile。
// AllowRoots 必须显式设置；先解析真实根目录，再校验授权包含关系。
type Options struct {
	Root       string
	AllowRoots []string
	Context    string
	Dockerfile string
	Limits     Limits
}

// ArchiveOptions 的 Dockerfile 相对归档根，ExpectedSHA256 是可选的 gzip 原始字节摘要。
type ArchiveOptions struct {
	Dockerfile     string
	ExpectedSHA256 string
	Limits         Limits
}

type Entry struct {
	Path      string
	Size      int64
	Mode      fs.FileMode
	Directory bool
}

type Exclusion struct {
	Path   string
	Reason string
}

// Result 中摘要来自本次实际读写的压缩字节；不复用任何先前预览的摘要。
// Preview 不产生归档，因此其摘要与压缩/解压字节为空；FileBytes 仍是预览实际读取量。
type Result struct {
	SHA256          string
	CompressedBytes int64
	ExpandedBytes   int64
	FileBytes       int64
	Dockerfile      string
	Entries         []Entry
	Excluded        []Exclusion
}
