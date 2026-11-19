package transport

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/identity"
	"github.com/sunshanpeng/yummy/internal/domains/oauth"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"go.uber.org/zap"
)

// 直连 handler 验证实例管理 OAuth RPC 的消息映射与错误码；会话层由 identity 测试覆盖。
func TestOAuthAdminHandlers(t *testing.T) {
	db := testkit.OpenDatabase(t)
	for _, value := range []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}, &model.User{ID: "admin", Username: "admin", PasswordHash: "hash"}} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.NewService(db, access)
	if err != nil {
		t.Fatal(err)
	}
	service, err := oauth.NewService(db, access, ids, oauth.Config{PublicBaseURL: "https://yummy.example", HMACSecret: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h := &OAuthAdminHandler{Service: service, Log: zap.NewNop()}
	root := authorization.Principal{UserID: "root", AuthVersion: 1}
	ctx := authorization.WithPrincipal(context.Background(), root)
	resource := "https://yummy.example/api"
	registered, err := h.RegisterOAuthClient(ctx, connect.NewRequest(&platformv1.RegisterOAuthClientRequest{ClientId: "web-app", Name: "Web 应用", RedirectUris: []string{"https://web.example/callback"}, Scopes: []string{"application:read"}, Resources: []string{resource}}))
	if err != nil {
		t.Fatal(err)
	}
	client := registered.Msg.GetClient()
	if client == nil || client.ClientId != "web-app" || client.Name != "Web 应用" || client.Source != "pre-registered" || !client.Active || client.CreatedAt == "" || len(client.RedirectUris) != 1 || client.RedirectUris[0] != "https://web.example/callback" || len(client.Scopes) != 1 {
		t.Fatalf("登记响应=%#v", client)
	}
	// 重复 client_id：Aborted；回调非法（http 非回环）：InvalidArgument。
	if _, err := h.RegisterOAuthClient(ctx, connect.NewRequest(&platformv1.RegisterOAuthClientRequest{ClientId: "web-app", Name: "Web 应用", RedirectUris: []string{"https://web.example/callback"}, Resources: []string{resource}})); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("重复登记=%v", err)
	}
	if _, err := h.RegisterOAuthClient(ctx, connect.NewRequest(&platformv1.RegisterOAuthClientRequest{ClientId: "bad-app", Name: "坏客户端", RedirectUris: []string{"http://web.example/callback"}, Resources: []string{resource}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("非法回调=%v", err)
	}
	// 键集分页：fixture 自带 yummy-cli，全局顺序为 web-app, yummy-cli。
	first, err := h.ListOAuthClients(ctx, connect.NewRequest(&platformv1.ListOAuthClientsRequest{Limit: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Msg.GetClients()) != 1 || first.Msg.GetClients()[0].GetClientId() != "web-app" || first.Msg.GetNextCursor() == "" {
		t.Fatalf("第一页=%#v cursor=%q", first.Msg.GetClients(), first.Msg.GetNextCursor())
	}
	second, err := h.ListOAuthClients(ctx, connect.NewRequest(&platformv1.ListOAuthClientsRequest{Limit: 10, Cursor: first.Msg.GetNextCursor()}))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Msg.GetClients()) != 1 || second.Msg.GetClients()[0].GetClientId() != "yummy-cli" || second.Msg.GetNextCursor() != "" {
		t.Fatalf("第二页=%#v cursor=%q", second.Msg.GetClients(), second.Msg.GetNextCursor())
	}
	// 停用返回更新后视图；不存在：NotFound。
	deactivated, err := h.SetOAuthClientActive(ctx, connect.NewRequest(&platformv1.SetOAuthClientActiveRequest{ClientId: "web-app"}))
	if err != nil {
		t.Fatal(err)
	}
	if deactivated.Msg.GetClient().GetClientId() != "web-app" || deactivated.Msg.GetClient().GetActive() {
		t.Fatalf("停用响应=%#v", deactivated.Msg.GetClient())
	}
	if _, err := h.SetOAuthClientActive(ctx, connect.NewRequest(&platformv1.SetOAuthClientActiveRequest{ClientId: "ghost", Active: true})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("停用缺失=%v", err)
	}
	// 非全局管理员三入口全拒：PermissionDenied；未认证：Unauthenticated。
	adminCtx := authorization.WithPrincipal(context.Background(), authorization.Principal{UserID: "admin", AuthVersion: 1})
	registerReq := connect.NewRequest(&platformv1.RegisterOAuthClientRequest{ClientId: "probe", Name: "probe", RedirectUris: []string{"https://probe.example/callback"}, Resources: []string{resource}})
	if _, err := h.RegisterOAuthClient(adminCtx, registerReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("普通用户登记=%v", err)
	}
	if _, err := h.ListOAuthClients(adminCtx, connect.NewRequest(&platformv1.ListOAuthClientsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("普通用户列表=%v", err)
	}
	if _, err := h.SetOAuthClientActive(adminCtx, connect.NewRequest(&platformv1.SetOAuthClientActiveRequest{ClientId: "web-app", Active: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("普通用户停用=%v", err)
	}
	if _, err := h.ListOAuthClients(context.Background(), connect.NewRequest(&platformv1.ListOAuthClientsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("匿名列表=%v", err)
	}
}
