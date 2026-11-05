package quality

import (
	"agri-steward/internal/model"
	"agri-steward/internal/service/quality/stat"
)

// 线上口径（口径表「线上口径」）：只出 all 切片，比例类 Wilson 区间（按卡计）。
const (
	// RefuseValidation 是输出校验拒收的 refuse_reason（agent/graph RefuseValidation，service 不 import graph）。
	RefuseValidation = "VALIDATION_FAILED"
	// MaxUnknownShare 是 judge UNKNOWN 占比上限（与离线一致）：超过即「不可判」，当日不写事实一致率。
	MaxUnknownShare = 0.10
	// judgeTopScore 是事实一致率分子的分数（fact_consistency = 3）。
	judgeTopScore = 3
)

// AdviceCounts 是单农场一日的出卡计数（advice_card 汇总 + 引用覆盖查询）。
type AdviceCounts struct {
	Total            int // 全部卡（含拒答）
	Advice           int // outcome=ADVICE
	Cited            int // ADVICE 且每个结论字段都有引用
	Refused          int // outcome=REFUSED
	ValidationFailed int // REFUSED 且 refuse_reason=VALIDATION_FAILED
	Degraded         int // degraded=1（只计数，不单列指标）
}

// Add 累加（平台汇总用）。
func (c *AdviceCounts) Add(o AdviceCounts) {
	c.Total += o.Total
	c.Advice += o.Advice
	c.Cited += o.Cited
	c.Refused += o.Refused
	c.ValidationFailed += o.ValidationFailed
	c.Degraded += o.Degraded
}

// JudgeCounts 是一日事实一致率的 judge / 专家结果计数：Scored 为有分（非 UNKNOWN）的卡，Top 为 3 分的卡。
type JudgeCounts struct {
	Scored  int
	Top     int
	Unknown int
}

// Add 累加。
func (j *JudgeCounts) Add(o JudgeCounts) {
	j.Scored += o.Scored
	j.Top += o.Top
	j.Unknown += o.Unknown
}

// UnknownShare 是 UNKNOWN 占比（分母为全部抽中的卡）。
func (j JudgeCounts) UnknownShare() float64 {
	if n := j.Scored + j.Unknown; n > 0 {
		return float64(j.Unknown) / float64(n)
	}
	return 0
}

// FactStatus 是一日线上事实一致率的状态：OK 写数，其余不写（日报显示对应文案）。
type FactStatus string

// 线上事实一致率状态。
const (
	FactOK             FactStatus = "OK"
	FactUnjudged       FactStatus = "UNJUDGED"       // judge 不可用：「未评」
	FactUndeterminable FactStatus = "UNDETERMINABLE" // UNKNOWN 占比 > 10%：「不可判」
	FactNoSample       FactStatus = "NO_SAMPLE"      // 当日无抽样卡：「无数据」
)

// FactState 判定事实一致率能否出数。
func FactState(j JudgeCounts, unjudged bool) FactStatus {
	switch {
	case unjudged:
		return FactUnjudged
	case j.Scored+j.Unknown == 0:
		return FactNoSample
	case j.UnknownShare() > MaxUnknownShare:
		return FactUndeterminable
	}
	return FactOK
}

// OnlineRows 是一个 (tenant, farm, 业务日) 的线上指标行：引用覆盖率、数值准确率、拒答率，以及 fact 为 FactOK 时的
// 事实一致率（is_estimate=1）。分母为 0 的指标不写（日报显示「无数据」）。
func OnlineRows(tenantID, farmID uint64, date string, c AdviceCounts, j JudgeCounts, fact FactStatus) []model.QualityDaily {
	var out []model.QualityDaily
	add := func(metric string, k, n int, estimate bool) {
		if r, ok := ratioRow(tenantID, farmID, date, metric, k, n, estimate); ok {
			out = append(out, r)
		}
	}
	add(model.MetricCitationCoverage, c.Cited, c.Advice, false)
	// 数值准确率线上代理：1 − VALIDATION_FAILED 拒收占比，分母为出卡与被拒收的卡（其余拒答没有数值可判）。
	add(model.MetricNumericAccuracy, c.Advice, c.Advice+c.ValidationFailed, false)
	add(model.MetricRefusalRate, c.Refused, c.Total, false)
	if fact == FactOK {
		add(model.MetricFactConsistency, j.Top, j.Scored, true)
	}
	return out
}

// ratioRow 是一行线上比例指标（Wilson 区间，按卡计）；分母为 0 时 ok 为 false（不写）。
func ratioRow(tenantID, farmID uint64, date, metric string, k, n int, estimate bool) (model.QualityDaily, bool) {
	if n <= 0 {
		return model.QualityDaily{}, false
	}
	lo, hi := stat.Wilson(k, n)
	return model.QualityDaily{TenantID: tenantID, FarmID: farmID, StatDate: date, Source: model.MetricSourceOnline,
		Metric: metric, SliceKey: model.SliceAll, Numerator: k, Denominator: n, Point: round5(float64(k) / float64(n)),
		Low: ptr(round5(lo)), High: ptr(round5(hi)), IsEstimate: estimate}, true
}

// round5 对齐 decimal(7,5) 列，避免写库截断与回读不一致。
func round5(v float64) float64 {
	const scale = 1e5
	if v < 0 {
		return -float64(int64(-v*scale+0.5)) / scale
	}
	return float64(int64(v*scale+0.5)) / scale
}

func ptr[T any](v T) *T { return &v }
