package quality

import (
	"context"
	"fmt"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service/quality/judge"
)

// quality-metric-rollup（§6.4 技术实现 2）：单农场一日的线上汇总写农场行，再由库里的农场行重算平台行
// （tenant_id=0, farm_id=0）。同键补跑覆盖（ON DUPLICATE KEY UPDATE）；不写 run_event。

// Rollup 汇总线上指标。
type Rollup struct {
	Store   RollupStore
	Version string // 事实一致率只计当前 judge 版本的结果
}

// FarmStat 是单农场一日的汇总（日报与日志用）。
type FarmStat struct {
	Counts AdviceCounts
	Judge  JudgeCounts
	Fact   FactStatus
	Rows   []model.QualityDaily
}

// Farm 汇总单农场一日并写库；unjudged 为该日抽样有卡未评上。
func (r Rollup) Farm(ctx context.Context, sys repository.SystemScope, scope model.Scope, day Day, unjudged bool,
	at time.Time) (FarmStat, error) {
	c, err := r.counts(ctx, scope, day)
	if err != nil {
		return FarmStat{}, err
	}
	t, err := r.Store.FarmJudgeTally(ctx, scope, day.Date, judge.DimFactConsistency, r.Version, judgeTopScore)
	if err != nil {
		return FarmStat{}, err
	}
	j := JudgeCounts(t)
	st := FarmStat{Counts: c, Judge: j, Fact: FactState(j, unjudged)}
	st.Rows = OnlineRows(scope.TenantID, scope.FarmID, day.Date, c, j, st.Fact)
	if err := r.Store.UpsertDaily(ctx, sys, st.Rows, at); err != nil {
		return FarmStat{}, err
	}
	return st, nil
}

func (r Rollup) counts(ctx context.Context, scope model.Scope, day Day) (AdviceCounts, error) {
	rows, err := r.Store.AdviceOutcomeCounts(ctx, scope, day.From, day.To)
	if err != nil {
		return AdviceCounts{}, err
	}
	var c AdviceCounts
	for _, o := range rows {
		c.Total += o.N
		switch model.Outcome(o.Outcome) {
		case model.OutcomeAdvice:
			c.Advice += o.N
		case model.OutcomeRefused:
			c.Refused += o.N
			if o.RefuseReason != nil && *o.RefuseReason == RefuseValidation {
				c.ValidationFailed += o.N
			}
		}
		if o.Degraded {
			c.Degraded += o.N
		}
	}
	if c.Advice > 0 {
		if c.Cited, err = r.Store.CitedAdviceCount(ctx, scope, day.From, day.To); err != nil {
			return AdviceCounts{}, err
		}
	}
	return c, nil
}

// Platform 由库里该日全部农场的 ONLINE all 行重算平台行；事实一致率按全部农场的 judge 结果重算，unjudged 为任一农场未评。
// extra 是同批写入的平台行（离线导入），返回平台事实一致率状态。
func (r Rollup) Platform(ctx context.Context, sys repository.SystemScope, date string, unjudged bool,
	extra []model.QualityDaily, at time.Time) (FactStatus, error) {
	totals, err := r.Store.OnlineFarmTotals(ctx, sys, date)
	if err != nil {
		return "", err
	}
	rows := append([]model.QualityDaily(nil), extra...)
	for _, t := range totals {
		if t.Metric == model.MetricFactConsistency {
			continue // 平台事实一致率按结果行重算（含 UNKNOWN 占比），不由农场行相加
		}
		if row, ok := ratioRow(0, 0, date, t.Metric, t.Numerator, t.Denominator, false); ok {
			rows = append(rows, row)
		}
	}
	tally, err := r.Store.PlatformJudgeTally(ctx, sys, date, judge.DimFactConsistency, r.Version, judgeTopScore)
	if err != nil {
		return "", err
	}
	j := JudgeCounts(tally)
	fact := FactState(j, unjudged)
	if fact == FactOK {
		if row, ok := ratioRow(0, 0, date, model.MetricFactConsistency, j.Top, j.Scored, true); ok {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return fact, nil
	}
	if err := r.Store.UpsertDaily(ctx, sys, rows, at); err != nil {
		return "", fmt.Errorf("quality: platform rows %s: %w", date, err)
	}
	return fact, nil
}
