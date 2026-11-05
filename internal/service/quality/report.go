package quality

import (
	"fmt"
	"math"
	"strconv"

	"agri-steward/internal/model"
	"agri-steward/internal/service/quality/stat"
)

// 日报（§6.4 功能任务 5-6、技术实现 4）：离线（最近一次夜间全量）与线上两栏并列，给点估计、区间、相对基线差值；
// 离线切片表做 Holm 校正后才标红；门禁只看 all 切片（日报不做门禁判定）。只含 ID 与聚合值。

// HolmAlpha 是切片标红的族错误率。
const HolmAlpha = 0.05

// 日报单元格文案。
const (
	TextVoid           = "本期作废"
	TextNoData         = "无数据"
	TextUndeterminable = "不可判"
	TextUnjudged       = "未评"
	TextUncalibrated   = "未校准"
	TextNoDiff         = "—"
	TextEstimate       = "抽样估计"
)

// reportMetrics 是日报行序（口径表六指标 + 只出报告的引用正确率 + 只有线上值的拒答率）。
var reportMetrics = []string{model.MetricCitationCoverage, "citation_correctness", model.MetricNumericAccuracy,
	"refusal_accuracy", "over_conservative_rate", model.MetricFactConsistency, "hallucination_rate", model.MetricRefusalRate}

// metricTitles 是离线 metrics.json 缺行时的中文名（有行时取 metrics.json 的 title）。
var metricTitles = map[string]string{
	model.MetricCitationCoverage: "引用覆盖率", "citation_correctness": "引用正确率", model.MetricNumericAccuracy: "数值准确率",
	"refusal_accuracy": "拒答正确率", "over_conservative_rate": "过度保守率", model.MetricFactConsistency: "事实一致率",
	"hallucination_rate": "幻觉率", model.MetricRefusalRate: "拒答率（线上）",
}

// ReportInput 是生成一份日报的全部数据（job 组装，Build 为纯函数便于测试）。
type ReportInput struct {
	Date         string // 报告日 yyyy-mm-dd（report_timezone 下的前一日）
	Offline      Offline
	Baseline     *model.OfflineMetrics // nil 表示基线未配置或不存在：差值「—」
	BaselineName string
	Online       []model.QualityDaily // 平台 ONLINE 行（tenant_id=0, farm_id=0）
	OnlineFact   FactStatus
	OnlineRuns   map[string][]uint64 // 指标 → 下钻 run_id（未覆盖 / 拒收 / 拒答 / 待专家）
}

// Cell 是日报一个单元格。Text 为点估计或状态文案；Link 非空时 Text 可点开。
type Cell struct {
	Text  string
	CI    string
	Diff  string
	Note  string
	Link  string
	Runs  []RunLink
	Red   bool
	Value bool // 有数字（非状态文案）
}

// RunLink 是线上下钻到 /runs/{run_id} 的链接。
type RunLink struct {
	RunID uint64
	Href  string
}

// MetricRow 是 all 切片一行。
type MetricRow struct {
	Metric, Title   string
	Offline, Online Cell
}

// SliceRow 是离线切片表一行；Red 为 Holm 校正后显著且变差。
type SliceRow struct {
	Metric, Title, Slice string
	Cell                 Cell
	P                    string
}

// Report 是日报视图。
type Report struct {
	Date, BaselineName string
	OfflineState       OfflineState
	ExperimentID       uint64
	ReportHref         string
	Seed               uint64
	Resamples          int
	Rows               []MetricRow
	Slices             []SliceRow
}

// RunHref 是线上 run 详情页链接。
func RunHref(runID uint64) string { return "/runs/" + strconv.FormatUint(runID, 10) }

// BuildReport 组装日报视图。
func BuildReport(in ReportInput) Report {
	rep := Report{Date: in.Date, BaselineName: in.BaselineName, OfflineState: in.Offline.State,
		ExperimentID: in.Offline.Latest.ExperimentID}
	cur := index(in.Offline.Metrics)
	base := index(in.Baseline)
	if m := in.Offline.Metrics; m != nil && in.Offline.State == OfflineOK {
		rep.ReportHref, rep.Seed, rep.Resamples = model.NightlyReportPath(m.ExperimentID), m.Seed, m.Resamples
	}
	online := map[string]model.QualityDaily{}
	for _, r := range in.Online {
		if r.Source == model.MetricSourceOnline && r.SliceKey == model.SliceAll {
			online[r.Metric] = r
		}
	}
	for _, metric := range reportMetrics {
		title := metricTitles[metric]
		if x, ok := cur[sliceKey{metric, model.SliceAll}]; ok && x.Title != "" {
			title = x.Title
		}
		row := MetricRow{Metric: metric, Title: title, Offline: offlineCell(in, cur, base, metric, model.SliceAll)}
		row.Online = onlineCell(in, online, cur, base, metric)
		rep.Rows = append(rep.Rows, row)
	}
	rep.Slices = sliceRows(in, cur, base)
	return rep
}

type sliceKey struct{ metric, slice string }

func index(m *model.OfflineMetrics) map[sliceKey]model.OfflineMetric {
	out := map[sliceKey]model.OfflineMetric{}
	if m == nil {
		return out
	}
	for _, x := range m.Metrics {
		out[sliceKey{x.Metric, x.Slice}] = x
	}
	return out
}

// uncalibrated 报告 judge 维度在当前或基线任一侧未校准（不与校准后的数据做差值）。
func uncalibrated(xs ...model.OfflineMetric) bool {
	for _, x := range xs {
		if x.Judge && !x.Calibrated {
			return true
		}
	}
	return false
}

func offlineCell(in ReportInput, cur, base map[sliceKey]model.OfflineMetric, metric, slice string) Cell {
	switch in.Offline.State {
	case OfflineVoid:
		return Cell{Text: TextVoid}
	case OfflineNoData:
		return Cell{Text: TextNoData}
	}
	x, ok := cur[sliceKey{metric, slice}]
	if !ok {
		return Cell{Text: TextNoData}
	}
	switch x.Status {
	case model.MetricStatusOK:
	case model.MetricStatusUndeterminable:
		return Cell{Text: TextUndeterminable, Note: fmt.Sprintf("UNKNOWN %.1f%%", x.UnknownShare*100)}
	case model.MetricStatusVoid:
		return Cell{Text: TextVoid}
	default:
		return Cell{Text: TextNoData}
	}
	c := Cell{Text: pct(x.Point), CI: ci(x.Low, x.High), Value: true, Link: model.NightlyReportPath(in.Offline.Metrics.ExperimentID)}
	if len(x.Failing) > 0 {
		c.Link += "#" + model.SampleAnchor(x.Failing[0])
	}
	b, ok := base[sliceKey{metric, slice}]
	switch {
	case uncalibrated(x):
		c.Diff, c.Note = TextNoDiff, TextUncalibrated
	case !ok || b.Status != model.MetricStatusOK || uncalibrated(b):
		c.Diff = TextNoDiff
	default:
		c.Diff = signed(x.Point - b.Point)
	}
	return c
}

func onlineCell(in ReportInput, online map[string]model.QualityDaily, cur, base map[sliceKey]model.OfflineMetric, metric string) Cell {
	if metric == model.MetricFactConsistency {
		switch in.OnlineFact {
		case FactUnjudged:
			return Cell{Text: TextUnjudged, Runs: runLinks(in.OnlineRuns[metric])}
		case FactUndeterminable:
			return Cell{Text: TextUndeterminable, Runs: runLinks(in.OnlineRuns[metric])}
		}
	}
	r, ok := online[metric]
	if !ok {
		return Cell{Text: TextNoData}
	}
	c := Cell{Text: pct(r.Point), Value: true, Note: fmt.Sprintf("%d/%d", r.Numerator, r.Denominator),
		Runs: runLinks(in.OnlineRuns[metric])}
	if r.Low != nil && r.High != nil {
		c.CI = ci(*r.Low, *r.High)
	}
	if r.IsEstimate {
		c.Note += " " + TextEstimate
	}
	if len(c.Runs) > 0 {
		c.Link = c.Runs[0].Href
	}
	k := sliceKey{metric, model.SliceAll}
	b, ok := base[k]
	switch {
	case uncalibrated(cur[k], b):
		c.Diff = TextUncalibrated
	case !ok || b.Status != model.MetricStatusOK:
		c.Diff = TextNoDiff
	default:
		c.Diff = signed(r.Point-b.Point) + "（参考：对离线基线）"
	}
	return c
}

func runLinks(ids []uint64) []RunLink {
	out := make([]RunLink, 0, len(ids))
	for _, id := range ids {
		out = append(out, RunLink{RunID: id, Href: RunHref(id)})
	}
	return out
}

func pct(v float64) string     { return fmt.Sprintf("%.1f%%", v*100) }
func ci(lo, hi float64) string { return fmt.Sprintf("[%.1f%%, %.1f%%]", lo*100, hi*100) }
func signed(d float64) string  { return fmt.Sprintf("%+.1fpp", d*100) }

// diffP 是当前与基线同一指标切片之差的双侧 p 值：Wilson 指标按样本数做两比例检验，bootstrap 指标按区间近似。
func diffP(x, b model.OfflineMetric) float64 {
	if x.Interval == model.IntervalWilson {
		k1, k2 := int(math.Round(x.Point*float64(x.Samples))), int(math.Round(b.Point*float64(b.Samples)))
		return stat.TwoProportionP(k1, x.Samples, k2, b.Samples)
	}
	return stat.IntervalDiffP(x.Point, x.Low, x.High, b.Point, b.Low, b.High)
}
