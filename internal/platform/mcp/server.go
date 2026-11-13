package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/source"
)

type scope struct {
	principal                  authorization.Principal
	workspaceID, applicationID string
	services                   Services
}

type errorBody struct {
	Code           string   `json:"code"`
	Message        string   `json:"message"`
	RequiredScopes []string `json:"requiredScopes,omitempty"`
}

func toolError(code, message string, scopes ...string) *sdk.CallToolResult {
	body := errorBody{Code: code, Message: message, RequiredScopes: scopes}
	text, _ := json.Marshal(body)
	return &sdk.CallToolResult{IsError: true, StructuredContent: body, Content: []sdk.Content{&sdk.TextContent{Text: string(text)}}}
}

func failure(err error) *sdk.CallToolResult {
	switch {
	case errors.Is(err, authorization.ErrUnauthenticated):
		return toolError("UNAUTHENTICATED", "身份已失效，请重新授权")
	case errors.Is(err, authorization.ErrDenied), errors.Is(err, authorization.ErrNotFound):
		return toolError("NOT_FOUND_OR_DENIED", "资源不存在或无权访问")
	case errors.Is(err, authorization.ErrConflict):
		return toolError("CONFLICT", "资源版本或幂等键冲突，请重新读取")
	case errors.Is(err, authorization.ErrInvalid):
		return toolError("INVALID_ARGUMENT", "输入无效，请检查工具参数")
	case errors.Is(err, source.ErrExpired), errors.Is(err, build.ErrSourceExpired):
		return toolError("SOURCE_EXPIRED", "源码快照已过期")
	case errors.Is(err, source.ErrNotReady), errors.Is(err, build.ErrSourceInvalid):
		return toolError("SOURCE_NOT_READY", "源码快照尚不可用")
	case errors.Is(err, inventory.ErrNotReady), errors.Is(err, deployment.ErrNotReady):
		return toolError("NOT_READY", "执行资源尚未就绪，请联系管理员")
	case errors.Is(err, deployment.ErrVerification), errors.Is(err, build.ErrVerification):
		return toolError("VERIFICATION_FAILED", "产物校验未通过")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return toolError("REQUEST_INTERRUPTED", "请求中断；若写入已经受理，请使用原幂等键查询或重试")
	default:
		return toolError("INTERNAL", "服务暂时不可用，请稍后重试")
	}
}

func (s scope) check(actions ...authorization.Action) *sdk.CallToolResult {
	if s.principal.UserID == "" || s.workspaceID == "" || s.applicationID == "" || !s.principal.IsBearer() {
		return toolError("UNAUTHENTICATED", "缺少有效身份或应用范围")
	}
	for _, action := range actions {
		if !s.principal.AllowsScope(s.applicationID, action) {
			return toolError("INSUFFICIENT_SCOPE", "当前凭证缺少执行此操作所需的范围", string(action))
		}
	}
	return nil
}

func annotations(readOnly, destructive, idempotent, openWorld bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive, IdempotentHint: idempotent, OpenWorldHint: &openWorld}
}

// typedTool 使用 SDK 根据 Go 类型生成并验证禁止未知字段的 JSON Schema。
func typedTool[In any](server *sdk.Server, name, description string, ann *sdk.ToolAnnotations, h func(context.Context, In) (any, *sdk.CallToolResult)) {
	sdk.AddTool[In, any](server, &sdk.Tool{Name: name, Description: description, Annotations: ann}, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		out, failure := h(ctx, in)
		if failure != nil {
			return failure, nil, nil
		}
		return nil, out, nil
	})
}

func newServer(s scope) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "yummy-application", Version: "1.0.0"}, nil)
	registerApplicationTools(server, s)
	if s.services.Sources != nil {
		registerSourceTools(server, s)
	}
	if s.services.Builds != nil {
		registerBuildTools(server, s)
	}
	if s.services.Deployments != nil {
		registerDeploymentTools(server, s)
	}
	return server
}

type emptyInput struct{}
type idInput struct {
	ID string `json:"id" jsonschema:"当前应用内的对象 ID"`
}
type pageInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"每页数量，上限 100"`
	Cursor string `json:"cursor,omitempty" jsonschema:"上次响应提供的分页游标"`
}

func validID(v string) bool { return applicationIDPattern.MatchString(v) }
func validKey(v string) bool {
	return len(v) > 0 && len(v) <= 128 && v == strings.TrimSpace(v) && !strings.ContainsAny(v, "\x00\r\n")
}
func validPage(v pageInput) bool { return v.Limit >= 0 && v.Limit <= 100 && len(v.Cursor) <= 2048 }
