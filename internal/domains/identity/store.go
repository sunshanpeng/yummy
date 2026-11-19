package identity

import "gorm.io/gorm"

// identityAdvisoryLockKey 是本域事务锁的固定键（"yummyadm" 的 ASCII 编码）。
// 【数据库专有】pg_advisory_xact_lock 是 PostgreSQL 专有能力：同一键的事务级
// 咨询锁在事务结束时自动释放。MySQL 没有对应机制，切换时以
// SELECT ... FOR UPDATE 锁一张单行锚点表（如 instance_settings）替代，
// 语义等价（同键事务串行）。调用方必须持有进行中的事务。
func lockAdminMutation(tx *gorm.DB) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(0x79756d6d7961646d)).Error
}
