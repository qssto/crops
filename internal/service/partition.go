package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// 分区维护（P0 §6.8 功能任务 7）：每月 25 日起（按 Clock、中国时区）为 audit_log / run_event 预建后两个月分区。
const (
	ScanPartition           = "partition"
	partitionDay            = 25
	partitionAhead          = 2
	partitionLockTTL        = 10 * time.Minute
	DefaultPartitionCheck   = time.Hour
	partitionChinaOffsetSec = 8 * 3600
)

// PartitionStore 是分区维护需要的仓储能力，repository.PartitionRepo 实现。
type PartitionStore interface {
	Partitions(ctx context.Context, table string) ([]string, error)
	SplitMax(ctx context.Context, table string, months []time.Time) error
	CountMax(ctx context.Context, table string) (int64, error)
}

// PartitionDeps 是 PartitionMaintainer 的依赖；Store / Locks / Clock 必填。
type PartitionDeps struct {
	Store    PartitionStore
	Locks    KeyLocker
	Keys     rediskey.Keys
	Clock    clock.Clock
	Logger   *slog.Logger
	Loc      *time.Location // 判定「25 日」的时区，空取 UTC+8
	Interval time.Duration  // 检查周期，0 取 1h
}

// PartitionMaintainer 以 lock:scan:partition 互斥，在每月 25 日（及之后、本月尚未成功时）预建分区。
type PartitionMaintainer struct {
	d      PartitionDeps
	tables []string
	mu     sync.Mutex
	done   string // 本进程最近一次成功维护的月份 yyyy-mm
}

// NewPartitionMaintainer 校验依赖。
func NewPartitionMaintainer(d PartitionDeps) (*PartitionMaintainer, error) {
	if d.Store == nil || d.Locks == nil || d.Clock == nil {
		return nil, errors.New("service: partition maintainer needs Store, Locks and Clock")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Loc == nil {
		d.Loc = time.FixedZone("CST", partitionChinaOffsetSec)
	}
	if d.Interval <= 0 {
		d.Interval = DefaultPartitionCheck
	}
	return &PartitionMaintainer{d: d, tables: []string{repository.TableAuditLog, repository.TableRunEvent}}, nil
}

// Run 启动即检查一次，之后每个周期检查，直到 ctx 取消。
func (m *PartitionMaintainer) Run(ctx context.Context) {
	t := time.NewTicker(m.d.Interval)
	defer t.Stop()
	for {
		if err := m.Tick(ctx); err != nil {
			m.d.Logger.WarnContext(ctx, "partition maintenance", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 在当月 25 日及之后且本月未成功维护时执行 RunOnce。
func (m *PartitionMaintainer) Tick(ctx context.Context) error {
	now := m.d.Clock.Now().In(m.d.Loc)
	month := now.Format("2006-01")
	m.mu.Lock()
	skip := now.Day() < partitionDay || m.done == month
	m.mu.Unlock()
	if skip {
		return nil
	}
	ran, err := m.RunOnce(ctx)
	if err != nil || !ran {
		return err
	}
	m.mu.Lock()
	m.done = month
	m.mu.Unlock()
	return nil
}

// RunOnce 持 lock:scan:partition 维护各表；锁被他人持有返回 ran=false。单表失败不影响其余表。
func (m *PartitionMaintainer) RunOnce(ctx context.Context) (ran bool, err error) {
	lease, err := m.d.Locks.AcquireLease(ctx, m.d.Keys.LockScan(ScanPartition), partitionLockTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("partition lock: %w", err)
	}
	defer func() {
		if rerr := lease.Release(context.WithoutCancel(ctx)); rerr != nil {
			m.d.Logger.WarnContext(ctx, "release partition lock", "err", rerr)
		}
	}()
	now := m.d.Clock.Now().In(m.d.Loc)
	var errs []error
	for _, table := range m.tables {
		if err := m.ensure(ctx, table, now); err != nil {
			errs = append(errs, fmt.Errorf("partition %s: %w", table, err))
		}
	}
	return true, errors.Join(errs...)
}

// ensure 先查 pmax 行数（非 0 告警），再把缺的月分区从 pmax 拆出；已存在则跳过。
func (m *PartitionMaintainer) ensure(ctx context.Context, table string, now time.Time) error {
	n, err := m.d.Store.CountMax(ctx, table)
	if err != nil {
		return err
	}
	if n > 0 {
		m.d.Logger.ErrorContext(ctx, "rows landed in pmax", "table", table, "rows", n)
	}
	names, err := m.d.Store.Partitions(ctx, table)
	if err != nil {
		return err
	}
	months := MissingMonths(names, now, partitionAhead)
	if len(months) == 0 {
		return nil
	}
	if err := m.d.Store.SplitMax(ctx, table, months); err != nil {
		return err
	}
	m.d.Logger.InfoContext(ctx, "partitions added", "table", table, "from", repository.PartitionName(months[0]),
		"to", repository.PartitionName(months[len(months)-1]))
	return nil
}

// MissingMonths 返回需要从 pmax 拆出的月份（升序，各取当月 1 日）：从现有最大 pYYYYMM 的下一月起（中间缺月也补齐，
// 保持按月粒度），到 now 所在月 + ahead。没有任何月分区时从 now 所在月起。不认识的分区名（含 pmax）忽略。
func MissingMonths(partitions []string, now time.Time, ahead int) []time.Time {
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	target := cur.AddDate(0, ahead, 0)
	var next time.Time
	for _, p := range partitions {
		m, err := time.Parse("p200601", p)
		if err != nil {
			continue
		}
		if after := m.AddDate(0, 1, 0); after.After(next) {
			next = after
		}
	}
	if next.IsZero() {
		next = cur
	}
	var out []time.Time
	for m := next; !m.After(target); m = m.AddDate(0, 1, 0) {
		out = append(out, m)
	}
	return out
}
