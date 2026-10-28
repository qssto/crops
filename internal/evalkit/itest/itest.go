//go:build integration

// Package itest 是集成测试共用的环境装配：真实 docker MySQL / Redis，经 env.Pool 拿隔离 schema。
// 每个测试包传不同 name，避免 go test 并行跑多个包时互相清表。
package itest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/app"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/repository"
	"agri-steward/internal/seed"
)

// Env 是一个测试包的集成环境。
type Env struct {
	Root  string
	Cfg   *config.Config
	Redis *redis.Client
	Pool  *env.Pool
}

// RepoRoot 从当前目录向上找 go.mod。
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("itest: getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("itest: go.mod not found")
		}
		dir = parent
	}
}

// Setup 打开 agri_it_{name}_t_1 库与 as:it-{name}-t1: 前缀；任何依赖不可用直接 Fatal（集成测试不 Skip）。
func Setup(t *testing.T, name string) *Env {
	t.Helper()
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "configs/app.yaml"), filepath.Join(root, "deploy/.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireSecrets("mysql.password", "redis.password"); err != nil {
		t.Fatal(err)
	}
	data, err := seed.Load(filepath.Join(root, "configs/seed/fields.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rdb, err := app.OpenRedis(ctx, cfg)
	if err != nil {
		t.Fatalf("redis unavailable (docker compose up?): %v", err)
	}
	pool, err := env.NewPool(ctx, env.PoolConfig{MySQL: cfg.MySQL, MigrationsDir: filepath.Join(root, "migrations"),
		Seed: data, Redis: rdb, Logger: logger, SchemaPrefix: "agri_it_" + name + "_t_", EnvPrefix: "it-" + name + "-t"})
	if err != nil {
		t.Fatalf("mysql unavailable (docker compose up?): %v", errors.Join(err, rdb.Close()))
	}
	t.Cleanup(func() {
		if err := errors.Join(pool.Close(), rdb.Close()); err != nil {
			t.Errorf("itest cleanup: %v", err)
		}
	})
	return &Env{Root: root, Cfg: cfg, Redis: rdb, Pool: pool}
}

// Fresh 取一个重置到 at 的槽位，测试结束归还。
func (e *Env) Fresh(t *testing.T, at time.Time) *env.Slot {
	t.Helper()
	s, err := e.Pool.Acquire(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Pool.Release(s) })
	return s
}

// 示范农场（configs/seed/fields.yaml）的 tenant / farm。
const (
	TenantID = 1
	FarmID   = 1
)

// InsertRun 插入一条 RUNNING 的 agent_run，返回 id。
func InsertRun(t *testing.T, db *repository.DB, at time.Time) uint64 {
	t.Helper()
	return insert(t, db, `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version,
  prompt_version, model_tier, knowledge_ref, created_at, updated_at)
  VALUES (?, ?, 1, 1001, 'RUNNING', 'g1', 'p1', 'small', 'kb@test', ?, ?)`, TenantID, FarmID, at, at)
}

// InsertOrder 插入一条指定状态的 work_order（advice_id 取 runID 保证唯一），返回 id。
func InsertOrder(t *testing.T, db *repository.DB, runID uint64, status string, at time.Time) uint64 {
	t.Helper()
	return insert(t, db, `INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type,
  device_type, payload, time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at)
  VALUES (?, ?, 1, ?, ?, 'SPRAY', 'DRONE', '{}', ?, ?, ?, 'DRY_RUN', 1, ?, ?)`,
		TenantID, FarmID, runID, runID, at, at.Add(time.Hour), status, at, at)
}

func insert(t *testing.T, db *repository.DB, q string, args ...any) uint64 {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	res, err := sqlDB.ExecContext(t.Context(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return uint64(id)
}

// Count 返回 SELECT COUNT(*) 结果。
func Count(t *testing.T, db *repository.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.WithContext(t.Context()).Raw(q, args...).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}
