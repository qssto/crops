package reduce

import (
	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// SummaryStatus 是指标汇总状态（口径表 §汇总）。
type SummaryStatus string

const (
	// StatusOK：正常出数字。
	StatusOK SummaryStatus = model.MetricStatusOK
	// StatusNoData：无适用 trial（分母为 0）。
	StatusNoData SummaryStatus = model.MetricStatusNoData
	// StatusUndeterminable：judge UNKNOWN 占比超过 MaxUnknownShare，标「不可判」（数字照出、仅供参考）。
	StatusUndeterminable SummaryStatus = model.MetricStatusUndeterminable
	// StatusVoid：存在 grader_error，整指标作废、不出数字。
	StatusVoid SummaryStatus = model.MetricStatusVoid
)

// IntervalKind 是区间算法。
type IntervalKind string

const (
	// IntervalWilson：红线（样本级 0/1）按样本 Wilson。
	IntervalWilson IntervalKind = "wilson"
	// IntervalBootstrap：非红线（样本级均值）按样本 bootstrap。
	IntervalBootstrap IntervalKind = "bootstrap"
)

// MaxUnknownShare 是 UNKNOWN 占比上限：超过即「不可判」。
const MaxUnknownShare = 0.10

// Summary 是一个指标在一组 trial 上的汇总（可比较：同输入同 seed 必相等）。
// Numerator / Denominator 是 trial 级计数；Point / Low / High 是样本级估计与 95% 区间。
type Summary struct {
	Metric       string
	Title        string
	Status       SummaryStatus
	Numerator    int
	Denominator  int
	InfraErrors  int
	GraderErrors int
	Unknown      int
	UnknownShare float64
	Samples      int
	Point        float64
	Low, High    float64
	Interval     IntervalKind
}

// Summarize 汇总一个指标：trial 级归类计数，样本级取点估计与区间（红线 Wilson，非红线 bootstrap，seed / resamples 进报告）。
// 有 grader_error 时只留出错计数、不出数字；UNKNOWN 不进分子分母，占比 = UNKNOWN ÷（分母 + UNKNOWN）。
func Summarize(m QualityMetric, trials []evalkit.Trial, seed uint64, resamples int) Summary {
	s := Summary{Metric: m.Name(), Title: m.Title(), Interval: IntervalBootstrap}
	if m.RedLine() {
		s.Interval = IntervalWilson
	}
	for _, t := range trials {
		switch r := m.Classify(t); r.Kind {
		case KindCounted:
			s.Denominator++
			if r.Value == 1 {
				s.Numerator++
			}
		case KindInfraError:
			s.InfraErrors++
		case KindGraderError:
			s.GraderErrors++
		case KindUnknown:
			s.Unknown++
		}
	}
	if s.GraderErrors > 0 {
		return Summary{Metric: s.Metric, Title: s.Title, Status: StatusVoid, Interval: s.Interval,
			InfraErrors: s.InfraErrors, GraderErrors: s.GraderErrors, Unknown: s.Unknown}
	}
	if s.Denominator+s.Unknown > 0 {
		s.UnknownShare = float64(s.Unknown) / float64(s.Denominator+s.Unknown)
	}
	if s.Denominator == 0 {
		s.Status = StatusNoData
		return s
	}
	values := SampleValues(m, trials)
	xs := make([]float64, len(values))
	passed := 0
	for i, v := range values {
		xs[i] = v.Value
		if v.Value == 1 {
			passed++
		}
	}
	s.Samples = len(xs)
	if s.Samples > 0 {
		s.Point = meanOf(xs)
	}
	if m.RedLine() {
		s.Low, s.High = Wilson(passed, s.Samples)
	} else {
		s.Low, s.High = BootstrapMean(xs, resamples, seed)
	}
	s.Status = StatusOK
	if s.UnknownShare > MaxUnknownShare {
		s.Status = StatusUndeterminable
	}
	return s
}
