package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
)

const maxRequestBytes = 3 << 20
const resourcePrefix = "/mcp/apps/"
const metadataPrefix = "/.well-known/oauth-protected-resource/mcp/apps/"

var applicationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// NewHTTPHandler 返回包含远程 MCP 与受保护资源元数据的 handler；调用方只需挂载两个路径前缀。
// MCP 协议由官方 SDK 处理，Stateless 模式确保每次 HTTP 请求认证并绑定新身份。
func NewHTTPHandler(config Config, services Services) (http.Handler, error) {
	base, err := validateBase(config.PublicBaseURL, config.AllowInsecureLoopback)
	if err != nil || config.Authenticate == nil || services.Applications == nil {
		return nil, errors.New("MCP 配置、认证器或应用服务无效")
	}
	timeout := config.RequestTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	if timeout < time.Second || timeout > 30*time.Second {
		return nil, errors.New("MCP 请求超时须在 1～30 秒之间")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var prefix string
		switch {
		case strings.HasPrefix(r.URL.Path, metadataPrefix):
			prefix = metadataPrefix
		case strings.HasPrefix(r.URL.Path, resourcePrefix):
			prefix = resourcePrefix
		default:
			http.NotFound(w, r)
			return
		}
		app := strings.TrimPrefix(r.URL.Path, prefix)
		// RawPath、转义、查询和不规范标识都不能参与 resource 生成。
		if !applicationIDPattern.MatchString(app) || r.URL.EscapedPath() != r.URL.Path || r.URL.RawQuery != "" || r.URL.Fragment != "" {
			http.Error(w, "无效资源", http.StatusBadRequest)
			return
		}
		resource := base + resourcePrefix + app
		metadataURL := base + metadataPrefix + app
		if prefix == resourcePrefix {
			// MCP 仅允许可信站点和无 Origin 的非浏览器客户端；元数据本身仍可公开跨域发现。
			origins := r.Header.Values("Origin")
			if len(origins) > 1 || len(origins) == 1 && origins[0] != base {
				http.Error(w, "不允许跨域请求", http.StatusForbidden)
				return
			}
		}
		if prefix == metadataPrefix {
			sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
				Resource: resource, AuthorizationServers: []string{base},
				ScopesSupported: slices.Clone(authorization.TokenScopes), BearerMethodsSupported: []string{"header"},
			}).ServeHTTP(w, r)
			return
		}
		challenge := fmt.Sprintf(`Bearer resource_metadata=%q`, metadataURL)
		authHeaders := r.Header.Values("Authorization")
		if len(authHeaders) != 1 {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "需要 OAuth Bearer", http.StatusUnauthorized)
			return
		}
		parts := strings.Fields(authHeaders[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 4096 {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "需要 OAuth Bearer", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		p, err := config.Authenticate(ctx, parts[1], resource)
		if err != nil || p.UserID == "" || p.AuthKind != "oauth" || p.TokenID != "" || p.OAuthGrantID == "" || p.OAuthTokenSignature == "" || p.Audience != resource || len(p.ApplicationIDs) != 1 || p.ApplicationIDs[0] != app {
			w.Header().Set("WWW-Authenticate", challenge+`, error="invalid_token"`)
			http.Error(w, "OAuth 凭证无效", http.StatusUnauthorized)
			return
		}
		if !p.AllowsScope(app, authorization.ApplicationRead) {
			w.Header().Set("WWW-Authenticate", challenge+`, error="insufficient_scope", scope="application:read"`)
			http.Error(w, "权限范围不足", http.StatusForbidden)
			return
		}
		workspace, err := services.Applications.ResolveWorkspace(ctx, p, app)
		if err != nil || workspace == "" {
			// 不泄露应用是否存在或授权变更细节。
			http.Error(w, "应用不存在或无权访问", http.StatusForbidden)
			return
		}
		server := newServer(scope{principal: p, workspaceID: workspace, applicationID: app, services: services})
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
			Stateless: true, MaxRequestBodyBytes: maxRequestBytes, PropagateRequestCancellation: true,
		})
		// SDK 对每次 stateless 请求建立独立会话。移除认证头，防止工具意外读取原始令牌。
		request := r.Clone(ctx)
		request.Header = r.Header.Clone()
		request.Header.Del("Authorization")
		request.Header.Del("Cookie")
		handler.ServeHTTP(w, request)
	}), nil
}

func validateBase(raw string, allowLoopback bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", errors.New("无效公开地址")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if !(allowLoopback && u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
			return "", errors.New("公开地址必须为 HTTPS")
		}
	}
	return strings.TrimRight(raw, "/"), nil
}
