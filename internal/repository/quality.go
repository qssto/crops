package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// QualityRepo 是 P2 L3 可信度指标的读写（§6.4 ② 定版 SQL）：advice_card / advice_citation 汇总、quality_metric_daily、
// online_judge_result。不写 run_event（母文档 §4.6.2-7）。
type QualityRepo struct{ DB *gorm.DB }

// FarmTZ 是 farm_profile 的农场与时区（rollup 逐农场切业务日）。
type FarmTZ struct {
	TenantID uint64
	FarmID   uint64
	Timezone string
}

// FarmTimezones 返回全部农场及时区（按 tenant_id, farm_id 升序，系统级）。
func (r QualityRepo) FarmTimezones(ctx context.Context, sys SystemScope) ([]FarmTZ, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []FarmTZ
	if err := r.DB.WithContext(ctx).Raw(`SELECT tenant_id, farm_id, timezone FROM farm_profile ORDER BY tenant_id, farm_id`).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list farm timezones: %w", err)
	}
	return rows, nil
}

// OutcomeCount 是单农场一日按 (outcome, refuse_reason, degraded) 的出卡计数。
type OutcomeCount struct {
	Outcome      string
	RefuseReason *string
	Degraded     bool
	N            int
}

// AdviceOutcomeCounts 是线上汇总（单农场一日，走 idx_scope_time），SQL 照抄 §6.4 ②。
func (r QualityRepo) AdviceOutcomeCounts(ctx context.Context, scope model.Scope, from, to time.Time) ([]OutcomeCount, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []OutcomeCount
	if err := r.DB.WithContext(ctx).Raw(`SELECT outcome, refuse_reason, degraded, COUNT(*) AS n FROM advice_card
WHERE tenant_id = ? AND farm_id = ? AND created_at >= ? AND created_at < ?
GROUP BY outcome, refuse_reason, degraded`, scope.TenantID, scope.FarmID, from.UTC(), to.UTC()).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: advice outcome counts farm %d: %w", scope.FarmID, err)
	}
	return rows, nil
}

// citedCond 是「出卡的每个结论字段（conclusion、reasons[i]、numbers[i]）都有引用」：不同 claim_path（空记 conclusion）
// 的个数不少于字段数。指向不存在字段的引用在出卡前已被 output_validator 拒收，故个数比较即覆盖判定（线上口径见口径表）。
const citedCond = `(SELECT COUNT(DISTINCT IF(a.claim_path = '', 'conclusion', a.claim_path)) FROM advice_citation a
 WHERE a.tenant_id = c.tenant_id AND a.farm_id = c.farm_id AND a.advice_id = c.id)
 >= 1 + JSON_LENGTH(c.reasons) + COALESCE(JSON_LENGTH(c.numbers), 0)`

// CitedAdviceCount 是单农场一日 outcome=ADVICE 且每个结论字段都有引用的卡数（引用覆盖率线上分子）。
func (r QualityRepo) CitedAdviceCount(ctx context.Context, scope model.Scope, from, to time.Time) (int, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	var n int
	if err := r.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM advice_card c
WHERE c.tenant_id = ? AND c.farm_id = ? AND c.created_at >= ? AND c.created_at < ? AND c.outcome = 'ADVICE' AND `+citedCond,
		scope.TenantID, scope.FarmID, from.UTC(), to.UTC()).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("repository: cited advice count farm %d: %w", scope.FarmID, err)
	}
	return n, nil
}

// RunFilter 是日报线上下钻要列出的 run（固定 SQL 片段，不拼接外部输入）。
type RunFilter int

// 下钻过滤条件。
const (
	RunsUncited          RunFilter = iota + 1 // 出卡但有结论字段没有引用
	RunsValidationFailed                      // VALIDATION_FAILED 拒收
	RunsRefused                               // 全部拒答
)

var runFilterSQL = map[RunFilter]string{
	RunsUncited:          `c.outcome = 'ADVICE' AND NOT ` + citedCond,
	RunsValidationFailed: `c.outcome = 'REFUSED' AND c.refuse_reason = 'VALIDATION_FAILED'`,
	RunsRefused:          `c.outcome = 'REFUSED'`,
}

// AdviceRuns 列单农场一日满足 f 的 run_id（按 advice id 升序，最多 limit 个）。
func (r QualityRepo) AdviceRuns(ctx context.Context, scope model.Scope, from, to time.Time, f RunFilter, limit int) ([]uint64, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	cond, ok := runFilterSQL[f]
	if !ok {
		return nil, fmt.Errorf("repository: unknown run filter %d", f)
	}
	var ids []uint64
	if err := r.DB.WithContext(ctx).Raw(`SELECT c.run_id FROM advice_card c
WHERE c.tenant_id = ? AND c.farm_id = ? AND c.created_at >= ? AND c.created_at < ? AND `+cond+` ORDER BY c.id LIMIT ?`,
		scope.TenantID, scope.FarmID, from.UTC(), to.UTC(), limit).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: advice runs farm %d: %w", scope.FarmID, err)
	}
	return ids, nil
}

// upsertDailySQL 照抄 §6.4 ②；更新列另加 is_estimate / experiment_id（补跑导入了更新的夜间全量时，实验号随数字一起覆盖）。
const upsertDailySQL = `INSERT INTO quality_metric_daily (tenant_id, farm_id, stat_date, source, metric, slice_key, numerator, denominator, point, ci_low, ci_high, is_estimate, experiment_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE numerator = VALUES(numerator), denominator = VALUES(denominator),
  point = VALUES(point), ci_low = VALUES(ci_low), ci_high = VALUES(ci_high), is_estimate = VALUES(is_estimate), experiment_id = VALUES(experiment_id)`

// UpsertDaily 在一个事务内写入一批日指标行（农场行与 tenant_id=0, farm_id=0 平台行，系统级）；同键补跑覆盖。
func (r QualityRepo) UpsertDaily(ctx context.Context, sys SystemScope, rows []model.QualityDaily, at time.Time) error {
	if err := requireSystem(sys); err != nil {
		return err
	}
	return r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, q := range rows {
			if _, err := execInTx(ctx, tx, upsertDailySQL, q.TenantID, q.FarmID, q.StatDate, q.Source, q.Metric, q.SliceKey,
				q.Numerator, q.Denominator, q.Point, q.Low, q.High, q.IsEstimate, nullIfZero(q.ExperimentID), at.UTC()); err != nil {
				return fmt.Errorf("repository: upsert quality_metric_daily %d/%d %s %s %s: %w",
					q.TenantID, q.FarmID, q.StatDate, q.Metric, q.SliceKey, err)
			}
		}
		return nil
	})
}

func nullIfZero(v uint64) any {
	if v == 0 {
		return nil
	}
	return v
}

type dailyRow struct {
	TenantID     uint64
	FarmID       uint64
	StatDate     time.Time
	Source       string
	Metric       string
	SliceKey     string
	Numerator    int
	Denominator  int
	Point        float64
	CILow        *float64
	CIHigh       *float64
	IsEstimate   bool
	ExperimentID *uint64
}

// DailyRows 读一个业务日的日指标行（tenantID=0, farmID=0 为平台行），按 source, metric, slice_key 排序。
func (r QualityRepo) DailyRows(ctx context.Context, sys SystemScope, tenantID, farmID uint64, statDate string) ([]model.QualityDaily, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []dailyRow
	if err := r.DB.WithContext(ctx).Raw(`SELECT tenant_id, farm_id, stat_date, source, metric, slice_key, numerator, denominator, point, ci_low, ci_high, is_estimate, experiment_id
FROM quality_metric_daily WHERE tenant_id = ? AND farm_id = ? AND stat_date = ? ORDER BY source, metric, slice_key`,
		tenantID, farmID, statDate).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list quality_metric_daily %d/%d %s: %w", tenantID, farmID, statDate, err)
	}
	out := make([]model.QualityDaily, 0, len(rows))
	for _, d := range rows {
		q := model.QualityDaily{TenantID: d.TenantID, FarmID: d.FarmID, StatDate: d.StatDate.Format(time.DateOnly),
			Source: d.Source, Metric: d.Metric, SliceKey: d.SliceKey, Numerator: d.Numerator, Denominator: d.Denominator,
			Point: d.Point, Low: d.CILow, High: d.CIHigh, IsEstimate: d.IsEstimate}
		if d.ExperimentID != nil {
			q.ExperimentID = *d.ExperimentID
		}
		out = append(out, q)
	}
	return out, nil
}

// MetricTotal 是一个指标全部农场行的分子 / 分母合计。
type MetricTotal struct {
	Metric      string
	Numerator   int
	Denominator int
}

// OnlineFarmTotals 汇总一个业务日全部农场的 ONLINE all 切片行（不含平台行），按 metric 排序（平台汇总行用，系统级）。
// 平台行由库里的农场行重算：各农场时区不同、分批到达前一日时，补跑也得到同一合计。
func (r QualityRepo) OnlineFarmTotals(ctx context.Context, sys SystemScope, statDate string) ([]MetricTotal, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []MetricTotal
	if err := r.DB.WithContext(ctx).Raw(`SELECT metric, SUM(numerator) AS numerator, SUM(denominator) AS denominator
FROM quality_metric_daily WHERE stat_date = ? AND source = 'ONLINE' AND slice_key = 'all' AND NOT (tenant_id = 0 AND farm_id = 0)
GROUP BY metric ORDER BY metric`, statDate).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: online farm totals %s: %w", statDate, err)
	}
	return rows, nil
}
