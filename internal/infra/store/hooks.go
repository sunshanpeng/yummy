package store

import (
	"context"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/sunshanpeng/yummy/internal/lib/security"
)

// IDGenerator 是模型的可选接口：声明该表 id 主键的生成前缀。
// 未实现时使用默认前缀 "id"。
type IDGenerator interface {
	IDPrefix() string
}

type actorKey struct{}

// WithActor 把当前操作者（授权主体的用户标识）写入上下文；写入回调据此
// 填充 created_by / updated_by。授权内核 Do/DoInTx 统一注入，内部任务与
// 机器写入没有 actor，审计列保持空串。
func WithActor(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, actorKey{}, actorID)
}

// ActorFromContext 读取上下文中的操作者标识，无注入时返回空串。
func ActorFromContext(ctx context.Context) string {
	value, _ := ctx.Value(actorKey{}).(string)
	return value
}

// RegisterModelHooks 注册数据库设计规范的统一落地点：
//   - Create：主键 id 为空按 security.NewID(前缀) 生成；created_by / updated_by
//     为空且上下文有 actor 时填充；created_at / updated_at 由 GORM 按字段名
//     自动填充，无需回调。
//   - Update：updated_by 为空且上下文有 actor 时填充。
func RegisterModelHooks(db *gorm.DB) {
	db.Callback().Create().Before("gorm:before_create").Register("yummy:fill_model", fillOnCreate)
	db.Callback().Update().Before("gorm:before_update").Register("yummy:fill_updated_by", fillOnUpdate)
}

func fillOnCreate(tx *gorm.DB) {
	if tx.Statement == nil || tx.Statement.Schema == nil {
		return
	}
	field := tx.Statement.Schema.PrioritizedPrimaryField
	if field == nil || field.DBName != "id" || field.FieldType.Kind() != reflect.String {
		return
	}
	prefix := "id"
	value := reflect.ValueOf(tx.Statement.Dest)
	for value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	actor := ActorFromContext(tx.Statement.Context)
	switch value.Kind() {
	case reflect.Struct:
		applyModelDefaults(tx, value, field, prefix, actor)
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			applyModelDefaults(tx, value.Index(i), field, prefix, actor)
		}
	}
}

// applyModelDefaults 对单个模型实例填充 id 与 created_by/updated_by。
func applyModelDefaults(tx *gorm.DB, value reflect.Value, field *schema.Field, prefix, actor string) {
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if generator, ok := value.Interface().(IDGenerator); ok {
		prefix = generator.IDPrefix()
	}
	if _, zero := field.ValueOf(tx.Statement.Context, value); zero {
		id, err := security.NewID(prefix)
		if err != nil {
			_ = tx.AddError(err)
			return
		}
		_ = field.Set(tx.Statement.Context, value, id)
	}
	fillAuditField(tx, value, "created_by", actor)
	fillAuditField(tx, value, "updated_by", actor)
}

func fillAuditField(tx *gorm.DB, value reflect.Value, dbName, actor string) {
	if actor == "" {
		return
	}
	if field, found := tx.Statement.Schema.FieldsByDBName[dbName]; found && field.FieldType.Kind() == reflect.String {
		if _, zero := field.ValueOf(tx.Statement.Context, value); zero {
			_ = field.Set(tx.Statement.Context, value, actor)
		}
	}
}

// fillOnUpdate 更新路径填充 updated_by（updated_at 由 GORM 按字段名自动维护）。
// updated_by 的语义是“本次更新由谁执行”：无条件覆盖写入而不是仅在为空时填充，
// 否则行上会一直保留上一个操作者（如空间改名成功后 updated_by 仍是创建者）。
// Updates(map) 的列集合不在结构体反射面内，经 Statement.SetColumn 注入列；
// 调用方显式提供的 updated_by 同样被覆盖，本回调是更新写路径的统一裁决点。
func fillOnUpdate(tx *gorm.DB) {
	if tx.Statement == nil || tx.Statement.Schema == nil {
		return
	}
	actor := ActorFromContext(tx.Statement.Context)
	if actor == "" {
		return
	}
	if field, found := tx.Statement.Schema.FieldsByDBName["updated_by"]; found && field.FieldType.Kind() == reflect.String {
		tx.Statement.SetColumn("updated_by", actor, true)
	}
}

func setID(tx *gorm.DB, value reflect.Value, field *schema.Field, prefix string) {
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if generator, ok := value.Interface().(IDGenerator); ok {
		prefix = generator.IDPrefix()
	}
	if _, empty := field.ValueOf(tx.Statement.Context, value); !empty {
		return
	}
	id, err := security.NewID(prefix)
	if err != nil {
		_ = tx.AddError(err)
		return
	}
	_ = field.Set(tx.Statement.Context, value, id)
}
