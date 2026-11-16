package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"gorm.io/gorm"
)

type fixture struct {
	db      *gorm.DB
	service *Service
	access  *authorization.Access
	admin   authorization.Principal
	viewer  authorization.Principal
}

func setup(t *testing.T) fixture {
	t.Helper()
	db := testkit.OpenDatabase(t)
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, access)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.InitializeAdmin(context.Background(), "root-admin", "a-secure-initial-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{
		&model.User{ID: "viewer", Username: "viewer", PasswordHash: "unused", Status: model.UserActive, AuthVersion: 1},
		&model.Workspace{ID: "space", Name: "测试空间", Slug: "space", Status: model.ResourceActive, Revision: 1},
		&model.WorkspaceMember{WorkspaceID: "space", UserID: admin.ID, Role: model.WorkspaceAdmin},
		&model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: model.WorkspaceMemberRole},
		&model.Application{ID: "app", WorkspaceID: "space", Name: "gateway", Status: model.ResourceActive, Revision: 1},
		&model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app", UserID: "viewer", Role: model.ApplicationViewer},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	return fixture{db: db, service: service, access: access, admin: authorization.Principal{UserID: admin.ID, AuthVersion: 1, InstanceAdmin: true}, viewer: authorization.Principal{UserID: "viewer", AuthVersion: 1}}
}

func TestInitializeAndAuthenticate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.service.InitializeAdmin(ctx, "other-admin", "another-secure-password"); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("重复初始化: %v", err)
	}
	user, p, err := f.service.Authenticate(ctx, "ROOT-ADMIN", "a-secure-initial-password")
	if err != nil || user.ID != f.admin.UserID || !p.InstanceAdmin {
		t.Fatalf("登录失败: %#v %v", user, err)
	}
	if _, _, err := f.service.Authenticate(ctx, "root-admin", "wrong-password"); !errors.Is(err, ErrCredentials) {
		t.Fatal(err)
	}
	if _, _, err := f.service.Authenticate(ctx, "unknown", "wrong-password"); !errors.Is(err, ErrCredentials) {
		t.Fatal(err)
	}
	if err := f.service.SetUserStatus(ctx, f.admin, f.admin.UserID, model.UserDisabled); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("最后全局管理员被停用: %v", err)
	}
}

func TestTokenIntersectionRevocationAndOwner(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.service.CreateToken(ctx, f.viewer, CreateTokenInput{Name: "too-powerful", Scopes: []string{"build:write"}, ApplicationIDs: []string{"app"}}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("查看者扩大scope: %v", err)
	}
	if _, err := f.service.CreateToken(ctx, f.admin, CreateTokenInput{Name: "wildcard", Scopes: []string{"build:read"}}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("空应用范围: %v", err)
	}
	created, err := f.service.CreateToken(ctx, f.viewer, CreateTokenInput{Name: "read-only", Scopes: []string{"build:read"}, ApplicationIDs: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.service.AuthenticatePAT(ctx, created.Plaintext, "https://example.test/api")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowsScope("app", authorization.BuildRead) || p.AllowsScope("app", authorization.BuildWrite) {
		t.Fatal("scope交集错误")
	}
	if err := f.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		_, _, err := f.access.Application(tx, current, "space", "app", authorization.BuildRead)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RevokeToken(ctx, f.admin, created.Token.ID); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("全局管理员能撤销他人凭证: %v", err)
	}
	if err := f.db.Where("application_id = ? AND user_id = ?", "app", f.viewer.UserID).Delete(&model.ApplicationMember{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		_, _, err := f.access.Application(tx, current, "space", "app", authorization.BuildRead)
		return err
	}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("撤权后仍可读: %v", err)
	}
	if err := f.service.RevokeToken(ctx, f.viewer, created.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AuthenticatePAT(ctx, created.Plaintext, "https://example.test/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("撤销凭证仍可用: %v", err)
	}
}

func TestUserDisableAndPasswordChangeInvalidateCredentials(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	created, err := f.service.CreateToken(ctx, f.viewer, CreateTokenInput{Name: "read-only", Scopes: []string{"build:read"}, ApplicationIDs: []string{"app"}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.SetUserStatus(ctx, f.admin, f.viewer.UserID, model.UserDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Current(ctx, f.viewer); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("停用会话未失效: %v", err)
	}
	if _, err := f.service.AuthenticatePAT(ctx, created.Plaintext, "https://example.test/api"); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("停用令牌未失效: %v", err)
	}
	if err := f.service.ChangePassword(ctx, f.admin, "a-secure-initial-password", "the-new-secure-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Current(ctx, f.admin); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("旧身份版本未失效: %v", err)
	}
	if _, _, err := f.service.Authenticate(ctx, "root-admin", "the-new-secure-password"); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationIsSingleUseAndDoesNotTakeOverExistingAccount(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	created, err := f.service.CreateInvitation(ctx, f.admin, "space", "new-person", model.WorkspaceMemberRole)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.service.AcceptInvitation(ctx, created.Plaintext, "new-person-secure-password")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("邀请消费成功%d次", winners)
	}
	var count int64
	if err := f.db.Model(&model.User{}).Where("username = ?", "new-person").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("用户数量%d %v", count, err)
	}
	other := model.Workspace{ID: "space-other", Name: "其他空间", Slug: "other"}
	if err := f.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	invite, err := f.service.CreateInvitation(ctx, f.admin, other.ID, "root-admin", model.WorkspaceAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AcceptInvitation(ctx, invite.Plaintext, "wrong-password"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("邀请可接管已有账号: %v", err)
	}
	if _, err := f.service.AcceptInvitation(ctx, invite.Plaintext, "a-secure-initial-password"); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordResetSingleUseAndImmediateInvalidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	plain, err := f.service.CreatePasswordReset(ctx, f.admin, f.admin.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Current(ctx, f.admin); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("发起重设未即时失效: %v", err)
	}
	if err := f.service.CompletePasswordReset(ctx, plain, "reset-secure-password"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.CompletePasswordReset(ctx, plain, "another-secure-password"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("重设重复消费: %v", err)
	}
	if _, _, err := f.service.Authenticate(ctx, "root-admin", "reset-secure-password"); err != nil {
		t.Fatal(err)
	}
}
