package clientcli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

func transferURL(base, relative string) (string, error) {
	u, err := url.Parse(relative)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasPrefix(u.Path, "/transfer/") || strings.HasPrefix(relative, "//") || strings.Contains(relative, "\\") || u.RawPath != "" {
		return "", errors.New("上传地址必须是本实例的相对传输路径")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "", errors.New("上传地址路径无效")
		}
	}
	return base + u.Path, nil
}

// sourcePreview 在本机预览结果之外给出预判的构建方式。服务端受理构建时按已验证的源码快照
// 与应用构建配置重新确定并冻结，预览只用于让用户在上传前知道会发生什么。
type sourcePreview struct {
	BuildMethod string `json:"buildMethod"`
	sourcearchive.Result
}

// previewBuildMethod 与服务端规则一致：有 Dockerfile 使用它，没有则交给 Railpack。
func previewBuildMethod(result sourcearchive.Result) string {
	if result.Dockerfile != "" {
		return "DOCKERFILE"
	}
	return "RAILPACK"
}

func (a *clientApp) sourceCommand() *cobra.Command {
	root := &cobra.Command{Use: "source", Short: "本机源码预览、上传与快照查询"}
	var sourceRoot, contextPath, dockerfile string
	var allowRoots []string
	root.PersistentFlags().StringVar(&sourceRoot, "root", ".", "本机授权根目录")
	root.PersistentFlags().StringSliceVar(&allowRoots, "allow-root", nil, "明确允许读取的根目录，可重复")
	root.PersistentFlags().StringVar(&contextPath, "context", ".", "相对根目录的构建上下文")
	root.PersistentFlags().StringVar(&dockerfile, "dockerfile", "", "相对根目录的 Dockerfile 路径；留空时使用上下文根目录的 Dockerfile，没有则作为纯源码交给服务端 Railpack 构建")
	options := func() (sourcearchive.Options, error) {
		if len(allowRoots) == 0 {
			return sourcearchive.Options{}, errors.New("必须使用 --allow-root 明确授权本机目录")
		}
		return sourcearchive.Options{Root: sourceRoot, AllowRoots: allowRoots, Context: contextPath, Dockerfile: dockerfile}, nil
	}
	preview := &cobra.Command{Use: "preview", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		o, err := options()
		if err != nil {
			return configError(err)
		}
		r, err := sourcearchive.Preview(cmd.Context(), o)
		if err != nil {
			return configError(err)
		}
		return output(cmd, sourcePreview{BuildMethod: previewBuildMethod(r), Result: r})
	}}
	upload := &cobra.Command{Use: "upload", Short: "重新校验并上传快照，不触发构建", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		w, id, err := requireTarget(p)
		if err != nil {
			return err
		}
		o, err := options()
		if err != nil {
			return configError(err)
		}
		if _, err = sourcearchive.Preview(cmd.Context(), o); err != nil {
			return configError(err)
		}
		completed, err := c.uploadSource(cmd.Context(), w, id, o)
		if err != nil {
			return err
		}
		return output(cmd, completed)
	}}
	get := &cobra.Command{Use: "get SNAPSHOT_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		w, id, err := requireTarget(p)
		if err != nil {
			return err
		}
		r, err := c.source.GetSourceSnapshot(cmd.Context(), auth(c.token, &platformv1.GetSourceSnapshotRequest{WorkspaceId: w, ApplicationId: id, SnapshotId: args[0]}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	root.AddCommand(preview, upload, get)
	return root
}

func (c *apiClient) uploadSource(ctx context.Context, w, id string, o sourcearchive.Options) (*platformv1.CompleteSourceUploadResponse, error) {
	f, err := os.CreateTemp("", "yummy-source-*.tar.gz")
	if err != nil {
		return nil, fail("LOCAL_IO", 1, false, "无法创建本机临时文件")
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if err = f.Chmod(0600); err != nil {
		return nil, fail("LOCAL_IO", 1, false, "无法保护本机临时文件")
	}
	result, err := sourcearchive.Archive(ctx, f, o)
	if err != nil {
		return nil, configError(err)
	}
	if err = f.Sync(); err != nil {
		return nil, fail("LOCAL_IO", 1, false, "源码归档写入失败")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, fail("LOCAL_IO", 1, false, "源码归档读取失败")
	}
	created, err := c.source.CreateSourceUpload(ctx, auth(c.token, &platformv1.CreateSourceUploadRequest{WorkspaceId: w, ApplicationId: id, Size: result.CompressedBytes, Sha256: result.SHA256, Dockerfile: result.Dockerfile}))
	if err != nil {
		return nil, rpcError(err)
	}
	if created.Msg.Upload == nil || created.Msg.Upload.Id == "" {
		return nil, fail("INVALID_RESPONSE", 1, false, "服务端未返回上传会话")
	}
	target, err := transferURL(c.base, created.Msg.TransferPath)
	if err != nil {
		return nil, fail("INVALID_RESPONSE", 1, false, "服务端返回了不可信的传输路径")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f)
	if err != nil {
		return nil, fail("REQUEST_FAILED", 1, false, "无法创建上传请求")
	}
	req.ContentLength = result.CompressedBytes
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+c.token)
	client := newHTTPClient()
	client.Timeout = 10 * time.Minute
	res, err := client.Do(req)
	if err != nil {
		return nil, fail("UNAVAILABLE", 7, true, "上传中断；请检查上传状态后重试")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return nil, fail("UPLOAD_FAILED", 1, false, fmt.Sprintf("源码上传失败，HTTP %d", res.StatusCode))
	}
	completed, err := c.source.CompleteSourceUpload(ctx, auth(c.token, &platformv1.CompleteSourceUploadRequest{WorkspaceId: w, ApplicationId: id, UploadId: created.Msg.Upload.Id}))
	if err != nil {
		return nil, rpcError(err)
	}
	if completed.Msg.Snapshot == nil || completed.Msg.Snapshot.Id == "" {
		return nil, fail("INVALID_RESPONSE", 1, false, "服务端未返回源码快照")
	}
	return completed.Msg, nil
}
