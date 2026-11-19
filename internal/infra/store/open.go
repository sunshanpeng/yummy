// Package store 负责正式产品的 PostgreSQL 连接与持久化边界。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sunshanpeng/yummy/migrations"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 使用 pgx 的 database/sql 驱动；旧 demo 可暂时保留独立 pgxpool。
// 跨模块写操作必须共用同一个 GORM 事务，不能混入另一连接池的写入。
func Open(ctx context.Context, databaseURL string) (*gorm.DB, error) {
	if databaseURL == "" {
		return nil, errors.New("数据库配置不能为空")
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, errors.New("解析 PostgreSQL 连接配置失败")
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, errors.New("连接 PostgreSQL 失败")
	}
	if err := migrations.ApplySQL(ctx, sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		// SQL 参数可能含密码散列或令牌摘要，默认不写入 ORM 日志。
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("初始化数据库访问层: %w", err)
	}
	// 数据库设计规范：每张表必有单列 id 主键，Create 时按模型前缀自动填充。
	RegisterIDCallback(db)
	return db, nil
}

// Close 由装配层调用一次；业务仓库不拥有连接池生命周期。
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
