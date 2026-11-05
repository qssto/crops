package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// P2 L4：rule_shadow_diff 保留期清理（§6.5 技术实现：缺省 180 天，worker 按农场逐项 DELETE … LIMIT 5000）。
// 单实例互斥走 lock:p2job:rule-shadow-purge（P2 第 3 批接缝）；只在 cmd/worker 运行（registerP2Loops）。

// LoopRuleShadowPurge 是清理任务名（也是 lock:p2job 的 job 名与 SystemScope 任务名）。
const LoopRuleShadowPurge = "rule-shadow-purge"

// shadowPurgeScope 是清理任务的系统扫描凭证（只构造一次：archtest 白名单只放行本文件构造 SystemScope）。
var shadowPurgeScope = repository.NewSystemScope(LoopRuleShadowPurge)

// 主链路 persist 对 AdviceRepo 做 ShadowAdviceStore 断言（同事务写影子行）；此处编译期保证实现不脱节。
var _ graph.ShadowAdviceStore = (*repository.AdviceRepo)(nil)

func init() { registerP2Loops("P2-L4", ruleOffloadLoops) }

// ruleOffloadLoops：DB 或锁未装配时不起任务；配置非法时 worker 启动失败（零值取 config.DefaultRuleOffload）。
func ruleOffloadLoops(a *App) ([]Loop, error) {
	if a == nil || a.DB == nil || a.Locker == nil {
		return nil, nil
	}
	cfg := a.ruleOffload
	if cfg == (config.RuleOffload{}) {
		cfg = config.DefaultRuleOffload()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	p := &shadowPurger{store: repository.RuleShadowDiffRepo{DB: a.DB}, locks: a.Locker,
		key: a.Keys.LockP2Job(LoopRuleShadowPurge), clock: a.Clock, log: a.Logger, keep: cfg.Retention(), every: cfg.PurgeEvery}
	return []Loop{{Name: LoopRuleShadowPurge, Run: p.Run}}, nil
}

// shadowPurgeStore 是清理用到的仓储（repository.RuleShadowDiffRepo 实现）。
type shadowPurgeStore interface {
	ShadowFarms(ctx context.Context, sys repository.SystemScope, before time.Time) ([]repository.FarmRef, error)
	PurgeBatch(ctx context.Context, sys repository.SystemScope, farm repository.FarmRef, item model.RuleOffloadItem,
		before time.Time) (int64, error)
}

// leaseLocker 取 Redis token 锁（repository.Locker 实现；被持有返回 repository.ErrLockHeld）。
type leaseLocker interface {
	AcquireLease(ctx context.Context, key string, ttl time.Duration) (agent.Lease, error)
}

// shadowPurger 周期清理超过保留期的影子行。
type shadowPurger struct {
	store shadowPurgeStore
	locks leaseLocker
	key   string
	clock clock.Clock
	log   *slog.Logger
	keep  time.Duration
	every time.Duration
}

// Run 启动即清理一次，之后每 every 一次，直到 ctx 取消。单轮失败只记日志，下一轮重试。
func (p *shadowPurger) Run(ctx context.Context) {
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		if err := p.tick(ctx); err != nil && ctx.Err() == nil {
			p.log.ErrorContext(ctx, "rule_shadow_diff purge failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick 持锁清理一轮；锁被他人持有（另一副本或手动补跑）时跳过本轮。
func (p *shadowPurger) tick(ctx context.Context) (err error) {
	lease, err := p.locks.AcquireLease(ctx, p.key, rediskey.LockP2JobTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		p.log.InfoContext(ctx, "rule_shadow_diff purge skipped, lock held", "key", p.key)
		return nil
	}
	if err != nil {
		return fmt.Errorf("acquire %s: %w", p.key, err)
	}
	defer func() {
		// 释放用独立 ctx：ctx 已取消时也尽量释放（否则等 TTL 过期）。
		if rerr := lease.Release(context.WithoutCancel(ctx)); rerr != nil {
			err = errors.Join(err, fmt.Errorf("release %s: %w", p.key, rerr))
		}
	}()
	n, err := p.purge(ctx, lease)
	if err != nil {
		return fmt.Errorf("after %d rows: %w", n, err)
	}
	p.log.InfoContext(ctx, "rule_shadow_diff purged", "rows", n, "keep", p.keep)
	return nil
}

// purge 逐农场逐项分批删除早于 now-keep 的行（每批后续租；单批不足 ShadowPurgeBatch 即该项删完）。返回删除总行数。
func (p *shadowPurger) purge(ctx context.Context, lease agent.Lease) (int64, error) {
	sys := shadowPurgeScope
	before := p.clock.Now().Add(-p.keep)
	farms, err := p.store.ShadowFarms(ctx, sys, before)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, farm := range farms {
		for _, item := range model.RuleOffloadItems() {
			for {
				n, err := p.store.PurgeBatch(ctx, sys, farm, item, before)
				total += n
				if err != nil {
					return total, err
				}
				if err := lease.Renew(ctx); err != nil {
					return total, fmt.Errorf("renew %s: %w", p.key, err)
				}
				if n < repository.ShadowPurgeBatch {
					break
				}
			}
		}
	}
	return total, nil
}
