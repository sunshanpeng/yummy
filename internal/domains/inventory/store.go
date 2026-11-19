package inventory

import "gorm.io/gorm"

// instanceAdvisoryLockKey 是实例记录初始化的固定锁键（"yummyinv" 的 ASCII 编码）。
// 【数据库专有】pg_advisory_xact_lock 为 PostgreSQL 专有能力：并发初始化共享同一
// 事务级咨询锁，先到者建行、后到者读行。MySQL 切换时以 SELECT ... FOR UPDATE 锁
// 单行锚点表替代；调用方必须持有进行中的事务。
func lockInstance(tx *gorm.DB) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(0x79756d6d79696e76)).Error
}
