package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	// 迁移与建库直接用 database/sql + go-sql-driver（golang-migrate 的 mysql 驱动只接受 *sql.DB）
	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/minio/minio-go/v7"
)

// dbNamePattern 限定库名，避免拼接 CREATE DATABASE 时注入。
var dbNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ErrBadDBName 表示库名不合法。
var ErrBadDBName = errors.New("repository: database name must match ^[a-z][a-z0-9_]{0,63}$")

// CreateDatabase 在 serverDSN（不带库名）所指实例上建库（已存在则跳过）。
func CreateDatabase(ctx context.Context, serverDSN, name string) error {
	if !dbNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrBadDBName, name)
	}
	db, err := sql.Open("mysql", serverDSN)
	if err != nil {
		return fmt.Errorf("repository: open mysql: %w", err)
	}
	_, execErr := db.ExecContext(ctx,
		"CREATE DATABASE IF NOT EXISTS `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	if err := errors.Join(execErr, db.Close()); err != nil {
		return fmt.Errorf("repository: create database %s: %w", name, err)
	}
	return nil
}

// Migrate 用 golang-migrate 把 dir 下的 *.up.sql 执行到 dsn（须带 multiStatements=true）所指库；
// 已是最新版本不报错。返回迁移后的版本号。
func Migrate(dsn, dir string) (uint, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return 0, fmt.Errorf("repository: migrations dir %s: %w", dir, err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, fmt.Errorf("repository: open mysql: %w", err)
	}
	driver, err := migratemysql.WithInstance(db, &migratemysql.Config{})
	if err != nil {
		return 0, errors.Join(fmt.Errorf("repository: migrate driver: %w", err), db.Close())
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+abs, "mysql", driver)
	if err != nil {
		return 0, errors.Join(fmt.Errorf("repository: migrate init %s: %w", dir, err), driver.Close())
	}
	upErr := m.Up()
	if errors.Is(upErr, migrate.ErrNoChange) {
		upErr = nil
	}
	version, _, verErr := m.Version()
	srcErr, dbErr := m.Close()
	if err := errors.Join(upErr, verErr, srcErr, dbErr); err != nil {
		return 0, fmt.Errorf("repository: migrate %s: %w", dir, err)
	}
	return version, nil
}

// EnsureBuckets 创建缺失的 bucket；MinIO 不可达时返回明确错误。
func EnsureBuckets(ctx context.Context, c *minio.Client, buckets ...string) error {
	for _, b := range buckets {
		ok, err := c.BucketExists(ctx, b)
		if err != nil {
			return fmt.Errorf("repository: minio %s: check bucket %s: %w", c.EndpointURL().Host, b, err)
		}
		if ok {
			continue
		}
		if err := c.MakeBucket(ctx, b, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("repository: minio make bucket %s: %w", b, err)
		}
	}
	return nil
}
