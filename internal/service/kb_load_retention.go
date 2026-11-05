package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// kb_load_item 保留期清理（P2 L5 §6.6 技术实现 ②）：worker 任务逐农场执行 DELETE ... LIMIT 5000，
// 直到本农场无早于截止时间的行。单实例互斥走 lock:p2job:{job}，锁被他人持有时本轮跳过；每清完一个农场续期一次。

// KBLoadRetentionJob 是任务名（Loop 名与 lock:p2job:{job} 的 job）。
const KBLoadRetentionJob = "kb-load-retention"

// DefaultKBLoadRetentionDays 是 retention.table_days.kb_load_item 未配置时的天数（§6.6：默认 180 天）。
const DefaultKBLoadRetentionDays = 180

// DefaultKBLoadRetentionInterval 是清理周期。
const DefaultKBLoadRetentionInterval = time.Hour

// kbLoadMaxBatchesPerFarm 限制单农场单轮 DELETE 次数（每次 ≤ 5000 行），余量留到下一轮，避免单轮长时间持锁。
const kbLoadMaxBatchesPerFarm = 100

// KBLoadPurger 是清理所需的仓储（repository.KBLoadItemRepo 实现）。
type KBLoadPurger interface {
	KBLoadFarms(ctx context.Context, sys repository.SystemScope, before time.Time) ([]model.Scope, error)
	PurgeKBLoadItems(ctx context.Context, scope model.Scope, before time.Time) (int64, error)
}

// KBLoadRetention 是 kb_load_item 保留期清理任务。
type KBLoadRetention struct {
	Store    KBLoadPurger
	Locks    KeyLocker // 可空：不互斥（单测）
	LockKey  string    // Keys.LockP2Job(KBLoadRetentionJob)
	Clock    clock.Clock
	Logger   *slog.Logger
	Days     int           // retention.table_days.kb_load_item，须 > 0
	Interval time.Duration // ≤ 0 取 DefaultKBLoadRetentionInterval
}

// Tick 执行一轮清理，返回删除行数。锁被他人持有时返回 0, nil；续期失败（锁丢失）即停止并返回错误。
func (r *KBLoadRetention) Tick(ctx context.Context) (int64, error) {
	if r.Store == nil || r.Clock == nil || r.Logger == nil {
		return 0, errors.New("service: kb_load retention needs store / clock / logger")
	}
	if r.Days <= 0 {
		return 0, fmt.Errorf("service: kb_load retention days %d must be > 0", r.Days)
	}
	var lease agent.Lease
	if r.Locks != nil {
		l, err := r.Locks.AcquireLease(ctx, r.LockKey, rediskey.LockP2JobTTL)
		if errors.Is(err, repository.ErrLockHeld) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("service: kb_load retention lock: %w", err)
		}
		lease = l
		defer func() {
			if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
				r.Logger.WarnContext(ctx, "kb_load retention release lock", "err", err)
			}
		}()
	}
	before := r.Clock.Now().AddDate(0, 0, -r.Days)
	farms, err := r.Store.KBLoadFarms(ctx, scanSystem, before)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, sc := range farms {
		n, err := r.purgeFarm(ctx, sc, before)
		total += n
		if err != nil {
			return total, err
		}
		if lease != nil {
			if err := lease.Renew(ctx); err != nil {
				return total, fmt.Errorf("service: kb_load retention renew lock: %w", err)
			}
		}
	}
	if total > 0 {
		r.Logger.InfoContext(ctx, "kb_load_item purged", "rows", total, "farms", len(farms), "before", before)
	}
	return total, nil
}

// purgeFarm 对一个农场重复 DELETE ... LIMIT 5000，直到删除行数不足一批或达到单轮上限。
func (r *KBLoadRetention) purgeFarm(ctx context.Context, sc model.Scope, before time.Time) (int64, error) {
	var total int64
	for range kbLoadMaxBatchesPerFarm {
		n, err := r.Store.PurgeKBLoadItems(ctx, sc, before)
		total += n
		if err != nil || n < repository.KBLoadPurgeBatch {
			return total, err
		}
	}
	return total, nil
}

// Run 周期执行 Tick，直到 ctx 取消。
func (r *KBLoadRetention) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultKBLoadRetentionInterval
	}
	runPeriodic(ctx, interval, r.Logger, "kb_load retention", func(ctx context.Context) error {
		_, err := r.Tick(ctx)
		return err
	})
}
