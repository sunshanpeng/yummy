package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/runtimeconfig"
)

type configInput struct {
	Config runtimeconfig.Input `json:"config" jsonschema:"完整运行配置；必须包含 expectedRevision 和 idempotencyKey"`
}

func safeConfig(v runtimeconfig.View) runtimeconfig.View {
	// 不信任下游 DTO 对敏感值的处理，边界再次清空。
	for i := range v.Variables {
		if v.Variables[i].Sensitive {
			v.Variables[i].Value = ""
		}
	}
	for i := range v.Files {
		if v.Files[i].Sensitive {
			v.Files[i].Content = ""
		}
	}
	return v
}

func registerApplicationTools(server *sdk.Server, s scope) {
	typedTool(server, "app_get", "读取当前应用和当前权限能力。", annotations(true, false, true, false), func(ctx context.Context, _ emptyInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.ApplicationRead); err != nil {
			return nil, err
		}
		out, err := s.services.Applications.Get(ctx, s.principal, s.workspaceID, s.applicationID, "")
		if err != nil {
			return nil, failure(err)
		}
		return out, nil
	})
	if s.services.RuntimeConfigs == nil {
		return
	}
	typedTool(server, "config_get", "读取应用运行配置，敏感值只显示已配置状态。", annotations(true, false, true, false), func(ctx context.Context, _ emptyInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.ConfigRead); err != nil {
			return nil, err
		}
		out, err := s.services.RuntimeConfigs.Get(ctx, s.principal, s.workspaceID, s.applicationID)
		if err != nil {
			return nil, failure(err)
		}
		return safeConfig(out), nil
	})
	typedTool(server, "config_save", "保存完整运行配置草稿；不自动部署。相同幂等键只能重试相同请求。", annotations(false, true, true, false), func(ctx context.Context, in configInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.ConfigWrite); err != nil {
			return nil, err
		}
		if !validKey(in.Config.IdempotencyKey) || in.Config.ExpectedRevision < 0 {
			return nil, toolError("INVALID_ARGUMENT", "必须提供有效幂等键与非负 expectedRevision")
		}
		out, err := s.services.RuntimeConfigs.Save(ctx, s.principal, s.workspaceID, s.applicationID, in.Config)
		if err != nil {
			return nil, failure(err)
		}
		return safeConfig(out), nil
	})
}
