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
	input := DefaultInput()
	input.Confirmed = true
	input.Variables = []VariableInput{{Name: "PUBLIC", Value: "hello"}, {Name: "PASSWORD", Value: "private-secret-value", Sensitive: true}}
	input.Files = []FileInput{{Path: "/app/config.json", Content: "{\"private\":true}", Sensitive: true}}
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
	var material Materialized
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
	input := DefaultInput()
	oneMiB := string(bytes.Repeat([]byte("x"), 1<<20))
	input.Files = []FileInput{{Path: "/app/a", Content: oneMiB, Sensitive: true}, {Path: "/app/b", Content: oneMiB, Sensitive: true}}
	if _, err := s.Save(context.Background(), p, "space", "app", input); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = 1
	input.Files = []FileInput{{Path: "/app/a", Sensitive: true, KeepExisting: true}, {Path: "/app/b", Sensitive: true, KeepExisting: true}, {Path: "/app/c", Content: "extra", Sensitive: true}}
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
			_, err := service.Save(context.Background(), p, "space", "app", DefaultInput())
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
	if _, err := s.Save(ctx, viewer, "space", "app", DefaultInput()); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("查看者可写配置")
	}
	if _, err := s.Get(ctx, p, "other-space", "app"); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("跨空间可读配置")
	}
	if _, err := s.Save(ctx, p, "space", "app", DefaultInput()); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := s.MaterializeForDeployment(tx, "space", "app", 1); return err }); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatal("未确认配置可部署")
	}
}
func TestValidateRejectsUnsafePathsAndConflicts(t *testing.T) {
	cases := []func(*Input){
		func(i *Input) { i.Port = 0 }, func(i *Input) { i.Probe.Path = "https://evil.test/health" }, func(i *Input) { i.Domains = []string{"127.0.0.1"} }, func(i *Input) { i.Domains = []string{"*.example.com"} },
		func(i *Input) { i.Domains = []string{"１２７．０．０．１"} },
		func(i *Input) {
			i.Files = []FileInput{{Path: "/app/data"}, {Path: "/app/data-foo"}, {Path: "/app/data/x"}}
		},
		func(i *Input) { i.Files = []FileInput{{Path: "/app/../etc/passwd"}} }, func(i *Input) { i.Files = []FileInput{{Path: "/proc/self/environ"}} },
		func(i *Input) {
			i.Files = []FileInput{{Path: "/app/data/config"}}
			i.Volumes = []Volume{{Name: "data", MountPath: "/app/data", SizeGiB: 1}}
		},
		func(i *Input) {
			i.Variables = []VariableInput{{Name: "SECRET", Sensitive: true, KeepExisting: true, Value: "mask"}}
		},
		func(i *Input) { i.Type = TypeWorker }, func(i *Input) { i.Resources.CPULimitMilli = 1 },
	}
	for index, mutate := range cases {
		input := DefaultInput()
		mutate(&input)
		if _, err := Validate(input); err == nil {
			t.Fatalf("危险配置%d未拒绝", index)
		}
	}
	input := DefaultInput()
	input.Type = TypeWorker
	input.Port = 0
	input.Probe = Probe{}
	if _, err := Validate(input); err != nil {
		t.Fatal(err)
	}
	input = DefaultInput()
	input.Domains = []string{"WWW.Example.COM."}
	got, err := Validate(input)
	if err != nil || got.Domains[0] != "www.example.com" {
		t.Fatal("域名未规范化")
	}
}
