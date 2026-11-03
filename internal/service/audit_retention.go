package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7 保留期清理（§6.8 技术实现第 7 条）：worker 任务 retention 每日一轮，按 idx_expire 分批取到期 raw_object，
// 跳过仍被未终态工单 / 未关闭票据引用的行，先删 MinIO 对象再置 deleted_at（行保留作墓碑），每批按 (租户, 农场, kind)
// 写 RETENTION_PURGE 审计。audit_log / run_event 只登记保留期，本任务不删。

// ScanRetention 是保留期清理任务名（lock:scan:retention）。
const ScanRetention = "retention"

// 分批缺省值：每批 200 行，每个扫描周期最多 5 批（≤1000 次对象删除，单轮秒级）。扫描任务串行执行、锁 TTL 60s 不续期，
// 单轮不能长时间拖住同周期的其他扫描；未扫完的部分由当日游标在下一周期续扫。
const (
	DefaultRetentionBatch      = 200
	DefaultRetentionMaxBatches = 5
)

// RetentionStore 是清理的仓储能力，repository.RetentionRepo 实现。
type RetentionStore interface {
	ListExpired(ctx context.Context, sys repository.SystemScope, now time.Time, after repository.RetentionCursor,
		limit int) ([]repository.ExpiredRaw, error)
	Referenced(ctx context.Context, sys repository.SystemScope, ids []uint64) (map[uint64]bool, error)
	MarkDeletedTx(ctx context.Context, tx *repository.DB, ref repository.RawRef, now time.Time) (bool, error)
}

// RetentionService 执行保留期清理（由 Scanner 的 retention 任务驱动）。
type RetentionService struct {
	Store      RetentionStore
	Tx         Transactor
	Audit      AuditTxAppender
	Objects    repository.ObjectRemover
	Bucket     string // raw bucket
	Logger     *slog.Logger
	BatchSize  int // 0 取 DefaultRetentionBatch
	MaxBatches int // 0 取 DefaultRetentionMaxBatches

	mu     sync.Mutex
	date   string                     // 当前清理轮次的日期（北京时间）
	cursor repository.RetentionCursor // 当日游标：跨扫描周期续扫，被引用 / 删除失败的行当日不再重试
	done   bool                       // 当日已扫完
}

// RetentionStats 是一次 Tick 的结果。
type RetentionStats struct {
	Purged, Referenced, Failed int
	Drained                    bool
}

// Tick 续扫当日到期行；当日已扫完直接返回。删对象失败的行不置墓碑，次日重试；错误汇总返回。
func (s *RetentionService) Tick(ctx context.Context, sys repository.SystemScope, now time.Time) (RetentionStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := AnchorDate(now); d != s.date {
		s.date, s.cursor, s.done = d, repository.RetentionCursor{}, false
	}
	var st RetentionStats
	if s.done {
		st.Drained = true
		return st, nil
	}
	size, maxBatches := s.BatchSize, s.MaxBatches
	if size <= 0 {
		size = DefaultRetentionBatch
	}
	if maxBatches <= 0 {
		maxBatches = DefaultRetentionMaxBatches
	}
	var errs []error
	for range maxBatches {
		rows, err := s.Store.ListExpired(ctx, sys, now, s.cursor, size)
		if err != nil {
			return st, errors.Join(append(errs, fmt.Errorf("service: retention list: %w", err))...)
		}
		if len(rows) == 0 {
			s.done, st.Drained = true, true
			break
		}
		if err := s.purgeBatch(ctx, sys, rows, now, &st); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil || errors.Is(err, errRetentionFatal) {
				break // 游标不前进：本批下一周期重扫（已落墓碑的行不会再被列出）
			}
		}
		last := rows[len(rows)-1]
		s.cursor = repository.RetentionCursor{ExpireAt: last.ExpireAt, ID: last.ID}
		if len(rows) < size {
			s.done, st.Drained = true, true
			break
		}
	}
	if st.Purged+st.Failed > 0 || st.Drained {
		s.Logger.InfoContext(ctx, "retention tick", "date", s.date, "purged", st.Purged, "referenced", st.Referenced,
			"failed", st.Failed, "drained", st.Drained)
	}
	return st, errors.Join(errs...)
}

// errRetentionFatal 标记本批未能落墓碑（引用查询或事务失败），游标不前进以免漏删。
var errRetentionFatal = errors.New("service: retention batch aborted")

// purgeBatch 处理一批：查引用 → 删对象 → 同一事务置墓碑并按 (tenant, farm, kind) 写 RETENTION_PURGE。
func (s *RetentionService) purgeBatch(ctx context.Context, sys repository.SystemScope, rows []repository.ExpiredRaw,
	now time.Time, st *RetentionStats) error {
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	refs, err := s.Store.Referenced(ctx, sys, ids)
	if err != nil {
		return fmt.Errorf("%w: references: %w", errRetentionFatal, err)
	}
	var removed []repository.ExpiredRaw
	var errs []error
	for _, r := range rows {
		if refs[r.ID] {
			st.Referenced++
			continue
		}
		if err := s.Objects.RemoveObject(ctx, s.Bucket, r.ObjectKey); err != nil {
			st.Failed++
			errs = append(errs, fmt.Errorf("service: retention remove raw_object %d: %w", r.ID, err))
			continue
		}
		removed = append(removed, r)
	}
	if len(removed) == 0 {
		return errors.Join(errs...)
	}
	n := 0
	err = s.Tx.InTx(ctx, func(tx *repository.DB) error {
		n = 0
		groups := map[purgeGroup]*purgeSummary{}
		var order []purgeGroup
		for _, r := range removed {
			ok, err := s.Store.MarkDeletedTx(ctx, tx, r.RawRef, now)
			if err != nil {
				return err
			}
			if !ok {
				continue // 已被他人清理
			}
			n++
			g := purgeGroup{TenantID: r.TenantID, FarmID: r.FarmID, Kind: r.Kind}
			sum := groups[g]
			if sum == nil {
				sum = &purgeSummary{Kind: string(r.Kind)}
				groups[g] = sum
				order = append(order, g)
			}
			sum.Count++
			if r.ExpireAt.After(sum.maxExpire) {
				sum.maxExpire = r.ExpireAt
			}
		}
		for _, g := range order {
			if err := s.appendPurgeAudit(ctx, tx, g, groups[g], now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// 对象已删、墓碑未落：下一轮 ListExpired 仍会取到，再删对象（不存在视为成功）后补墓碑。
		return errors.Join(append(errs, fmt.Errorf("%w: tombstone: %w", errRetentionFatal, err))...)
	}
	st.Purged += n
	return errors.Join(errs...)
}

type purgeGroup struct {
	TenantID, FarmID uint64
	Kind             model.RawKind
}

// purgeSummary 是 RETENTION_PURGE 的 summary（不含对象 key / 内容）。
type purgeSummary struct {
	Kind        string `json:"kind"`
	Count       int    `json:"count"`
	MaxExpireAt string `json:"max_expire_at"`
	maxExpire   time.Time
}

func (s *RetentionService) appendPurgeAudit(ctx context.Context, tx *repository.DB, g purgeGroup, sum *purgeSummary,
	now time.Time) error {
	sum.MaxExpireAt = sum.maxExpire.UTC().Format("2006-01-02T15:04:05.000Z")
	b, err := json.Marshal(sum)
	if err != nil {
		return fmt.Errorf("service: marshal retention summary: %w", err)
	}
	return s.Audit.AppendTx(ctx, tx, repository.NewAuditRecord(model.Scope{TenantID: g.TenantID, FarmID: g.FarmID}, nil,
		model.Actor{Type: model.ActorSystem}, model.AuditRetentionPurge, "raw_kind:"+string(g.Kind), b,
		model.AuditResultOK, now))
}
