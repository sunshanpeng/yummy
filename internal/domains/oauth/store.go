package oauth

import "gorm.io/gorm"

// appendClientResources 为已存在的 CIMD 未验证发布者客户端追加资源登记候选。
// 原子条件更新：资源已包含时不重复追加。
// 【数据库专有】JSONB @> 包含与 || 数组拼接为 PostgreSQL 专有；MySQL 用
// JSON_CONTAINS / JSON_ARRAY_APPEND。收口在本文件，业务代码不出现表达式。
func appendClientResources(db *gorm.DB, id, resource string) error {
	return db.Model(&clientRecord{}).Where("id = ? AND active=TRUE", id).
		Update("resources", gorm.Expr("CASE WHEN resources @> ?::jsonb THEN resources ELSE resources || ?::jsonb END", jsonArray(resource), jsonArray(resource))).Error
}
