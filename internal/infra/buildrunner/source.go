package buildrunner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

// PrepareOptions的路径必须与固定Pod spec相同；URI只承载位置，授权不放query/用户密码中。
type PrepareOptions struct {
	URI, TokenFile, ArchiveSHA256, Destination, Dockerfile string
	MaxBytes                                               int64
}

func sourceClient(roots *x509.CertPool) *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func PrepareSource(ctx context.Context, options PrepareOptions) error {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("读取系统CA失败")
	}
	client := sourceClient(roots)
	defer client.CloseIdleConnections()
	return prepareSource(ctx, options, fixedPaths(), client)
}
func prepareSource(ctx context.Context, o PrepareOptions, p paths, client *http.Client) error {
	endpoint, err := url.Parse(o.URI)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || !digestPattern.MatchString(o.ArchiveSHA256) || o.MaxBytes < 1 || o.MaxBytes > 2<<30 || !safeRelative(o.Dockerfile) {
		return ErrInvalid
	}
	if o.TokenFile != p.sourceToken || o.Destination != filepath.Join(p.workspace, "source") {
		return fmt.Errorf("%w: prepare路径固定", ErrInvalid)
	}
	token, err := readBoundedFile(o.TokenFile, 65536)
	if err != nil {
		return fmt.Errorf("读取源码授权文件失败")
	}
	if len(token) == 0 || strings.ContainsAny(string(token), "\r\n") {
		return ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URI, nil)
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/gzip")
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载源码失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载源码HTTP%d", response.StatusCode)
	}
	if response.ContentLength > o.MaxBytes {
		return sourcearchive.ErrLimit
	}
	// 暂存文件限制在任务emptyDir，先完整Inspect再安全Extract。失败不发布source目录。
	archive, err := os.CreateTemp(p.workspace, ".source-archive-")
	if err != nil {
		return err
	}
	archiveName := archive.Name()
	defer func() { _ = archive.Close(); _ = os.Remove(archiveName) }()
	n, err := io.Copy(archive, io.LimitReader(response.Body, o.MaxBytes+1))
	if err != nil {
		return fmt.Errorf("下载源码失败")
	}
	if n > o.MaxBytes {
		return sourcearchive.ErrLimit
	}
	options := sourcearchive.ArchiveOptions{Dockerfile: o.Dockerfile, ExpectedSHA256: strings.TrimPrefix(o.ArchiveSHA256, "sha256:"), Limits: sourcearchive.Limits{MaxCompressedBytes: o.MaxBytes, MaxExpandedBytes: 1 << 30, MaxEntries: 50000}}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = sourcearchive.Inspect(ctx, archive, options); err != nil {
		return err
	}
	if _, err = os.Lstat(o.Destination); err == nil {
		return sourcearchive.ErrDestination
	} else if !os.IsNotExist(err) {
		return err
	}
	staging, err := os.MkdirTemp(p.workspace, ".source-prepared-")
	if err != nil {
		return err
	}
	// staging由固定emptyDir目录内MkdirTemp创建，清理不使用用户提供或未核对的路径。
	defer func() { _ = os.RemoveAll(staging) }()
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = sourcearchive.Extract(ctx, archive, staging, options); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(staging, o.Destination)
}
func readBoundedFile(name string, max int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrInvalid
	}
	return data, nil
}
