package store

import (
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/infra/store/model"
)

// TestIDCallbackFillsEmptyPrimaryID 验证数据库设计规范的落地点：
// Create 时主键 id 为空由回调自动填充，且按业务键可查询命中。
func TestIDCallbackFillsEmptyPrimaryID(t *testing.T) {
	db := openIsolatedStore(t)
	workspace := model.Workspace{ID: "wsp-x", Name: "回调验证", Slug: "callback-verify", Status: "ACTIVE", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	user := model.User{ID: "usr-x", Username: "callback-verify", PasswordHash: "x", Status: model.UserActive, AuthVersion: 1}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	row := model.IdempotencyKey{WorkspaceID: "wsp-x", ActorID: "usr-x", Action: "runtime.config.save:app-x", Key: "config-first", Fingerprint: "fp", ResultID: "rev-1", CreatedAt: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.ID == "" {
		t.Fatal("Create 回调未填充 id 主键")
	}
	var found model.IdempotencyKey
	if err := db.First(&found, "workspace_id = ? AND actor_id = ? AND action = ? AND key = ?", row.WorkspaceID, row.ActorID, row.Action, row.Key).Error; err != nil {
		t.Fatalf("按业务键查询未命中: %v", err)
	}
	if found.ID != row.ID {
		t.Fatalf("业务键命中行与写入 id 不一致: %q vs %q", found.ID, row.ID)
	}
	// 唯一约束保留：同业务键重复插入必须被拒绝（id 不同、业务键相同）。
	dup := model.IdempotencyKey{WorkspaceID: "ws-x", ActorID: "usr-x", Action: "runtime.config.save:app-x", Key: "config-first", Fingerprint: "fp2", ResultID: "rev-1", CreatedAt: time.Now().UTC()}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("同业务键重复插入未被业务键 UNIQUE 拒绝")
	}
}
