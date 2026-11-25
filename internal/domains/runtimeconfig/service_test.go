package runtimeconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"github.com/sunshanpeng/yummy/internal/lib/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
)

func setup(t *testing.T) (*Service, *gorm.DB, authorization.Principal) {
	t.Helper()
	db := testkit.OpenDatabase(t)
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{&model.User{ID: "admin", Username: "admin", PasswordHash: "hash", InstanceAdmin: true, AuthVersion: 1}, &model.Workspace{ID: "space", Name: "空间", Slug: "space"}, &model.Application{ID: "app", WorkspaceID: "space", Name: "gateway"}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	return NewService(db, access, box), db, authorization.Principal{UserID: "admin", AuthVersion: 1, InstanceAdmin: true}
}
func TestSuggestedConfigAndImmutableSecretVersions(t *testing.T) {
	s, db, p := setup(t)
	ctx := context.Background()
	suggested, err := s.Get(ctx, p, "space", "app")
	if err != nil || suggested.Revision != 0 || suggested.Confirmed {
		t.Fatalf("建议配置错误:%#v %v", suggested, err)
	}
	input := runtimeconfig.DefaultInput()
	input.Confirmed = true
	input.Variables = []runtimeconfig.VariableInput{{Name: "PUBLIC", Value: "hello"}, {Name: "PASSWORD", Value: "private-secret-value", Sensitive: true}}
	input.Files = []runtimeconfig.FileInput{{Path: "/app/config.json", Content: "{\"private\":true}", Sensitive: true}}
	first, err := s.Save(ctx, p, "space", "app", input)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(first)
	if bytes.Contains(public, []byte("private-secret-value")) || bytes.Contains(public, []byte("private\\\"")) {
		t.Fatalf("DTO泄露秘密:%s", public)
	}
	var raw record
	if err := db.First(&raw, "application_id = ? AND revision = ?", "app", 1).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw.Spec, []byte("private-secret-value")) {
		t.Fatal("数据库存明文秘密")
	}
	input.ExpectedRevision = 1
	input.Variables[0].Value = "updated"
	input.Variables[1].Value = ""
	input.Variables[1].KeepExisting = true
	input.Files[0].Content = ""
	input.Files[0].KeepExisting = true
	second, err := s.Save(ctx, p, "space", "app", input)
	if err != nil || second.Revision != 2 {
		t.Fatalf("第二版错误:%v", err)
	}
	old, err := s.GetRevision(ctx, p, "space", "app", 1)
	if err != nil || old.Variables[0].Value != "hello" {
		t.Fatal("保存覆盖历史版本")
	}
	var material runtimeconfig.Materialized
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		material, err = s.MaterializeForDeployment(tx, "space", "app", 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if material.Variables["PASSWORD"] != "private-secret-value" || material.Files["/app/config.json"] != "{\"private\":true}" {
		t.Fatal("保留秘密未绑定版本重新加密")
	}
	if _, err := s.Save(ctx, p, "space", "app", input); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("旧revision未冲突:%v", err)
	}
}
func TestKeepExistingCannotAccumulateBeyondFinalSizeLimit(t *testing.T) {
	s, _, p := setup(t)
	input := runtimeconfig.DefaultInput()
	oneMiB := string(bytes.Repeat([]byte("x"), 1<<20))
	input.Files = []runtimeconfig.FileInput{{Path: "/app/a", Content: oneMiB, Sensitive: true}, {Path: "/app/b", Content: oneMiB, Sensitive: true}}
	if _, err := s.Save(context.Background(), p, "space", "app", input); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = 1
	input.Files = []runtimeconfig.FileInput{{Path: "/app/a", Sensitive: true, KeepExisting: true}, {Path: "/app/b", Sensitive: true, KeepExisting: true}, {Path: "/app/c", Content: "extra", Sensitive: true}}
	if _, err := s.Save(context.Background(), p, "space", "app", input); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("保留旧值绕过总量: %v", err)
	}
}

func TestConcurrentSaveUsesApplicationLock(t *testing.T) {
	s, db, p := setup(t)
	otherAccess, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	other := NewService(db, otherAccess, s.box)
	var wg sync.WaitGroup
	result := make(chan error, 2)
	for _, service := range []*Service{s, other} {
		wg.Add(1)
		go func(service *Service) {
			defer wg.Done()
			_, err := service.Save(context.Background(), p, "space", "app", runtimeconfig.DefaultInput())
			result <- err
		}(service)
	}
	wg.Wait()
	close(result)
	winners := 0
	for err := range result {
		if err == nil {
			winners++
		} else if !errors.Is(err, authorization.ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("并发保存成功%d次", winners)
	}
}
func TestConfigPermissionsAndUnconfirmedDeployment(t *testing.T) {
	s, db, p := setup(t)
	ctx := context.Background()
	for _, v := range []any{&model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash", AuthVersion: 1}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: model.WorkspaceMemberRole}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app", UserID: "viewer", Role: model.ApplicationViewer}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	viewer := authorization.Principal{UserID: "viewer", AuthVersion: 1}
	if _, err := s.Get(ctx, viewer, "space", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, viewer, "space", "app", runtimeconfig.DefaultInput()); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("查看者可写配置")
	}
	if _, err := s.Get(ctx, p, "other-space", "app"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("跨空间可读配置")
	}
	if _, err := s.Save(ctx, p, "space", "app", runtimeconfig.DefaultInput()); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := s.MaterializeForDeployment(tx, "space", "app", 1); return err }); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal("未确认配置可部署")
	}
}
func TestValidateRejectsUnsafePathsAndConflicts(t *testing.T) {
	cases := []func(*runtimeconfig.Input){
		func(i *runtimeconfig.Input) { i.Port = 0 }, func(i *runtimeconfig.Input) { i.Probe.Path = "https://evil.test/health" }, func(i *runtimeconfig.Input) { i.Domains = []string{"127.0.0.1"} }, func(i *runtimeconfig.Input) { i.Domains = []string{"*.example.com"} },
		func(i *runtimeconfig.Input) { i.Domains = []string{"１２７．０．０．１"} },
		func(i *runtimeconfig.Input) {
			i.Files = []runtimeconfig.FileInput{{Path: "/app/data"}, {Path: "/app/data-foo"}, {Path: "/app/data/x"}}
		},
		func(i *runtimeconfig.Input) { i.Files = []runtimeconfig.FileInput{{Path: "/app/../etc/passwd"}} }, func(i *runtimeconfig.Input) { i.Files = []runtimeconfig.FileInput{{Path: "/proc/self/environ"}} },
		func(i *runtimeconfig.Input) {
			i.Files = []runtimeconfig.FileInput{{Path: "/app/data/config"}}
			i.Volumes = []runtimeconfig.Volume{{Name: "data", MountPath: "/app/data", SizeGiB: 1}}
		},
		func(i *runtimeconfig.Input) {
			i.Variables = []runtimeconfig.VariableInput{{Name: "SECRET", Sensitive: true, KeepExisting: true, Value: "mask"}}
		},
		func(i *runtimeconfig.Input) { i.Type = runtimeconfig.TypeWorker }, func(i *runtimeconfig.Input) { i.Resources.CPULimitMilli = 1 },
	}
	for index, mutate := range cases {
		input := runtimeconfig.DefaultInput()
		mutate(&input)
		if _, err := runtimeconfig.Validate(input); err == nil {
			t.Fatalf("危险配置%d未拒绝", index)
		}
	}
	input := runtimeconfig.DefaultInput()
	input.Type = runtimeconfig.TypeWorker
	input.Port = 0
	input.Probe = runtimeconfig.Probe{}
	if _, err := runtimeconfig.Validate(input); err != nil {
		t.Fatal(err)
	}
	input = runtimeconfig.DefaultInput()
	input.Domains = []string{"WWW.Example.COM."}
	got, err := runtimeconfig.Validate(input)
	if err != nil || got.Domains[0] != "www.example.com" {
		t.Fatal("域名未规范化")
	}
}

// TestLatestRevisionFacts 验证跨域版本事实的语义：空应用返回零值、最新版本含
// 未确认草稿、确认状态如实返回——部署域期望校验与构建域冻结校验共用本方法。
func TestLatestRevisionFacts(t *testing.T) {
	s, _, p := setup(t)
	ctx := context.Background()
	revision, confirmed, err := s.LatestRevision(s.db.WithContext(ctx), "space", "app")
	if err != nil || revision != 0 || confirmed {
		t.Fatalf("空应用语义错误: %d %t %v", revision, confirmed, err)
	}
	input := runtimeconfig.DefaultInput()
	input.Confirmed = true
	if _, err := s.Save(ctx, p, "space", "app", input); err != nil {
		t.Fatal(err)
	}
	revision, confirmed, err = s.LatestRevision(s.db.WithContext(ctx), "space", "app")
	if err != nil || revision != 1 || !confirmed {
		t.Fatalf("已确认版本事实错误: %d %t %v", revision, confirmed, err)
	}
	input.ExpectedRevision = 1
	input.Confirmed = false
	if _, err := s.Save(ctx, p, "space", "app", input); err != nil {
		t.Fatal(err)
	}
	revision, confirmed, err = s.LatestRevision(s.db.WithContext(ctx), "space", "app")
	if err != nil || revision != 2 || confirmed {
		t.Fatalf("草稿版本事实错误: %d %t %v", revision, confirmed, err)
	}
}

func TestRestoreRevisionKeepsDraftHistoryAndRestoresSensitiveValues(t *testing.T) {
	s, db, p := setup(t)
	ctx := context.Background()
	base := runtimeconfig.DefaultInput()
	base.Confirmed = true
	base.Variables = []runtimeconfig.VariableInput{{Name: "TOKEN", Sensitive: true, Value: "original-secret"}}
	if _, err := s.Save(ctx, p, "space", "app", base); err != nil {
		t.Fatal(err)
	}
	draft := base
	draft.ExpectedRevision = 1
	draft.Variables = []runtimeconfig.VariableInput{{Name: "TOKEN", Sensitive: true, Value: "draft-secret"}}
	if _, err := s.Save(ctx, p, "space", "app", draft); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreRevision(ctx, p, "space", "app", 1, 3, "stale"); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("过期草稿应被拒绝：%v", err)
	}
	restored, err := s.RestoreRevision(ctx, p, "space", "app", 1, 2, "restore")
	if err != nil || restored.Revision != 3 || len(restored.Variables) != 1 || restored.Variables[0].Value != "" {
		t.Fatalf("恢复后的公开配置 = %#v, %v", restored, err)
	}
	replayed, err := s.RestoreRevision(ctx, p, "space", "app", 1, 2, "restore")
	if err != nil || replayed.Revision != 3 {
		t.Fatalf("重试生成了不同配置：%#v, %v", replayed, err)
	}
	changed, err := s.Compare(ctx, p, "space", "app", 1, 2)
	if err != nil || len(changed) != 1 || changed[0] != (Change{Field: "variable", Name: "TOKEN", Operation: "UPDATED"}) {
		t.Fatalf("敏感配置差异只应显示名称和变更类型：%#v, %v", changed, err)
	}
	unchanged, err := s.Compare(ctx, p, "space", "app", 1, 3)
	if err != nil || len(unchanged) != 0 {
		t.Fatalf("重新加密的同一密钥不应标为有变更：%#v, %v", unchanged, err)
	}
	for revision, secret := range map[int64]string{1: "original-secret", 2: "draft-secret", 3: "original-secret"} {
		value, err := s.MaterializeForDeployment(db, "space", "app", revision)
		if err != nil || value.Variables["TOKEN"] != secret {
			t.Fatalf("配置版本 %d 的密钥与预期不一致：%v", revision, err)
		}
	}
}
