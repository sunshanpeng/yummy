package audit

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
)

// TraceID 返回当前请求的 W3C trace_id，来自入口 OpenTelemetry 已解析/生成的
// span 上下文——响应头 X-Trace-Id 与日志 trace_id 字段同源；无 span 的上下文
// （内部任务等非 HTTP 入口）返回空串。
func TraceID(ctx context.Context) string {
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		return span.TraceID().String()
	}
	return ""
}

// Record 与业务写入使用同一事务，不接收任意请求正文或秘密元数据。
func Record(tx *gorm.DB, actorID, action, objectType, objectID string, workspaceID, applicationID *string) error {
	id, err := security.NewID("evt")
	if err != nil {
		return err
	}
	return tx.Create(&model.AuditEvent{
		ID: id, ActorID: actorID, Action: action, ObjectType: objectType, ObjectID: objectID,
		WorkspaceID: workspaceID, ApplicationID: applicationID, Result: "SUCCESS", TraceID: TraceID(tx.Statement.Context),
	}).Error
}
