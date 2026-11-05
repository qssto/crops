package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// kb_load_item（迁移 0015，P2 L5 §6.6 技术实现 ②，定版 SQL 照抄）：知识加载逐条明细。诊断数据，保留期逐农场清理。

var _ agent.KBLoadItemStore = KBLoadItemRepo{}

// KBLoadItemRepo 读写 kb_load_item。
type KBLoadItemRepo struct{ DB *gorm.DB }

// KBLoadPurgeBatch 是保留期清理单条 DELETE 的行数上限（§6.6：LIMIT 5000）。
const KBLoadPurgeBatch = 5000

// kb_load_item 列宽（varchar）。
const (
	kbLoadQuestionTypeMax = 32
	kbLoadKBIDMax         = 96
	kbLoadVersionMax      = 16
	kbLoadStrategyMax     = 16
)

// kbLoadItemCols 是 INSERT 列序（不含自增 id）。
const kbLoadItemCols = `tenant_id, farm_id, run_id, load_seq, rank_no, question_type, kb_id, kb_version, pick_reason, strategy, created_at`

// ReplaceKBLoadItems 写一次加载 (run_id, load_seq) 的条目：同一事务先删这次加载的旧明细，再一条 INSERT 整批写入。
// resume 重跑加载时（开关可能已变、策略不同）明细只反映最后一次加载，不留旧策略的行。
// 全部行须属于 scope 且同一 (run_id, load_seq)；字段越界或取值非法整批拒绝。
func (r KBLoadItemRepo) ReplaceKBLoadItems(ctx context.Context, scope model.Scope, items []model.KBLoadItem) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	runID, seq := items[0].RunID, items[0].LoadSeq
	var sb strings.Builder
	sb.WriteString(`INSERT INTO kb_load_item (` + kbLoadItemCols + `) VALUES `)
	args := make([]any, 0, len(items)*11)
	for i, it := range items {
		if err := checkKBLoadItem(scope, it); err != nil {
			return fmt.Errorf("repository: kb_load_item run %d rank %d: %w", it.RunID, it.RankNo, err)
		}
		if it.RunID != runID || it.LoadSeq != seq {
			return fmt.Errorf("repository: kb_load_item batch mixes (run %d, seq %d) with (run %d, seq %d)", runID, seq, it.RunID, it.LoadSeq)
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, it.TenantID, it.FarmID, it.RunID, it.LoadSeq, it.RankNo, it.QuestionType, it.KBID, it.KBVersion,
			it.PickReason, it.Strategy, it.CreatedAt)
	}
	return r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM kb_load_item WHERE tenant_id = ? AND farm_id = ? AND run_id = ? AND load_seq = ?`,
			scope.TenantID, scope.FarmID, runID, seq).Error; err != nil {
			return fmt.Errorf("repository: clear kb_load_item run %d seq %d: %w", runID, seq, err)
		}
		if err := tx.Exec(sb.String(), args...).Error; err != nil {
			return fmt.Errorf("repository: insert kb_load_item run %d seq %d: %w", runID, seq, err)
		}
		return nil
	})
}

func checkKBLoadItem(scope model.Scope, it model.KBLoadItem) error {
	switch {
	case it.TenantID != scope.TenantID || it.FarmID != scope.FarmID:
		return fmt.Errorf("%w: item scope (%d,%d) != (%d,%d)", ErrScopeRequired, it.TenantID, it.FarmID, scope.TenantID, scope.FarmID)
	case it.RunID == 0:
		return errors.New("run_id is 0")
	case it.LoadSeq < 0 || it.RankNo < 0:
		return fmt.Errorf("load_seq %d / rank_no %d negative", it.LoadSeq, it.RankNo)
	case it.QuestionType == "" || len(it.QuestionType) > kbLoadQuestionTypeMax:
		return fmt.Errorf("question_type %q: want 1-%d bytes", it.QuestionType, kbLoadQuestionTypeMax)
	case it.KBID == "" || len(it.KBID) > kbLoadKBIDMax:
		return fmt.Errorf("kb_id %q: want 1-%d bytes", it.KBID, kbLoadKBIDMax)
	case it.KBVersion == "" || len(it.KBVersion) > kbLoadVersionMax:
		return fmt.Errorf("kb_version %q: want 1-%d bytes", it.KBVersion, kbLoadVersionMax)
	case it.Strategy == "" || len(it.Strategy) > kbLoadStrategyMax:
		return fmt.Errorf("strategy %q: want 1-%d bytes", it.Strategy, kbLoadStrategyMax)
	case it.CreatedAt.IsZero():
		return errors.New("created_at is zero")
	}
	switch it.PickReason {
	case model.PickReasonMustInclude, model.PickReasonRanked, model.PickReasonCommon:
		return nil
	}
	return fmt.Errorf("pick_reason %q unknown", it.PickReason)
}

// ListKBLoadItems 读一个 run 的加载明细（按 load_seq、rank_no 升序；run 详情与测试用）。
func (r KBLoadItemRepo) ListKBLoadItems(ctx context.Context, scope model.Scope, runID uint64) ([]model.KBLoadItem, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []kbLoadItemRow
	if err := r.DB.WithContext(ctx).Raw(`SELECT id, `+kbLoadItemCols+` FROM kb_load_item
 WHERE tenant_id = ? AND farm_id = ? AND run_id = ? ORDER BY load_seq, rank_no`,
		scope.TenantID, scope.FarmID, runID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list kb_load_item run %d: %w", runID, err)
	}
	out := make([]model.KBLoadItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.KBLoadItem(row))
	}
	return out, nil
}

// kbLoadItemRow 是 kb_load_item 的扫描行：列名显式标注（KBID 按 GORM 默认命名会映射成 kbid）。
type kbLoadItemRow struct {
	ID           uint64    `gorm:"column:id"`
	TenantID     uint64    `gorm:"column:tenant_id"`
	FarmID       uint64    `gorm:"column:farm_id"`
	RunID        uint64    `gorm:"column:run_id"`
	LoadSeq      int       `gorm:"column:load_seq"`
	RankNo       int       `gorm:"column:rank_no"`
	QuestionType string    `gorm:"column:question_type"`
	KBID         string    `gorm:"column:kb_id"`
	KBVersion    string    `gorm:"column:kb_version"`
	PickReason   string    `gorm:"column:pick_reason"`
	Strategy     string    `gorm:"column:strategy"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// MetricOnlineLoadPrecision 是线上加载精度的指标名（交 L3 quality-metric-rollup 写 quality_metric_daily）。
const MetricOnlineLoadPrecision = "kb_load_precision"

// OnlineLoadPrecision 照抄 §6.6 线上加载精度 SQL（单农场 [from, to)）：RANKED 条目中被本 run 建议卡引用的比例。
// 分母为 0（当日无 RANKED 加载）时 ok=false。
func (r KBLoadItemRepo) OnlineLoadPrecision(ctx context.Context, scope model.Scope, from, to time.Time) (value float64, ok bool, err error) {
	if err := requireScope(scope); err != nil {
		return 0, false, err
	}
	var v sql.NullFloat64
	if err := r.DB.WithContext(ctx).Raw(`SELECT COUNT(DISTINCT CASE WHEN c.id IS NOT NULL THEN k.id END) / COUNT(DISTINCT k.id) FROM kb_load_item k
LEFT JOIN advice_card a ON a.tenant_id = k.tenant_id AND a.farm_id = k.farm_id AND a.run_id = k.run_id
LEFT JOIN advice_citation c ON c.tenant_id = a.tenant_id AND c.farm_id = a.farm_id AND c.advice_id = a.id
  AND c.kb_id = k.kb_id AND c.kb_version = k.kb_version
WHERE k.tenant_id = ? AND k.farm_id = ? AND k.created_at >= ? AND k.created_at < ? AND k.pick_reason = 'RANKED'`,
		scope.TenantID, scope.FarmID, from, to).Scan(&v).Error; err != nil {
		return 0, false, fmt.Errorf("repository: kb_load precision (%d,%d): %w", scope.TenantID, scope.FarmID, err)
	}
	return v.Float64, v.Valid, nil
}

// KBLoadFarms 列出有早于 before 明细的农场（保留期清理逐农场执行；系统级）。按 (tenant_id, farm_id) 分组取
// MIN(created_at)：idx_scope_time 上每组只读首行（松散索引扫描），不随明细行数全量扫描。
func (r KBLoadItemRepo) KBLoadFarms(ctx context.Context, sys SystemScope, before time.Time) ([]model.Scope, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	// 扫描到只含两列的行结构：model.Scope 带 Roles 切片，GORM 解析其 schema 会报 unsupported data type。
	var rows []struct {
		TenantID uint64 `gorm:"column:tenant_id"`
		FarmID   uint64 `gorm:"column:farm_id"`
	}
	if err := r.DB.WithContext(ctx).Raw(`SELECT tenant_id, farm_id FROM kb_load_item GROUP BY tenant_id, farm_id HAVING MIN(created_at) < ?
 ORDER BY tenant_id, farm_id`,
		before).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list kb_load_item farms: %w", err)
	}
	out := make([]model.Scope, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.Scope{TenantID: row.TenantID, FarmID: row.FarmID})
	}
	return out, nil
}

// PurgeKBLoadItems 照抄 §6.6 保留期清理 SQL：删本农场早于 before 的明细，单次至多 KBLoadPurgeBatch 行。
func (r KBLoadItemRepo) PurgeKBLoadItems(ctx context.Context, scope model.Scope, before time.Time) (int64, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	res := r.DB.WithContext(ctx).Exec(`DELETE FROM kb_load_item WHERE tenant_id = ? AND farm_id = ? AND created_at < ? LIMIT 5000`,
		scope.TenantID, scope.FarmID, before)
	if res.Error != nil {
		return 0, fmt.Errorf("repository: purge kb_load_item (%d,%d): %w", scope.TenantID, scope.FarmID, res.Error)
	}
	return res.RowsAffected, nil
}
