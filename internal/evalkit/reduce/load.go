package reduce

import (
	"slices"
	"sort"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// LoadMetric 是一个离线知识加载指标（P2 L5，路线 §6.6 功能任务 1）：从 trial 的 LoadObs 取分子 / 分母。
// 独立注册表 LoadMetrics，不进 QualityMetrics（那是六个可信度指标的门禁表，归 L3）。
type LoadMetric interface {
	// Name 是指标键（报告与 L3 日报用）。
	Name() string
	// Title 是中文名。
	Title() string
	// LowerIsBetter 为 true 表示值越低越好（无关知识注入率）。
	LowerIsBetter() bool
	// Count 返回该 trial 的分子与分母；分母为 0 表示不适用。
	Count(o *evalkit.LoadObs) (num, den int)
}

// 指标键。
const (
	MetricLoadHitRate    = "kb_load_hit_rate"
	MetricLoadPrecision  = "kb_load_precision" // 与线上 repository.MetricOnlineLoadPrecision 同名同口径
	MetricLoadIrrelevant = "kb_load_irrelevant_rate"
)

// LoadMetrics 是三个加载指标（顺序即报告列序）。
var LoadMetrics = []LoadMetric{
	loadMetric{name: MetricLoadHitRate, title: "加载命中率", count: hitCount},
	loadMetric{name: MetricLoadPrecision, title: "加载精度", count: precisionCount},
	loadMetric{name: MetricLoadIrrelevant, title: "无关知识注入率", lowerIsBetter: true, count: irrelevantCount},
}

type loadMetric struct {
	name, title   string
	lowerIsBetter bool
	count         func(*evalkit.LoadObs) (int, int)
}

func (m loadMetric) Name() string                        { return m.name }
func (m loadMetric) Title() string                       { return m.title }
func (m loadMetric) LowerIsBetter() bool                 { return m.lowerIsBetter }
func (m loadMetric) Count(o *evalkit.LoadObs) (int, int) { return m.count(o) }

// hitCount：样本 must_cite 中被加载到的条数 / must_cite 条数（无 must_cite 不适用）。
func hitCount(o *evalkit.LoadObs) (int, int) {
	n := 0
	for _, ref := range o.MustCite {
		if o.Loaded(ref) {
			n++
		}
	}
	return n, len(o.MustCite)
}

// precisionCount：RANKED 条目中被卡片引用的条数 / RANKED 条数；分母排除 MUST_INCLUDE 与 COMMON
// （与线上 SQL 同口径：拒答 run 无卡片，其 RANKED 条目计入分母、不计分子）。P2 前的事件无 pick_reason，按 RANKED 计。
func precisionCount(o *evalkit.LoadObs) (int, int) {
	num, den := 0, 0
	for _, it := range o.Items {
		if it.PickReason != "" && it.PickReason != model.PickReasonRanked {
			continue
		}
		den++
		if it.Cited {
			num++
		}
	}
	return num, den
}

// irrelevantCount：与样本标注情境不匹配的加载条目数 / 全部加载条目数。
func irrelevantCount(o *evalkit.LoadObs) (int, int) {
	n := 0
	for _, it := range o.Items {
		if it.Irrelevant {
			n++
		}
	}
	return n, len(o.Items)
}

// LoadRow 是一个切片（问题类型或合计）上各加载指标的累计比值（微平均：各 trial 分子、分母分别求和）。
type LoadRow struct {
	Key     string
	Samples int
	Trials  int
	Ratios  []Ratio // 与 LoadMetrics 同序
}

// LoadResult 是加载指标汇总。Observed 为 0 表示没有加载观测（无 trial 库或回放旧实验），报告只给说明。
type LoadResult struct {
	Observed   int
	Total      LoadRow
	ByType     []LoadRow // 按问题类型升序
	Strategies []string  // 出现过的策略版本（升序）
}

// ReduceLoad 汇总全部带 LoadObs 的 trial；运行出错（INFRA_ERROR）的 trial 不计。
func ReduceLoad(trials []evalkit.Trial) *LoadResult {
	res := &LoadResult{Total: newLoadRow("合计")}
	rows := map[string]*LoadRow{}
	samples := map[string]map[string]bool{}
	for _, t := range trials {
		if t.Load == nil || t.Error != "" {
			continue
		}
		res.Observed++
		for _, s := range t.Load.Strategies {
			if !slices.Contains(res.Strategies, s) {
				res.Strategies = append(res.Strategies, s)
			}
		}
		key := t.Load.QuestionType
		if key == "" {
			key = "-"
		}
		row, ok := rows[key]
		if !ok {
			r := newLoadRow(key)
			row = &r
			rows[key] = row
			samples[key] = map[string]bool{}
		}
		for _, r := range []*LoadRow{&res.Total, row} {
			r.Trials++
			for i, m := range LoadMetrics {
				num, den := m.Count(t.Load)
				r.Ratios[i].Num += num
				r.Ratios[i].Den += den
			}
		}
		samples[key][t.SampleID] = true
	}
	all := map[string]bool{}
	for key, row := range rows {
		row.Samples = len(samples[key])
		for id := range samples[key] {
			all[id] = true
		}
		res.ByType = append(res.ByType, *row)
	}
	res.Total.Samples = len(all)
	sort.Slice(res.ByType, func(i, j int) bool { return res.ByType[i].Key < res.ByType[j].Key })
	sort.Strings(res.Strategies)
	return res
}

func newLoadRow(key string) LoadRow {
	return LoadRow{Key: key, Ratios: make([]Ratio, len(LoadMetrics))}
}
