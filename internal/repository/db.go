// Package repository 是 MySQL / Redis / MinIO 读写。MySQL 经 GORM：连接、事务句柄统一 *gorm.DB，
// 但文档定版 SQL（row_ver 乐观锁、AS new ON DUPLICATE KEY UPDATE、LAST_INSERT_ID(...)、FOR UPDATE SKIP LOCKED）
// 一律 tx.Exec / tx.Raw 照抄执行，不改写成链式查询；建表只走 golang-migrate，禁止 AutoMigrate。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"agri-steward/internal/clock"
)

// DB 是 *gorm.DB 的类型别名：service / evalkit 用 *repository.DB 传递事务句柄（与 *gorm.DB 是同一类型），
// 从而不直接 import gorm（archtest：只有 repository、app、cmd 可 import gorm）。
type DB = gorm.DB

// ErrNotInTx 表示要求在调用方事务内执行的方法拿到了非事务句柄。
var ErrNotInTx = errors.New("repository: must run inside a transaction")

const slowQueryThreshold = 200 * time.Millisecond

// OpenOptions 是打开 MySQL 的参数。
type OpenOptions struct {
	DSN          string // 须带 parseTime=true&loc=UTC（config.MySQL.DSN 生成）
	MaxOpenConns int
	Clock        clock.Clock
	Logger       *slog.Logger
}

// OpenMySQL 打开 GORM 连接并 Ping：NowFunc 用注入的 Clock，日志接 slog（只记参数化 SQL，不落参数值）。
func OpenMySQL(ctx context.Context, o OpenOptions) (*gorm.DB, error) {
	if o.Clock == nil || o.Logger == nil {
		return nil, errors.New("repository: OpenMySQL requires Clock and Logger")
	}
	db, err := gorm.Open(mysql.Open(o.DSN), &gorm.Config{
		NowFunc:                o.Clock.Now,
		SkipDefaultTransaction: true,
		Logger: logger.NewSlogLogger(o.Logger, logger.Config{
			SlowThreshold:             slowQueryThreshold,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			ParameterizedQueries:      true,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("repository: open mysql: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("repository: mysql pool: %w", err)
	}
	if o.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(o.MaxOpenConns)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		closeErr := sqlDB.Close()
		return nil, errors.Join(fmt.Errorf("repository: ping mysql: %w", err), closeErr)
	}
	return db, nil
}

// Close 关闭底层连接池。
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("repository: mysql pool: %w", err)
	}
	return sqlDB.Close()
}

// execInTx 在事务连接上执行 SQL 并返回 sql.Result（需要 LastInsertId 时用）。
// 事务内 ConnPool 即 *sql.Tx，LAST_INSERT_ID() 与前一句落在同一连接上。
func execInTx(ctx context.Context, tx *gorm.DB, query string, args ...any) (sql.Result, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	return tx.Statement.ConnPool.ExecContext(ctx, query, args...)
}

// requireTx 校验 tx 是事务句柄。
func requireTx(tx *gorm.DB) error {
	if tx == nil {
		return ErrNotInTx
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return ErrNotInTx
	}
	return nil
}

// Transactor 在 DB 上开事务执行 fn（fn 返回错误即回滚），供 service 层定事务边界而不 import gorm。
type Transactor struct{ DB *gorm.DB }

// InTx 开事务执行 fn。
func (t Transactor) InTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if t.DB == nil {
		return errors.New("repository: transactor has no DB")
	}
	return t.DB.WithContext(ctx).Transaction(fn)
}

// nullIfEmpty 把空串写成 NULL。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
