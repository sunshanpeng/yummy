package oauth

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
)

func adminInput(id string) ClientInput {
	return ClientInput{ID: id, Name: "测试客户端", RedirectURIs: []string{"https://client.example/callback"}, Scopes: []string{"application:read"}, Resources: []string{baseURL + "/api"}}
}

func auditEvents(t *testing.T, db *gorm.DB, objectID string) []model.AuditEvent {
	t.Helper()
	var events []model.AuditEvent
	if err := db.Where("object_type = ? AND object_id = ?", "oauth_client", objectID).Order("created_at ASC, id ASC").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	return events
}

func TestAdminClientEntriesRejectNonInstanceAdmin(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// 普通成员与只读成员三入口全拒：实例级管理不因工作空间身份放行。
	for _, p := range []authorization.Principal{e.p, e.viewer} {
		if _, err := e.service.RegisterClientView(ctx, p, adminInput("probe-1")); !errors.Is(err, authorization.ErrDenied) {
			t.Fatalf("成员登记=%v", err)
		}
		if _, err := e.service.ListClients(ctx, p, 10, ""); !errors.Is(err, authorization.ErrDenied) {
			t.Fatalf("成员列表=%v", err)
		}
		if _, err := e.service.SetClientActiveView(ctx, p, CLIClientID, false); !errors.Is(err, authorization.ErrDenied) {
			t.Fatalf("成员停用=%v", err)
		}
	}
	// 未认证与冲突预检同事务先复核权限，匿名得不到存在性信息。
	if _, err := e.service.RegisterClientView(ctx, authorization.Principal{}, adminInput("probe-2")); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("匿名登记=%v", err)
	}
	if _, err := e.service.ListClients(ctx, authorization.Principal{}, 10, ""); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("匿名列表=%v", err)
	}
	if _, err := e.service.SetClientActiveView(ctx, authorization.Principal{}, CLIClientID, false); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("匿名停用=%v", err)
	}
}

func TestRegisterClientViewSuccessRecordsAudit(t *testing.T) {
	e := setup(t)
	view, err := e.service.RegisterClientView(context.Background(), e.root, adminInput("partner-app"))
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "partner-app" || view.Name != "测试客户端" || view.Source != "pre-registered" || !view.Active || len(view.RedirectURIs) != 1 || view.RedirectURIs[0] != "https://client.example/callback" || len(view.Scopes) != 1 || view.Scopes[0] != "application:read" || len(view.Resources) != 1 || view.CreatedAt.IsZero() {
		t.Fatalf("登记视图=%+v", view)
	}
	events := auditEvents(t, e.db, "partner-app")
	if len(events) != 1 || events[0].Action != "instance.oauth.client.register" || events[0].ActorID != "root" || events[0].Result != "SUCCESS" {
		t.Fatalf("审计记录=%+v", events)
	}
	// 元数据非法：ErrClient，且不产生登记审计。
	if _, err := e.service.RegisterClientView(context.Background(), e.root, ClientInput{ID: "bad-app", Name: "坏客户端", RedirectURIs: []string{"http://client.example/callback"}, Resources: []string{baseURL + "/api"}}); !errors.Is(err, ErrClient) {
		t.Fatalf("http非回环回调=%v", err)
	}
	if events := auditEvents(t, e.db, "bad-app"); len(events) != 0 {
		t.Fatalf("非法登记产生审计=%+v", events)
	}
}

func TestRegisterClientViewDuplicateConflict(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.service.RegisterClientView(ctx, e.root, adminInput("dup-app")); err != nil {
		t.Fatal(err)
	}
	// 底层登记对重复主键静默幂等，管理入口必须显式冲突且不改动既有客户端。
	if _, err := e.service.RegisterClientView(ctx, e.root, adminInput("dup-app")); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("重复登记=%v", err)
	}
	var client clientRecord
	if err := e.db.First(&client, "id = ?", "dup-app").Error; err != nil {
		t.Fatal(err)
	}
	if client.Name != "测试客户端" || client.Source != "pre-registered" {
		t.Fatalf("重复登记改动既有客户端=%+v", client)
	}
	if events := auditEvents(t, e.db, "dup-app"); len(events) != 1 {
		t.Fatalf("重复登记审计=%+v", events)
	}
}

func TestListClientsKeysetPaginationAndCursorScope(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// 乱序登记；fixture 自带 yummy-cli，全局按 id ASC 应为 cli-a, cli-b, cli-c, yummy-cli。
	for _, id := range []string{"cli-b", "cli-a", "cli-c"} {
		if _, err := e.service.RegisterClientView(ctx, e.root, adminInput(id)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := e.service.ListClients(ctx, e.root, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Items[0].ID != "cli-a" || first.Items[1].ID != "cli-b" || first.NextCursor == "" {
		t.Fatalf("第一页=%+v cursor=%q", first.Items, first.NextCursor)
	}
	second, err := e.service.ListClients(ctx, e.root, 2, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.Items[0].ID != "cli-c" || second.Items[1].ID != "yummy-cli" || second.NextCursor != "" {
		t.Fatalf("第二页=%+v cursor=%q", second.Items, second.NextCursor)
	}
	// 大页一次取完，末页游标为空；空结果页同样为空。
	all, err := e.service.ListClients(ctx, e.root, 100, "")
	if err != nil || len(all.Items) != 4 || all.NextCursor != "" {
		t.Fatalf("全量=%d cursor=%q err=%v", len(all.Items), all.NextCursor, err)
	}
	// 游标绑定列表作用域：跨列表或损坏的游标一律拒绝。
	if _, err := e.service.ListClients(ctx, e.root, 2, store.NextCursor("instance-node-qualifications", "cli-a")); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("跨列表游标=%v", err)
	}
	if _, err := e.service.ListClients(ctx, e.root, 2, "!!!不是游标!!!"); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("损坏游标=%v", err)
	}
	// 非全局管理员列表拒绝。
	if _, err := e.service.ListClients(ctx, e.p, 2, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("成员列表=%v", err)
	}
}

func TestSetClientActiveViewRevokesGrantsAndLists(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// 通过既有授权流程给第一方 CLI 客户端制造真实授权。
	code := e.code(t, e.p, baseURL+"/api", "application:read build:read offline_access", true)
	status, tokens := e.token(t, exchange(code, baseURL+"/api"))
	if status != 200 || tokens.AccessToken == "" {
		t.Fatalf("token=%d %+v", status, tokens)
	}
	if _, err := e.service.Authenticate(ctx, tokens.AccessToken, baseURL+"/api"); err != nil {
		t.Fatal(err)
	}
	view, err := e.service.SetClientActiveView(ctx, e.root, CLIClientID, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != CLIClientID || view.Active {
		t.Fatalf("停用视图=%+v", view)
	}
	// 停用即撤销既有授权并停用协议会话；令牌立即失效。
	var grants []grantRecord
	if err := e.db.Where("client_id = ?", CLIClientID).Find(&grants).Error; err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].RevokedAt == nil {
		t.Fatalf("授权撤销=%+v", grants)
	}
	if _, err := e.service.Authenticate(ctx, tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("停用后令牌仍可用=%v", err)
	}
	page, err := e.service.ListClients(ctx, e.root, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	for _, item := range page.Items {
		if item.ID == CLIClientID {
			inactive = !item.Active
		}
	}
	if !inactive {
		t.Fatalf("列表未反映停用=%+v", page.Items)
	}
	events := auditEvents(t, e.db, CLIClientID)
	if len(events) != 1 || events[0].Action != "instance.oauth.client.status" || events[0].ActorID != "root" {
		t.Fatalf("停用审计=%+v", events)
	}
	// 重新启用后列表恢复 active，但已撤销授权不复活。
	enabled, err := e.service.SetClientActiveView(ctx, e.root, CLIClientID, true)
	if err != nil || !enabled.Active {
		t.Fatalf("启用视图=%+v err=%v", enabled, err)
	}
	if _, err := e.service.Authenticate(ctx, tokens.AccessToken, baseURL+"/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("启用复活已撤销授权=%v", err)
	}
	// 不存在的客户端：ErrNotFound。
	if _, err := e.service.SetClientActiveView(ctx, e.root, "ghost-client", true); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("停用缺失=%v", err)
	}
}

func TestCleanupExpiredRequestsBoundsAndRemoves(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	if err := env.db.Create(&clientRecord{ID: "cleanup-client", Name: "清理探针", Source: "pre-registered", RedirectURIs: []string{"https://127.0.0.1/callback"}, Scopes: []string{"application:read"}, Resources: []string{"http://127.0.0.1:8080/api"}, Active: true}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := consentRecord{ID: "req_expired", Parameters: url.Values{"client_id": {"cleanup-client"}}, CSRFHash: "stale", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour)}
	live := consentRecord{ID: "req_live", Parameters: url.Values{"client_id": {"cleanup-client"}}, CSRFHash: "stale", ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now}
	for _, record := range []*consentRecord{&expired, &live} {
		if err := env.db.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	removed, err := env.service.CleanupExpiredRequests(ctx, 100)
	if err != nil || removed != 1 {
		t.Fatalf("清理应删除且仅删除过期行: removed=%d err=%v", removed, err)
	}
	var count int64
	if err := env.db.Model(&consentRecord{}).Where("id = ?", "req_live").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("未过期记录不应被删除: count=%d err=%v", count, err)
	}
	if _, err := env.service.CleanupExpiredRequests(ctx, 0); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("非法 limit 应拒绝: %v", err)
	}
	// 幂等：再次清理无剩余行，返回 0。
	again, err := env.service.CleanupExpiredRequests(ctx, 100)
	if err != nil || again != 0 {
		t.Fatalf("重复清理应返回0: removed=%d err=%v", again, err)
	}
}
