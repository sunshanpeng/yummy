package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
)

func TestListAuditEventsScopeAndPagination(t *testing.T) {
	db := testkit.OpenDatabase(t)
	at := time.Now().UTC().Truncate(time.Second)
	space, other := "space-a", "space-b"
	fixtures := []any{
		&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true},
		&model.User{ID: "admin", Username: "admin", PasswordHash: "hash"},
		&model.User{ID: "member", Username: "member", PasswordHash: "hash"},
		&model.Workspace{ID: space, Slug: space, Name: space},
		&model.Workspace{ID: other, Slug: other, Name: other},
		&model.WorkspaceMember{WorkspaceID: space, UserID: "admin", Role: model.WorkspaceAdmin},
		&model.WorkspaceMember{WorkspaceID: space, UserID: "member", Role: "member"},
		&model.AuditEvent{ID: "evt-1", WorkspaceID: &space, Action: "workspace.update", ActorID: "root", CreatedAt: at},
		&model.AuditEvent{ID: "evt-2", WorkspaceID: &space, Action: "workspace.update", ActorID: "root", CreatedAt: at},
		&model.AuditEvent{ID: "evt-3", WorkspaceID: &other, Action: "workspace.update", ActorID: "root", CreatedAt: at},
		&model.AuditEvent{ID: "evt-4", Action: "instance.registry.verify", ActorID: "root", CreatedAt: at},
		&model.AuditEvent{ID: "evt-5", WorkspaceID: &space, Action: "instance.allocation.set", ActorID: "root", CreatedAt: at},
	}
	for _, row := range fixtures {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	service := audit.NewService(access)
	ctx := context.Background()
	root := authorization.Principal{UserID: "root", AuthVersion: 1}
	admin := authorization.Principal{UserID: "admin", AuthVersion: 1}
	member := authorization.Principal{UserID: "member", AuthVersion: 1}
	first, err := service.List(ctx, admin, space, 1, "")
	if err != nil || len(first.Events) != 1 || first.Events[0].ID != "evt-1" || first.NextCursor == "" {
		t.Fatalf("空间第一页 = %#v, %v", first, err)
	}
	next, err := service.List(ctx, admin, space, 1, first.NextCursor)
	if err != nil || len(next.Events) != 1 || next.Events[0].ID != "evt-2" || next.NextCursor != "" {
		t.Fatalf("空间末页 = %#v, %v", next, err)
	}
	global, err := service.List(ctx, root, "", 10, "")
	if err != nil || len(global.Events) != 5 {
		t.Fatalf("全局审计 = %#v, %v", global, err)
	}
	for _, forbidden := range []struct {
		principal authorization.Principal
		workspace string
	}{{admin, ""}, {admin, other}, {member, space}} {
		if _, err := service.List(ctx, forbidden.principal, forbidden.workspace, 10, ""); !errors.Is(err, authorization.ErrDenied) && !errors.Is(err, authorization.ErrNotFound) {
			t.Fatalf("审计越权，身份 %s，空间 %q，错误 %v", forbidden.principal.UserID, forbidden.workspace, err)
		}
	}
	if _, err := service.List(ctx, admin, space, 10, "evt-3"); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("跨空间游标错误 = %v", err)
	}
}
