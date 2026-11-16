package workspace_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"github.com/sunshanpeng/yummy/internal/platform/workspace"
	"gorm.io/gorm"
)

func fixture(t *testing.T) (*gorm.DB, *workspace.Service, authorization.Principal, authorization.Principal) {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, user := range []model.User{{ID: "root", Username: "root", PasswordHash: "hash", Status: model.UserActive, AuthVersion: 1, InstanceAdmin: true}, {ID: "alice", Username: "alice", PasswordHash: "hash", Status: model.UserActive, AuthVersion: 1}, {ID: "bob", Username: "bob", PasswordHash: "hash", Status: model.UserActive, AuthVersion: 1}} {
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, workspace.NewService(db, access), authorization.Principal{UserID: "root", AuthVersion: 1}, authorization.Principal{UserID: "alice", AuthVersion: 1}
}

func TestCreateWorkspaceRequiresInstanceAdminAndUpdatesUseRevision(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	if _, err := service.Create(ctx, alice, workspace.CreateInput{Name: "空间", Slug: "space"}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通用户创建 = %v", err)
	}
	if _, err := service.Create(ctx, root, workspace.CreateInput{Name: "空间", Slug: "INVALID"}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("非法slug = %v", err)
	}
	value, err := service.Create(ctx, root, workspace.CreateInput{Name: " 空间甲 ", Slug: "space-a", Description: " 说明 "})
	if err != nil {
		t.Fatal(err)
	}
	if value.Name != "空间甲" || value.Description != "说明" || value.Revision != 1 {
		t.Fatalf("创建结果=%#v", value)
	}
	var member model.WorkspaceMember
	if err := db.First(&member, "workspace_id = ? AND user_id = ?", value.ID, root.UserID).Error; err != nil || member.Role != "admin" {
		t.Fatalf("创建者管理员=%#v,%v", member, err)
	}
	// 空间名（slug）创建后不可修改：改名请求被拒绝，且不消耗修订号。
	if _, err := service.Update(ctx, root, value.ID, workspace.UpdateInput{Name: "改名", Slug: "space-b", ExpectedRevision: 1}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("空间名不可修改=%v", err)
	}
	updated, err := service.Update(ctx, root, value.ID, workspace.UpdateInput{Name: "空间乙", Slug: "space-a", Description: "", ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 {
		t.Fatal(updated)
	}
	if _, err := service.Update(ctx, root, value.ID, workspace.UpdateInput{Name: "过期", Slug: "space-a", ExpectedRevision: 1}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("过期revision=%v", err)
	}
	if _, err := service.Create(ctx, root, workspace.CreateInput{Name: "重复", Slug: "space-a"}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("重复slug=%v", err)
	}
	var audits int64
	if err := db.Model(&model.AuditEvent{}).Count(&audits).Error; err != nil || audits != 2 {
		t.Fatalf("审计=%d,%v", audits, err)
	}
	if err := service.Archive(ctx, root, value.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, root, value.ID); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("归档Get=%v", err)
	}
}

func TestWorkspaceListsFilterMembershipBearerScopeAndPagination(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	first, err := service.Create(ctx, root, workspace.CreateInput{Name: "甲", Slug: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(ctx, root, workspace.CreateInput{Name: "乙", Slug: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.WorkspaceMember{WorkspaceID: first.ID, UserID: alice.UserID, Role: "member"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Application{ID: "app-one", WorkspaceID: first.ID, Name: "gateway"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Application{ID: "app-two", WorkspaceID: second.ID, Name: "gateway"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ApplicationMember{WorkspaceID: first.ID, ApplicationID: "app-one", UserID: alice.UserID, Role: "viewer"}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.List(ctx, alice, 20, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID {
		t.Fatalf("成员过滤=%#v,%v", page, err)
	}
	page, err = service.List(ctx, root, 1, "")
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("第一页=%#v,%v", page, err)
	}
	next, err := service.List(ctx, root, 1, page.NextCursor)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("下一页=%#v,%v", next, err)
	}
	if _, err := service.List(ctx, alice, 1, page.NextCursor); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("跨用户游标=%v", err)
	}
	token := model.AccessToken{ID: "token", UserID: root.UserID, Name: "只读", TokenHash: "digest", Scopes: []string{"application:read"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	bearer := authorization.Principal{UserID: root.UserID, AuthVersion: 1, TokenID: token.ID}
	page, err = service.List(ctx, bearer, 20, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("空PAT范围=%#v,%v", page, err)
	}
	if err := db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: first.ID, ApplicationID: "app-one"}).Error; err != nil {
		t.Fatal(err)
	}
	page, err = service.List(ctx, bearer, 20, "")
	if err != nil || len(page.Items) != 1 || len(page.Items[0].Capabilities) != 1 {
		t.Fatalf("PAT只读空间=%#v,%v", page, err)
	}
	if _, err := service.Get(ctx, bearer, second.ID); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("跨PAT范围Get=%v", err)
	}
	if _, err := service.ListMembers(ctx, bearer, first.ID, 20, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT空间成员管理=%v", err)
	}
}

func TestMemberRemovalDropsApplicationGrantAndProtectsLastAdmin(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	space, err := service.Create(ctx, root, workspace.CreateInput{Name: "甲", Slug: "one"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{&model.WorkspaceMember{WorkspaceID: space.ID, UserID: "alice", Role: "member"}, &model.Application{ID: "app", WorkspaceID: space.ID, Name: "gateway"}, &model.ApplicationMember{WorkspaceID: space.ID, ApplicationID: "app", UserID: "alice", Role: "viewer"}} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := service.RemoveMember(ctx, root, space.ID, root.UserID); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("最后管理员=%v", err)
	}
	if err := service.ChangeMemberRole(ctx, root, space.ID, root.UserID, "member"); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("最后管理员降级=%v", err)
	}
	if _, err := service.ListMembers(ctx, alice, space.ID, 20, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("成员管理=%v", err)
	}
	members, err := service.ListMembers(ctx, root, space.ID, 20, "")
	if err != nil || len(members.Items) != 2 {
		t.Fatalf("成员列表=%#v,%v", members, err)
	}
	for _, member := range members.Items {
		if member.UserID == "alice" && (len(member.Applications) != 1 || member.Applications[0].Role != "viewer") {
			t.Fatal(member)
		}
	}
	if err := service.RemoveMember(ctx, root, space.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	var grants int64
	db.Model(&model.ApplicationMember{}).Where("user_id = ?", "alice").Count(&grants)
	if grants != 0 {
		t.Fatal("空间移除未清除应用授权")
	}
	if err := db.Create(&model.WorkspaceMember{WorkspaceID: space.ID, UserID: "alice", Role: "member"}).Error; err != nil {
		t.Fatal(err)
	}
	db.Model(&model.ApplicationMember{}).Where("user_id = ?", "alice").Count(&grants)
	if grants != 0 {
		t.Fatal("重加空间成员不应恢复旧授权")
	}
	var audits int64
	db.Model(&model.AuditEvent{}).Where("action IN ?", []string{"workspace.member.remove", "application.member.remove"}).Count(&audits)
	if audits != 2 {
		t.Fatalf("移除审计=%d", audits)
	}
}

func TestConcurrentDemotionsCannotRemoveBothAdmins(t *testing.T) {
	db, service, root, _ := fixture(t)
	ctx := context.Background()
	space, err := service.Create(ctx, root, workspace.CreateInput{Name: "甲", Slug: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.WorkspaceMember{WorkspaceID: space.ID, UserID: "alice", Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	// 两个 Access 实例绕开进程锁，验证数据库 workspace 行锁仍保护不变量。
	otherAccess, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	other := workspace.NewService(db, otherAccess)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, entry := range []struct {
		service *workspace.Service
		user    string
	}{{service, "root"}, {other, "alice"}} {
		workers.Add(1)
		go func(entry struct {
			service *workspace.Service
			user    string
		}) {
			defer workers.Done()
			<-start
			results <- entry.service.ChangeMemberRole(ctx, root, space.ID, entry.user, "member")
		}(entry)
	}
	close(start)
	workers.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, authorization.ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("并发降级成功次数=%d", success)
	}
	var admins int64
	db.Model(&model.WorkspaceMember{}).Where("workspace_id = ? AND role = ?", space.ID, "admin").Count(&admins)
	if admins != 1 {
		t.Fatalf("剩余管理员=%d", admins)
	}
}

// 自由建空间（2026-10-26 设计对齐）：开关关→非实例管理员拒绝；开关开→登录身份
// 可建并自动成为空间管理员；保留名单（kube-system 等集群/平台命名空间派生源）一律拒绝。
func TestWorkspaceSelfCreateToggleAndReservedNames(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()

	// 默认关：非实例管理员创建被拒。
	if _, err := service.Create(ctx, alice, workspace.CreateInput{Name: "演示空间", Slug: "self-create-1"}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("默认关闭时应拒绝: %v", err)
	}
	// 开启开关：instance_settings 单例行由迁移建表后插入（fixture 无实例行）。
	if err := db.Exec("INSERT INTO instance_settings (singleton, id, policy_revision, created_at, workspace_self_create) VALUES (TRUE, 'ins_test', 0, NOW(), TRUE)").Error; err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, alice, workspace.CreateInput{Name: "演示空间", Slug: "self-create-2"})
	if err != nil {
		t.Fatalf("开关开启后应允许: %v", err)
	}
	if created.Role != "workspace_admin" {
		t.Fatalf("创建者应成为空间管理员: %s", created.Role)
	}
	if _, err := service.Create(ctx, root, workspace.CreateInput{Name: "管理员空间", Slug: "admin-ws"}); err != nil {
		t.Fatalf("实例管理员创建应继续可用: %v", err)
	}
	// 保留名单：集群/平台命名空间派生源一律拒绝（实例管理员亦不豁免）。
	for _, slug := range []string{"kube-system", "default", "yummy-system"} {
		if _, err := service.Create(ctx, root, workspace.CreateInput{Name: "保留测试", Slug: slug}); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("保留名 %s 应被拒绝: %v", slug, err)
		}
	}
}
