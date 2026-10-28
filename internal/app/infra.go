package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
)

// OpenMySQL 按配置打开 db 库（GORM，NowFunc = c.Now，日志接 slog）。
func OpenMySQL(ctx context.Context, cfg *config.Config, db string, c clock.Clock, log *slog.Logger) (*gorm.DB, error) {
	if err := cfg.RequireSecrets("mysql.password"); err != nil {
		return nil, err
	}
	return repository.OpenMySQL(ctx, repository.OpenOptions{
		DSN: cfg.MySQL.DSN(db, false), MaxOpenConns: cfg.MySQL.MaxOpenConns, Clock: c, Logger: log})
}

// OpenRedis 按配置建 Redis 客户端并 PING。
func OpenRedis(ctx context.Context, cfg *config.Config) (*redis.Client, error) {
	if err := cfg.RequireSecrets("redis.password"); err != nil {
		return nil, err
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("app: ping redis %s: %w", cfg.Redis.Addr, err), rdb.Close())
	}
	return rdb, nil
}

// NewMinIO 按配置建 MinIO 客户端（不建连，首次请求时才连接）。
func NewMinIO(cfg *config.Config) (*minio.Client, error) {
	if err := cfg.RequireSecrets("minio.access_key", "minio.secret_key"); err != nil {
		return nil, err
	}
	c, err := minio.New(cfg.MinIO.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIO.AccessKey, cfg.MinIO.SecretKey, ""),
		Secure: cfg.MinIO.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("app: minio client %s: %w", cfg.MinIO.Endpoint, err)
	}
	return c, nil
}
