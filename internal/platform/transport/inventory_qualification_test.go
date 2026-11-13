package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
	"go.uber.org/zap"
)

// 直连 handler 验证实例管理RPC映射与错误码；会话层由identity测试覆盖。
func TestInventoryNodeQualificationHandlers(t *testing.T) {
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
	box, err := security.NewSecretBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := &InventoryHandler{Service: inventory.NewService(db, access, box), Log: zap.NewNop()}
	root := authorization.Principal{UserID: "root", AuthVersion: 1}
	ctx := authorization.WithPrincipal(context.Background(), root)
	node, err := h.Service.CreateNode(ctx, root, inventory.NodeInput{Name: "执行节点", WorkerID: "worker-1", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := h.RegisterNodeQualification(ctx, connect.NewRequest(&platformv1.RegisterNodeQualificationRequest{NodeId: node.ID, WorkerId: "worker-1", ClusterNodeUid: "uid-a", QualificationId: "install-accept-1", BuildReady: true, MetricsReady: true, Evidence: map[string]string{"验收": "线下安装验收"}}))
	if err != nil {
		t.Fatal(err)
	}
	qualification := registered.Msg.GetQualification()
	if qualification == nil || qualification.Id == "" || qualification.NodeId != node.ID || qualification.WorkerId != "worker-1" || qualification.ClusterNodeUid != "uid-a" || qualification.Status != "ACTIVE" || !qualification.BuildReady || qualification.RuntimeReady || qualification.CreatedBy != "root" || qualification.CreatedAt == "" || qualification.RevokedAt != "" {
		t.Fatalf("登记响应=%#v", qualification)
	}
	revoked, err := h.RevokeNodeQualification(ctx, connect.NewRequest(&platformv1.RevokeNodeQualificationRequest{Id: qualification.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Msg.GetQualification().GetStatus() != "REVOKED" || revoked.Msg.GetQualification().GetRevokedAt() == "" {
		t.Fatalf("撤销响应=%#v", revoked.Msg.GetQualification())
	}
	if _, err := h.RegisterNodeQualification(ctx, connect.NewRequest(&platformv1.RegisterNodeQualificationRequest{NodeId: node.ID, WorkerId: "worker-1", ClusterNodeUid: "uid-a", QualificationId: "install-accept-2", BuildReady: true})); err != nil {
		t.Fatal(err)
	}
	listed, err := h.ListNodeQualifications(ctx, connect.NewRequest(&platformv1.ListNodeQualificationsRequest{Limit: 10}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetQualifications()) != 2 {
		t.Fatalf("列表=%#v", listed.Msg.GetQualifications())
	}
	// 非实例管理员：PermissionDenied；未认证：Unauthenticated。
	adminCtx := authorization.WithPrincipal(context.Background(), authorization.Principal{UserID: "admin", AuthVersion: 1})
	if _, err := h.RegisterNodeQualification(adminCtx, connect.NewRequest(&platformv1.RegisterNodeQualificationRequest{NodeId: node.ID, WorkerId: "worker-1", ClusterNodeUid: "uid-a", QualificationId: "install-accept-3", BuildReady: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("空间管理员登记=%v", err)
	}
	if _, err := h.ListNodeQualifications(context.Background(), connect.NewRequest(&platformv1.ListNodeQualificationsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("匿名列表=%v", err)
	}
	// 记录不存在：NotFound。
	if _, err := h.RevokeNodeQualification(ctx, connect.NewRequest(&platformv1.RevokeNodeQualificationRequest{Id: "nqf_missing"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("撤销缺失=%v", err)
	}
}
