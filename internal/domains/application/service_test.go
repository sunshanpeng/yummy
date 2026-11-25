package application_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"gorm.io/gorm"
)

func fixture(t *testing.T) (*gorm.DB, *application.Service, authorization.Principal, authorization.Principal) {
	t.Helper()
	db := testkit.OpenDatabase(t)
	values := []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}, &model.User{ID: "alice", Username: "alice", PasswordHash: "hash"}, &model.User{ID: "bob", Username: "bob", PasswordHash: "hash"}, &model.User{ID: "outsider", Username: "outsider", PasswordHash: "hash"}, &model.Workspace{ID: "space-a", Name: "甲", Slug: "space-a"}, &model.Workspace{ID: "space-b", Name: "乙", Slug: "space-b"}, &model.WorkspaceMember{WorkspaceID: "space-a", UserID: "root", Role: "admin"}, &model.WorkspaceMember{WorkspaceID: "space-a", UserID: "alice", Role: "member"}, &model.WorkspaceMember{WorkspaceID: "space-a", UserID: "bob", Role: "member"}, &model.WorkspaceMember{WorkspaceID: "space-b", UserID: "outsider", Role: "admin"}}
	for _, value := range values {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	// 归档前置校验接真实部署域实现（StoppedForArchive 只读运行状态，不依赖
	// 运行配置/校验器），与控制面装配一致。
	return db, application.NewService(db, access, deployment.NewService(db, access, nil, nil, nil), build.NewService(db, access, nil, nil)), authorization.Principal{UserID: "root", AuthVersion: 1}, authorization.Principal{UserID: "alice", AuthVersion: 1}
}

func TestCreateApplicationIsNameOnlyIdempotentAndAuditedOnce(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	if _, err := service.Create(ctx, alice, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "one"}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("member create=%v", err)
	}
	if _, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway"}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("empty key=%v", err)
	}
	for _, name := range []string{"1gateway", "gateway-", "UPPER", "-bad"} {
		if _, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: name, IdempotencyKey: "bad"}); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("invalid name %q=%v", name, err)
		}
	}
	value, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "one"})
	if err != nil || replayed.ID != value.ID {
		t.Fatalf("replay=%#v,%v", replayed, err)
	}
	if _, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "changed", IdempotencyKey: "one"}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("fingerprint conflict=%v", err)
	}
	if _, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "two"}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("duplicate name=%v", err)
	}
	if _, err := service.Create(ctx, root, "space-b", application.CreateInput{Name: "gateway", IdempotencyKey: "one"}); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&model.AuditEvent{}).Where("workspace_id = ? AND action = ?", "space-a", "application.create").Count(&count)
	if count != 1 {
		t.Fatalf("创建审计=%d", count)
	}
	var keys int64
	db.Model(&model.IdempotencyKey{}).Where("workspace_id = ?", "space-a").Count(&keys)
	if keys != 1 {
		t.Fatalf("失败请求不应保留幂等键=%d", keys)
	}
}

func TestWorkspaceAdminCanCreateAndGrantWithoutInstancePermission(t *testing.T) {
	db, service, _, alice := fixture(t)
	ctx := context.Background()
	if err := db.Model(&model.WorkspaceMember{}).Where("workspace_id = ? AND user_id = ?", "space-a", "alice").Update("role", "admin").Error; err != nil {
		t.Fatal(err)
	}
	value, err := service.Create(ctx, alice, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "space-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(value.Capabilities, "member:write") {
		t.Fatal("空间管理员缺少应用授权能力")
	}
	if err := service.SetMember(ctx, alice, "space-a", value.ID, "bob", "developer"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, alice, "space-b", application.CreateInput{Name: "blocked", IdempotencyKey: "cross-space"}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("跨空间创建 = %v", err)
	}
}

func TestConcurrentIdempotentCreateUsesDatabaseLock(t *testing.T) {
	db, service, root, _ := fixture(t)
	ctx := context.Background()
	otherAccess, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	other := application.NewService(db, otherAccess, service.Runtimes, service.Builds)
	start := make(chan struct{})
	ids := make(chan string, 2)
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for _, current := range []*application.Service{service, other} {
		workers.Add(1)
		go func(current *application.Service) {
			defer workers.Done()
			<-start
			value, err := current.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "same"})
			if err != nil {
				failures <- err
			} else {
				ids <- value.ID
			}
		}(current)
	}
	close(start)
	workers.Wait()
	close(failures)
	close(ids)
	for err := range failures {
		t.Fatal(err)
	}
	var first string
	count := 0
	for id := range ids {
		count++
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("重复创建不同结果=%s,%s", first, id)
		}
	}
	if count != 2 {
		t.Fatalf("返回数量=%d", count)
	}
	var applications int64
	db.Model(&model.Application{}).Count(&applications)
	if applications != 1 {
		t.Fatalf("实际应用=%d", applications)
	}
}

func TestApplicationListsAndCapabilitiesRespectMembershipAndTokenScope(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	one, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "one", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "two", IdempotencyKey: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetMember(ctx, root, "space-a", one.ID, "alice", "viewer"); err != nil {
		t.Fatal(err)
	}
	page, err := service.List(ctx, alice, "space-a", 20, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != one.ID {
		t.Fatalf("authorized list=%#v,%v", page, err)
	}
	if slices.Contains(page.Items[0].Capabilities, "build:write") {
		t.Fatal("viewer不能有写权限")
	}
	if _, err := service.Get(ctx, alice, "space-a", two.ID, ""); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("unauthorized get=%v", err)
	}
	if _, err := service.Get(ctx, root, "space-b", one.ID, ""); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("cross workspace=%v", err)
	}
	page, err = service.List(ctx, root, "space-a", 1, "")
	if err != nil || page.NextCursor == "" {
		t.Fatalf("page one=%#v,%v", page, err)
	}
	next, err := service.List(ctx, root, "space-a", 1, page.NextCursor)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("page next=%#v,%v", next, err)
	}
	if _, err := service.List(ctx, root, "space-b", 1, page.NextCursor); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("cross scope cursor=%v", err)
	}
	token := model.AccessToken{ID: "token", UserID: "root", Name: "read", TokenHash: "digest", Scopes: []string{"application:read", "build:write"}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	bearer := authorization.Principal{UserID: "root", AuthVersion: 1, TokenID: token.ID}
	page, err = service.List(ctx, bearer, "space-a", 20, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("empty token scope=%#v,%v", page, err)
	}
	if err := db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: "space-a", ApplicationID: one.ID}).Error; err != nil {
		t.Fatal(err)
	}
	page, err = service.List(ctx, bearer, "space-a", 20, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != one.ID {
		t.Fatalf("token scope=%#v,%v", page, err)
	}
	if slices.Contains(page.Items[0].Capabilities, "member:write") || slices.Contains(page.Items[0].Capabilities, "config:write") {
		t.Fatal("PAT不能继承管理员能力")
	}
	if _, err := service.Create(ctx, bearer, "space-a", application.CreateInput{Name: "blocked", IdempotencyKey: "blocked"}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT create=%v", err)
	}
}

func TestMemberAuthorizationRequiresSpaceAdminAndActiveSameSpaceMember(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	app, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetMember(ctx, root, "space-a", app.ID, "alice", "maintainer"); err != nil {
		t.Fatal(err)
	}
	if err := service.SetMember(ctx, alice, "space-a", app.ID, "bob", "viewer"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("maintainer grants=%v", err)
	}
	if err := service.SetMember(ctx, root, "space-a", app.ID, "outsider", "viewer"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("outsider grant=%v", err)
	}
	if err := service.SetMember(ctx, root, "space-a", app.ID, "root", "viewer"); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("inherited admin grant=%v", err)
	}
	if err := service.RemoveMember(ctx, root, "space-a", app.ID, "root"); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("inherited admin revoke=%v", err)
	}
	members, err := service.ListMembers(ctx, root, "space-a", app.ID, 20, "")
	if err != nil || len(members.Items) != 2 {
		t.Fatalf("members=%#v,%v", members, err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", "bob").Update("status", model.UserDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.SetMember(ctx, root, "space-a", app.ID, "bob", "viewer"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("disabled grant=%v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", "alice").Update("status", model.UserDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveMember(ctx, root, "space-a", app.ID, "alice"); err != nil {
		t.Fatalf("disabled user's grant must remain removable: %v", err)
	}
}

func TestAuditFailureRollsBackApplicationAndIdempotencyKey(t *testing.T) {
	db, service, root, _ := fixture(t)
	ctx := context.Background()
	// 在独占 schema 注入审计写失败，验证业务与幂等结果不会先于审计提交。
	if err := db.Exec(`ALTER TABLE audit_events ADD CONSTRAINT test_reject_application_create CHECK (action <> 'application.create')`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "retry"}); err == nil {
		t.Fatal("审计失败时必须拒绝业务变更")
	}
	var applications, keys int64
	if err := db.Model(&model.Application{}).Count(&applications).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.IdempotencyKey{}).Count(&keys).Error; err != nil {
		t.Fatal(err)
	}
	if applications != 0 || keys != 0 {
		t.Fatalf("事务未完整回滚: applications=%d, keys=%d", applications, keys)
	}
	if err := db.Exec(`ALTER TABLE audit_events DROP CONSTRAINT test_reject_application_create`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "retry"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRevisionAndArchiveBoundaries(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	app, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetMember(ctx, root, "space-a", app.ID, "alice", "maintainer"); err != nil {
		t.Fatal(err)
	}
	// 应用名创建后不可修改：改名请求被拒绝，且不消耗修订号。
	if _, err := service.Update(ctx, alice, "space-a", app.ID, application.UpdateInput{Name: "renamed", ExpectedRevision: 1}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("应用名不可修改=%v", err)
	}
	updated, err := service.Update(ctx, alice, "space-a", app.ID, application.UpdateInput{Name: "gateway", Description: "说明", ExpectedRevision: 1})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update=%#v,%v", updated, err)
	}
	if _, err := service.Update(ctx, alice, "space-a", app.ID, application.UpdateInput{Name: "gateway", ExpectedRevision: 1}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("stale update=%v", err)
	}
	if err := service.Archive(ctx, alice, "space-a", app.ID, 2); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("maintainer archive=%v", err)
	}
	if err := service.Archive(ctx, root, "space-a", app.ID, 1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("stale archive=%v", err)
	}
	if err := service.Archive(ctx, root, "space-a", app.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, root, "space-a", app.ID, ""); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("archived get=%v", err)
	}
	var events int64
	db.Model(&model.AuditEvent{}).Where("application_id = ? AND action IN ?", app.ID, []string{"application.update", "application.archive"}).Count(&events)
	if events != 2 {
		t.Fatalf("write audits=%d", events)
	}
}

// TestArchiveRequiresStoppedRuntime 验证归档前的运行检查：期望运行（或在途部署）
// 的应用拒绝归档——归档后平台停止接口会因活跃状态校验拒绝，工作负载将失控运行；
// 已停止（或从未部署）的应用可正常归档。
func TestArchiveRequiresStoppedRuntime(t *testing.T) {
	db, service, root, _ := fixture(t)
	ctx := context.Background()
	app, err := service.Create(ctx, root, "space-a", application.CreateInput{Name: "gateway", IdempotencyKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	state := func(query string, args ...any) error {
		t.Helper()
		return db.Exec("UPDATE application_runtime_states SET "+query, args...).Error
	}
	insert := "INSERT INTO application_runtime_states(id, workspace_id, application_id, generation, desired_state, observed_state) VALUES (?, ?, ?, 1, 'RUN', 'PENDING')"
	if err := db.Exec(insert, app.ID, "space-a", app.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.Archive(ctx, root, "space-a", app.ID, 1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("运行中的应用可归档=%v", err)
	}
	if err := state("desired_state='STOP', observed_state='UNKNOWN' WHERE id = ?", app.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Archive(ctx, root, "space-a", app.ID, 1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("停止失败且无在途操作的应用可归档=%v", err)
	}
	// 停止完成后（期望 STOP 且无在途操作）允许归档。
	if err := state("desired_state='STOP', observed_state='STOPPED' WHERE id = ?", app.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Archive(ctx, root, "space-a", app.ID, 1); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteArchivedUnusedApplicationPreservesHistoryAndReleasesName(t *testing.T) {
	db, service, root, alice := fixture(t)
	ctx := context.Background()
	const ws = "space-a"
	input := application.CreateInput{Name: "gateway", IdempotencyKey: "original"}
	app, err := service.Create(ctx, root, ws, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, root, ws, app.ID, 1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("活动应用不能删除：%v", err)
	}
	if err := service.Archive(ctx, root, ws, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	page, err := service.ListArchived(ctx, root, ws, 24, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != app.ID || page.Items[0].Revision != 2 || !page.Items[0].DeletionReady {
		t.Fatalf("归档清单：%+v，%v", page, err)
	}
	if _, err := service.ListArchived(ctx, alice, ws, 24, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员能枚举归档应用：%v", err)
	}
	if err := service.Delete(ctx, alice, ws, app.ID, 2); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员能删除归档应用：%v", err)
	}
	if err := service.Delete(ctx, root, ws, app.ID, 1); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("旧版本能删除归档应用：%v", err)
	}
	if err := service.Delete(ctx, root, ws, app.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, root, ws, app.ID, 2); err != nil {
		t.Fatalf("重复删除应幂等：%v", err)
	}
	if page, err := service.ListArchived(ctx, root, ws, 24, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("归档清单仍有已删除应用：%+v，%v", page, err)
	}
	input.IdempotencyKey = "recreated"
	newApp, err := service.Create(ctx, root, ws, input)
	if err != nil || newApp.ID == app.ID {
		t.Fatalf("同名重建：%+v，%v", newApp, err)
	}
	if view, err := service.Get(ctx, root, ws, "", app.Name); err != nil || view.ID != newApp.ID {
		t.Fatalf("同名查找：%+v，%v", view, err)
	}
	var count int64
	if err := db.Model(&model.AuditEvent{}).Where("application_id = ? AND action = ?", app.ID, "application.delete").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("删除审计数 %d，%v", count, err)
	}
}
