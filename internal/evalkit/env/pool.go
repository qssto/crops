package env

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/seed"
)

const (
	// DefaultSchemaPrefix 与 DefaultEnvPrefix 是评测 trial 的库名 / Redis env 前缀（母文档 §4.6）。
	DefaultSchemaPrefix = "agri_eval_t_"
	DefaultEnvPrefix    = "eval-t"
	defaultSize         = 1
	scanBatch           = 200
	migrationsTable     = "schema_migrations"
)

// tablePattern 限定 TRUNCATE 拼接的表名（来自 information_schema，仍做白名单校验）。
var tablePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// PoolConfig 是 trial schema 池参数。
type PoolConfig struct {
	MySQL         config.MySQL          // 连接参数；库名由 SchemaPrefix + n 决定
	MigrationsDir string                // 业务库迁移目录（migrations）
	Seed          *seed.Data            // 每次 Acquire 重新灌入的种子
	Redis         redis.UniversalClient // 共享实例，按 as:{env}: 前缀隔离
	Logger        *slog.Logger
	Size          int    // 槽位数，默认 1
	SchemaPrefix  string // 默认 agri_eval_t_
	EnvPrefix     string // 默认 eval-t
}

// Slot 是一个隔离环境：独立库 + 独立 Redis 前缀 + 独立 FrozenClock（GORM NowFunc 也指向它）。
type Slot struct {
	N      int
	Schema string
	DB     *repository.DB
	Keys   rediskey.Keys
	Clock  *FrozenClock
}

// Pool 管理 trial 槽位：Acquire 时清表、清 Redis 前缀、重灌种子、拨钟。
type Pool struct {
	cfg   PoolConfig
	all   []*Slot
	slots chan *Slot
}

// NewPool 建库、迁移并打开全部槽位；任一步失败则关闭已打开的连接并返回错误。
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	if cfg.Seed == nil || cfg.Redis == nil || cfg.Logger == nil || cfg.MigrationsDir == "" {
		return nil, errors.New("env: PoolConfig requires Seed, Redis, Logger and MigrationsDir")
	}
	if cfg.Size <= 0 {
		cfg.Size = defaultSize
	}
	if cfg.SchemaPrefix == "" {
		cfg.SchemaPrefix = DefaultSchemaPrefix
	}
	if cfg.EnvPrefix == "" {
		cfg.EnvPrefix = DefaultEnvPrefix
	}
	p := &Pool{cfg: cfg, slots: make(chan *Slot, cfg.Size)}
	for n := 1; n <= cfg.Size; n++ {
		s, err := p.openSlot(ctx, n)
		if err != nil {
			return nil, errors.Join(err, p.Close())
		}
		p.all = append(p.all, s)
		p.slots <- s
	}
	return p, nil
}

func (p *Pool) openSlot(ctx context.Context, n int) (*Slot, error) {
	schema := p.cfg.SchemaPrefix + strconv.Itoa(n)
	if err := repository.CreateDatabase(ctx, p.cfg.MySQL.DSN("", false), schema); err != nil {
		return nil, fmt.Errorf("env: slot %d: %w", n, err)
	}
	if _, err := repository.Migrate(p.cfg.MySQL.DSN(schema, true), p.cfg.MigrationsDir); err != nil {
		return nil, fmt.Errorf("env: slot %d: %w", n, err)
	}
	keys, err := rediskey.New(p.cfg.EnvPrefix + strconv.Itoa(n))
	if err != nil {
		return nil, fmt.Errorf("env: slot %d: %w", n, err)
	}
	clk := NewFrozenClock(time.Unix(0, 0))
	db, err := repository.OpenMySQL(ctx, repository.OpenOptions{
		DSN: p.cfg.MySQL.DSN(schema, false), MaxOpenConns: p.cfg.MySQL.MaxOpenConns, Clock: clk, Logger: p.cfg.Logger})
	if err != nil {
		return nil, fmt.Errorf("env: slot %d: %w", n, err)
	}
	return &Slot{N: n, Schema: schema, DB: db, Keys: keys, Clock: clk}, nil
}

// Acquire 取一个空闲槽位并重置到 at 时刻的干净状态；无空闲槽位时阻塞到 ctx 结束。
// 重置失败时槽位归还池中，返回错误。
func (p *Pool) Acquire(ctx context.Context, at time.Time) (*Slot, error) {
	var s *Slot
	select {
	case s = <-p.slots:
	case <-ctx.Done():
		return nil, fmt.Errorf("env: acquire slot: %w", ctx.Err())
	}
	if err := p.reset(ctx, s, at); err != nil {
		p.Release(s)
		return nil, err
	}
	return s, nil
}

// Release 归还槽位（不清理；清理在下次 Acquire 时做，便于失败后人工查看现场）。
func (p *Pool) Release(s *Slot) { p.slots <- s }

func (p *Pool) reset(ctx context.Context, s *Slot, at time.Time) error {
	s.Clock.Set(at)
	if err := truncateAll(ctx, s.DB, s.Schema); err != nil {
		return fmt.Errorf("env: reset %s: %w", s.Schema, err)
	}
	if err := deletePrefix(ctx, p.cfg.Redis, s.Keys.Prefix()); err != nil {
		return fmt.Errorf("env: reset %s: %w", s.Keys.Prefix(), err)
	}
	if err := seed.Apply(ctx, s.DB, p.cfg.Seed, at); err != nil {
		return fmt.Errorf("env: reset %s: %w", s.Schema, err)
	}
	return nil
}

func truncateAll(ctx context.Context, db *repository.DB, schema string) error {
	var tables []string
	err := db.WithContext(ctx).Raw(
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_name <> ?",
		schema, migrationsTable).Scan(&tables).Error
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	for _, t := range tables {
		if !tablePattern.MatchString(t) {
			return fmt.Errorf("unexpected table name %q", t)
		}
		if err := db.WithContext(ctx).Exec("TRUNCATE TABLE `" + t + "`").Error; err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	return nil
}

func deletePrefix(ctx context.Context, rdb redis.UniversalClient, prefix string) error {
	iter := rdb.Scan(ctx, 0, prefix+"*", scanBatch).Iterator()
	var batch []string
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= scanBatch {
			if err := rdb.Del(ctx, batch...).Err(); err != nil {
				return fmt.Errorf("del keys: %w", err)
			}
			batch = batch[:0]
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("scan %s*: %w", prefix, err)
	}
	if len(batch) > 0 {
		if err := rdb.Del(ctx, batch...).Err(); err != nil {
			return fmt.Errorf("del keys: %w", err)
		}
	}
	return nil
}

// Close 关闭全部槽位连接（不删库，便于复用与排查）。
func (p *Pool) Close() error {
	var errs []error
	for _, s := range p.all {
		errs = append(errs, repository.Close(s.DB))
	}
	return errors.Join(errs...)
}
