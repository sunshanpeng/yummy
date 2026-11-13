package audit

import (
	"context"

	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"gorm.io/gorm"
)

type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// Record 与业务写入使用同一事务，不接收任意请求正文或秘密元数据。
func Record(tx *gorm.DB, actorID, action, objectType, objectID string, workspaceID, applicationID *string) error {
	id, err := security.NewID("evt")
	if err != nil {
		return err
	}
	return tx.Create(&model.AuditEvent{
		ID: id, ActorID: actorID, Action: action, ObjectType: objectType, ObjectID: objectID,
		WorkspaceID: workspaceID, ApplicationID: applicationID, Result: "SUCCESS", RequestID: RequestID(tx.Statement.Context),
	}).Error
}
