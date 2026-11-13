package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/platform/sourcearchive"
)

// LocalConfig 由 CLI 组装。Roots 必须从用户显式授权的 --allow-root 获取，
// 未提供或不是绝对路径时拒绝启动；不会读取个人凭据或回连 MCP HTTP 服务。
type LocalConfig struct{ AllowRoots []string }

type localPreviewInput struct {
	Root       string `json:"root" jsonschema:"用户显式授权目录中的本地源码路径"`
	Context    string `json:"context,omitempty" jsonschema:"相对源码根的构建上下文"`
	Dockerfile string `json:"dockerfile,omitempty" jsonschema:"相对源码根的 Dockerfile 路径"`
}

// validateAllowRoots 校验用户显式授权的源码根目录：必须非空且全部为绝对路径。
func validateAllowRoots(allowRoots []string) ([]string, error) {
	if len(allowRoots) == 0 {
		return nil, errors.New("必须显式指定授权源码根目录")
	}
	for _, root := range allowRoots {
		if root == "" || !filepath.IsAbs(root) {
			return nil, errors.New("授权源码根目录必须为绝对路径")
		}
	}
	return slices.Clone(allowRoots), nil
}

// registerSourcePreview 在服务上注册只读本地预览工具；语义固定：不上传、
// 不返回源码内容或本地绝对路径。
func registerSourcePreview(server *sdk.Server, roots []string) {
	typedTool(server, "source_preview", "只预览显式授权根目录内源码；不上传、不返回源码内容或本地绝对路径。", annotations(true, false, true, false), func(ctx context.Context, in localPreviewInput) (any, *sdk.CallToolResult) {
		if !filepath.IsAbs(in.Root) {
			return nil, toolError("INVALID_ARGUMENT", "源码根目录必须为绝对路径")
		}
		result, err := sourcearchive.Preview(ctx, sourcearchive.Options{Root: in.Root, AllowRoots: roots, Context: in.Context, Dockerfile: in.Dockerfile})
		if err != nil {
			return nil, toolError("SOURCE_PREVIEW_FAILED", "源码不在授权目录中或未通过安全校验")
		}
		return struct {
			FileCount     int    `json:"fileCount"`
			ExcludedCount int    `json:"excludedCount"`
			FileBytes     int64  `json:"fileBytes"`
			Dockerfile    string `json:"dockerfile"`
		}{len(result.Entries), len(result.Excluded), result.FileBytes, result.Dockerfile}, nil
	})
}

// NewLocalServer 只提供本地源码预览；业务工具应由 CLI 的 API 客户端桥接，
// 不接受调用方伪造服务端 Principal，也不在远程 MCP 开放本地目录读取。
func NewLocalServer(config LocalConfig) (*sdk.Server, error) {
	roots, err := validateAllowRoots(config.AllowRoots)
	if err != nil {
		return nil, err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "yummy-local", Version: "1.0.0"}, nil)
	registerSourcePreview(server, roots)
	return server, nil
}
