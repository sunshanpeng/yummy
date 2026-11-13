package buildsecret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"gorm.io/gorm"
)

type environment struct {
	db                                  *gorm.DB
	service                             *Service
	root, maintainer, developer, viewer authorization.Principal
}

func setup(t *testing.T) environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	values := []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}, &model.User{ID: "maintainer", Username: "maintainer", PasswordHash: "hash"}, &model.User{ID: "developer", Username: "developer", PasswordHash: "hash"}, &model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash"}, &model.Workspace{ID: "space", Name: "空间", Slug: "space"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "root", Role: "admin"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "maintainer", Role: "member"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "developer", Role: "member"}, &model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: "member"}, &model.Application{ID: "app-a", WorkspaceID: "space", Name: "one"}, &model.Application{ID: "app-b", WorkspaceID: "space", Name: "two"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app-a", UserID: "maintainer", Role: "maintainer"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app-a", UserID: "developer", Role: "developer"}, &model.ApplicationMember{WorkspaceID: "space", ApplicationID: "app-a", UserID: "viewer", Role: "viewer"}}
	for _, value := range values {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return environment{db: db, service: NewService(db, access, box), root: authorization.Principal{UserID: "root", AuthVersion: 1}, maintainer: authorization.Principal{UserID: "maintainer", AuthVersion: 1}, developer: authorization.Principal{UserID: "developer", AuthVersion: 1}, viewer: authorization.Principal{UserID: "viewer", AuthVersion: 1}}
}
func TestMetadataPermissionsValueBoundsAndEncryption(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	for _, principal := range []authorization.Principal{env.developer, env.viewer} {
		if _, err := env.service.Create(ctx, principal, "space", "app-a", CreateInput{Name: "TOKEN", Value: "secret"}); !errors.Is(err, authorization.ErrNotFound) {
			t.Fatalf("非maintainer管理=%v", err)
		}
	}
	for _, input := range []CreateInput{{Name: "bad/name", Value: "secret"}, {Name: "TOKEN", Value: ""}, {Name: "TOKEN", Value: strings.Repeat("x", MaxValueBytes+1)}} {
		if _, err := env.service.Create(ctx, env.root, "space", "app-a", input); !errors.Is(err, authorization.ErrInvalid) {
			t.Fatalf("非法输入=%v", err)
		}
	}
	value, err := env.service.Create(ctx, env.maintainer, "space", "app-a", CreateInput{Name: "NPM_TOKEN", Purpose: "私有依赖", Value: "never-print-me"})
	if err != nil {
		t.Fatal(err)
	}
	if value.CurrentVersion != 1 || !value.Configured {
		t.Fatal(value)
	}
	if _, err := env.service.Create(ctx, env.root, "space", "app-a", CreateInput{Name: "NPM_TOKEN", Value: "duplicate"}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("同应用重名=%v", err)
	}
	if _, err := env.service.Create(ctx, env.root, "space", "app-b", CreateInput{Name: "NPM_TOKEN", Value: "different-app"}); err != nil {
		t.Fatal(err)
	}
	page, err := env.service.List(ctx, env.maintainer, "space", "app-a", 20, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("元数据列表=%#v,%v", page, err)
	}
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), "never-print-me") || strings.Contains(string(data), "encrypted") {
		t.Fatal("元数据泄漏秘密")
	}
	var encrypted versionRecord
	if err := env.db.First(&encrypted, "secret_id = ? AND version = ?", value.ID, 1).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.EncryptedValue, []byte("never-print-me")) {
		t.Fatal("数据库明文")
	}
	var events []model.AuditEvent
	if err := env.db.Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(events)
	if strings.Contains(string(data), "never-print-me") {
		t.Fatal("审计明文")
	}
	material, err := env.service.ReadVersion(env.db, "space", "app-a", Ref{SecretID: value.ID, Version: 1})
	if err != nil || string(material.Value) != "never-print-me" {
		t.Fatal(err)
	}
	material.Clear()
	if material.Value != nil {
		t.Fatal("秘密未清理")
	}
	if _, err := env.service.Get(ctx, env.viewer, "space", "app-a", value.ID); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("viewer读管理元数据=%v", err)
	}
}
func TestResolveIsApplicationBoundAndOldVersionSurvivesRotationAndDisable(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	one, err := env.service.Create(ctx, env.root, "space", "app-a", CreateInput{Name: "TOKEN", Value: "version-one"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.service.Create(ctx, env.root, "space", "app-b", CreateInput{Name: "TOKEN", Value: "other-secret"})
	if err != nil {
		t.Fatal(err)
	}
	var frozen []Ref
	if err := env.db.Transaction(func(tx *gorm.DB) error {
		var err error
		frozen, err = env.service.Resolve(tx, "space", "app-a", []Ref{{SecretID: one.ID}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 1 || frozen[0].Version != 1 || frozen[0].Name != "TOKEN" {
		t.Fatal(frozen)
	}
	if _, err := env.service.Resolve(env.db, "space", "app-a", []Ref{{SecretID: other.ID, Version: 1}}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("跨应用引用=%v", err)
	}
	if _, err := env.service.Resolve(env.db, "space", "app-a", []Ref{{SecretID: one.ID, Name: "evil"}}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("伪造挂载名=%v", err)
	}
	if _, err := env.service.Replace(ctx, env.root, "space", "app-a", one.ID, ReplaceInput{ExpectedVersion: 1, Value: "version-two"}); err != nil {
		t.Fatal(err)
	}
	material, err := env.service.ReadVersion(env.db, "space", "app-a", frozen[0])
	if err != nil || string(material.Value) != "version-one" {
		t.Fatalf("旧输入变化=%q,%v", material.Value, err)
	}
	material.Clear()
	if _, err := env.service.Replace(ctx, env.root, "space", "app-a", one.ID, ReplaceInput{ExpectedVersion: 1, Value: "stale"}); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("旧CAS=%v", err)
	}
	if err := env.service.Deactivate(ctx, env.root, "space", "app-a", one.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.Resolve(env.db, "space", "app-a", []Ref{{SecretID: one.ID, Version: 1}}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("禁用密钥新构建引用=%v", err)
	}
	material, err = env.service.ReadVersion(env.db, "space", "app-a", frozen[0])
	if err != nil || string(material.Value) != "version-one" {
		t.Fatalf("禁用抹除已冻结旧任务=%v", err)
	}
	material.Clear()
	if _, err := env.service.ReadVersion(env.db, "space", "app-b", frozen[0]); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("跨应用解密=%v", err)
	}
	if err := env.db.Where("id = ?", one.ID).Delete(&secretRecord{}).Error; err == nil {
		t.Fatal("删除密钥未被历史版本外键保护")
	}
	if err := env.db.Where("secret_id = ? AND version = ?", one.ID, 2).Delete(&versionRecord{}).Error; err == nil {
		t.Fatal("删除当前版本未被外键保护")
	}
}
func TestApplicationScopedPATNeverGainsSecretManagement(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	token := model.AccessToken{ID: "token", UserID: "root", Name: "限定凭证", TokenHash: "digest", AuthVersion: 1, Scopes: []string{"application:read", "config:write"}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := env.db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if err := env.db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: "space", ApplicationID: "app-a"}).Error; err != nil {
		t.Fatal(err)
	}
	bearer := authorization.Principal{UserID: "root", AuthVersion: 1, TokenID: token.ID}
	if _, err := env.service.Create(ctx, bearer, "space", "app-a", CreateInput{Name: "TOKEN", Value: "secret"}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT管理密钥=%v", err)
	}
	if _, err := env.service.List(ctx, bearer, "space", "app-a", 20, ""); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("PAT列出密钥管理=%v", err)
	}
}

func TestConcurrentReplacementHasSingleCASWinner(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	value, err := env.service.Create(ctx, env.root, "space", "app-a", CreateInput{Name: "TOKEN", Value: "one"})
	if err != nil {
		t.Fatal(err)
	}
	access, err := authorization.NewAccess(env.db)
	if err != nil {
		t.Fatal(err)
	}
	other := NewService(env.db, access, env.service.box)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, service := range []*Service{env.service, other} {
		workers.Add(1)
		go func(service *Service) {
			defer workers.Done()
			<-start
			_, err := service.Replace(ctx, env.root, "space", "app-a", value.ID, ReplaceInput{ExpectedVersion: 1, Value: "replacement"})
			results <- err
		}(service)
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
		t.Fatalf("成功替换次数=%d", success)
	}
	var count int64
	env.db.Model(&versionRecord{}).Where("secret_id = ?", value.ID).Count(&count)
	if count != 2 {
		t.Fatalf("版本数=%d", count)
	}
}
func TestAuditFailureRollsBackReplacementAndVersion(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	value, err := env.service.Create(ctx, env.root, "space", "app-a", CreateInput{Name: "TOKEN", Value: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.Exec(`ALTER TABLE audit_events ADD CONSTRAINT test_reject_secret_replace CHECK (action <> 'application.build-secret.replace')`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := env.service.Replace(ctx, env.root, "space", "app-a", value.ID, ReplaceInput{ExpectedVersion: 1, Value: "uncommitted"}); err == nil {
		t.Fatal("审计失败未回滚")
	}
	current, err := env.service.Get(ctx, env.root, "space", "app-a", value.ID)
	if err != nil || current.CurrentVersion != 1 {
		t.Fatalf("当前版本=%#v,%v", current, err)
	}
	var count int64
	if err := env.db.Model(&versionRecord{}).Where("secret_id = ?", value.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("不应保留孤儿版本=%d,%v", count, err)
	}
}
