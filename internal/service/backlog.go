package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"agri-steward/internal/repository"
)

// P1 L7-B 审批积压 gauge：worker 每个扫描周期查一次各农场 OPEN 的 ORDER 票数，整体替换 as_approval_backlog。
// 不取 lock:scan：gauge 是进程内状态，每个 worker 副本各自刷新（规则用 max by(farm_id) 合并副本）。

// BacklogStore 按农场统计 OPEN 的 ORDER 票，repository.AlertRepo 实现。
type BacklogStore interface {
	OpenOrderTickets(ctx context.Context, sys repository.SystemScope) ([]repository.FarmCount, error)
}

// BacklogRefresher 周期刷新审批积压 gauge。
type BacklogRefresher struct {
	store    BacklogStore
	obs      Observer
	log      *slog.Logger
	interval time.Duration
}

// NewBacklogRefresher 构造刷新器；interval 为 0 取 DefaultScanInterval。
func NewBacklogRefresher(store BacklogStore, obs Observer, log *slog.Logger, interval time.Duration) (*BacklogRefresher, error) {
	if store == nil || obs == nil || log == nil {
		return nil, errors.New("service: backlog refresher needs store / observer / logger")
	}
	if interval <= 0 {
		interval = DefaultScanInterval
	}
	return &BacklogRefresher{store: store, obs: obs, log: log, interval: interval}, nil
}

// RefreshOnce 查一次并替换 gauge；查询失败保留上一轮的值并返回错误。
func (b *BacklogRefresher) RefreshOnce(ctx context.Context) error {
	rows, err := b.store.OpenOrderTickets(ctx, scanSystem)
	if err != nil {
		return err
	}
	open := make(map[uint64]int, len(rows))
	for _, r := range rows {
		open[r.FarmID] += r.N
	}
	b.obs.ApprovalBacklog(open)
	return nil
}

// Run 启动即刷新一次，之后每个周期刷新，直到 ctx 取消。
func (b *BacklogRefresher) Run(ctx context.Context) {
	runPeriodic(ctx, b.interval, b.log, "approval backlog refresh", b.RefreshOnce)
}

// runPeriodic 启动即执行一次 fn，之后每个 interval 执行一次，直到 ctx 取消；失败只记 Warn。
func runPeriodic(ctx context.Context, interval time.Duration, log *slog.Logger, what string, fn func(context.Context) error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, what, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// FarmLister 列出已登记农场（预置指标序列用），repository.AlertRepo 实现。
type FarmLister interface {
	KnownFarms(ctx context.Context, sys repository.SystemScope) ([]uint64, error)
}

// FarmSeriesPreseeder 按农场预置 0 值序列，MetricsObserver 实现。
type FarmSeriesPreseeder interface {
	Preseed(farmIDs []uint64)
}

// MetricsPreseeder 周期读已登记农场并预置 0 值计数器序列（审查修复 P0：新序列首样本 increase 不可见）。
// server 与 worker 各自的注册表都要预置（随 /metrics 监听任务启动）；新登记的农场在下一个周期补上，
// 其间由规则文件的 unless/offset 新序列分支兜底。
type MetricsPreseeder struct {
	farms    FarmLister
	obs      FarmSeriesPreseeder
	log      *slog.Logger
	interval time.Duration
}

// NewMetricsPreseeder 构造预置器；interval 为 0 取 DefaultScanInterval。
func NewMetricsPreseeder(farms FarmLister, obs FarmSeriesPreseeder, log *slog.Logger,
	interval time.Duration) (*MetricsPreseeder, error) {
	if farms == nil || obs == nil || log == nil {
		return nil, errors.New("service: metrics preseeder needs farm lister / observer / logger")
	}
	if interval <= 0 {
		interval = DefaultScanInterval
	}
	return &MetricsPreseeder{farms: farms, obs: obs, log: log, interval: interval}, nil
}

// PreseedOnce 读一次农场列表并预置；读失败返回错误（farm_id=0 的序列已在构造注册表时预置）。
func (p *MetricsPreseeder) PreseedOnce(ctx context.Context) error {
	ids, err := p.farms.KnownFarms(ctx, scanSystem)
	if err != nil {
		return err
	}
	p.obs.Preseed(ids)
	return nil
}

// Run 启动即预置一次，之后每个周期补一次，直到 ctx 取消。
func (p *MetricsPreseeder) Run(ctx context.Context) {
	runPeriodic(ctx, p.interval, p.log, "metrics preseed", p.PreseedOnce)
}
