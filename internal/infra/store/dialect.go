package store

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// NowPlusSeconds 返回以数据库时钟表达"NOW() + 秒数"的更新表达式。
// 【数据库专有】INTERVAL 语法两个方言不通用：PostgreSQL 写 INTERVAL '5 seconds'，
// MySQL 写 INTERVAL 5 SECOND，必须按 dialector 分支。刻意不用应用时钟传参替换：
// 调度租约的到期比较全部走数据库时钟（next_dispatch_at <= NOW()），换成宿主机
// 时钟会引入时钟偏差使租约不可领取或提前到期。
func NowPlusSeconds(db *gorm.DB, seconds int) clause.Expr {
	if db.Dialector.Name() == "mysql" {
		return gorm.Expr(fmt.Sprintf("NOW() + INTERVAL %d SECOND", seconds))
	}
	return gorm.Expr(fmt.Sprintf("NOW() + INTERVAL '%d seconds'", seconds))
}
