package store

import (
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

// RegisterIDCallback 注册 Create 回调：主键为单列字符串 id 且值为空时，
// 自动按 security.NewID(前缀) 生成——数据库设计规范（每张表必有单列 id 主键）
// 的统一落地点，业务写入点不再散落 id 生成逻辑。
func RegisterIDCallback(db *gorm.DB) {
	db.Callback().Create().Before("gorm:before_create").Register("yummy:fill_id", fillID)
}

func fillID(tx *gorm.DB) {
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
	switch value.Kind() {
	case reflect.Struct:
		setID(tx, value, field, prefix)
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			setID(tx, value.Index(i), field, prefix)
		}
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
