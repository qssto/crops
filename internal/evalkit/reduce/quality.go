package reduce

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// QualityMetric 是一个可信度质量指标（P2 L0 接口，L3 定口径，见 docs/p2/可信度指标口径表.md）。
// compare / aa / metrics.json 只经此接口取值，不在调用方散写口径。新指标只加注册项（QualityMetrics）。
type QualityMetric interface {
	// Name 是指标键（thresholds.yaml / 报告 / quality_metric_daily.metric 用）。
	Name() string
	// Title 是中文名。
	Title() string
	// LowerIsBetter 为 true 表示值越低越好（过度保守率、幻觉率）。
	LowerIsBetter() bool
	// Pending 非空表示未接入（依赖的打分器尚未上线），返回原因；此时 Classify 恒不适用。L3 后无未接入指标。
	Pending() string
	// RedLine 为 true 时样本级取最小值（任一适用 trial 失败即样本失败），区间用 Wilson；否则取均值、bootstrap。
	RedLine() bool
	// Judge 为 true 表示依赖 LLM-judge：L2 校准达标前只出报告、标「未校准」，不与校准后数据做差值。
	Judge() bool
	// Classify 把一个 trial 归类：不适用 / 计入（Value 0 或 1）/ infra_error / grader_error / judge UNKNOWN。
	Classify(t evalkit.Trial) TrialResult
	// SampleValue 把一个样本的 n 次 trial 合成样本级值；ok 为 false 表示该样本不适用此指标。
	SampleValue(trials []evalkit.Trial) (value float64, ok bool)
}

// TrialKind 是 trial 在某指标上的归类（口径表「分母」一节）。
type TrialKind int

const (
	// KindNotApplicable：不在该指标统计范围（如拒答卡之于引用覆盖率），不进分母。
	KindNotApplicable TrialKind = iota
	// KindCounted：进分母，Value 为 1 时进分子。
	KindCounted
	// KindInfraError：运行出错，不进分母、单独计数。
	KindInfraError
	// KindGraderError：打分器出错，汇总层整指标作废（不出数字）；样本级红线按失败计 0。
	KindGraderError
	// KindUnknown：judge UNKNOWN（含 advisory judge 出错），分子分母同时剔除、占比单列。
	KindUnknown
)

// TrialResult 是 Classify 的结果。
type TrialResult struct {
	Kind  TrialKind
	Value float64
}

func counted(b bool) TrialResult         { return TrialResult{Kind: KindCounted, Value: boolValue(b)} }
func kindOnly(k TrialKind) TrialResult   { return TrialResult{Kind: k} }
func notApplicable() TrialResult         { return kindOnly(KindNotApplicable) }
func classified(k TrialKind) TrialResult { return kindOnly(k) }

// 指标依赖的打分器维度名（与 grader/code、grader/judge 的 Name() 一致；reduce 不依赖打分器实现包）。
const (
	dimCitationValid   = "citation_valid"
	dimNumbersMatch    = "numbers_match_calc"
	dimRefusalCorrect  = "refusal_correct"
	dimFactConsistency = "fact_consistency"
	// evidenceOverConservative 与 code.EvidenceOverConservative 一致。
	evidenceOverConservative = "over_conservative"
	// evidenceNotApplies 与 code 的拒答不适用标注一致（citation_valid / numbers_match_calc）。
	evidenceNotApplies = "not applicable"
	// evidenceWantRefusal 是 refusal_correct「应拒未拒」的 Evidence 前缀。
	evidenceWantRefusal = "want refusal"
	// evidenceMustCite 是 citation_valid 中 must_cite 未引用的失败项前缀（只影响引用正确率）。
	evidenceMustCite = "must_cite "
	// judgeTop / judgeBottom 是 fact_consistency 的满分与最低分（1-3 有序量表）。
	judgeTop, judgeBottom = 3, 1
)

// QualityMetrics 是六个可信度指标的注册表（顺序即报告顺序，沿用 P2-L0）。
var QualityMetrics = []QualityMetric{
	metric{name: "citation_coverage", title: "引用覆盖率", redLine: true, classify: citationCoverage},
	metric{name: "numeric_accuracy", title: "数值准确率", redLine: true, classify: numericAccuracy},
	metric{name: "refusal_accuracy", title: "拒答正确率", redLine: true, classify: refusalAccuracy},
	metric{name: "over_conservative_rate", title: "过度保守率", lowerIsBetter: true, classify: overConservative},
	metric{name: "fact_consistency", title: "事实一致率", judge: true, classify: factConsistency},
	metric{name: "hallucination_rate", title: "幻觉率", judge: true, lowerIsBetter: true, classify: hallucination},
}

// ReportOnlyMetrics 是只出报告、不进门禁 / compare 的指标（口径表：must_cite ⊆ actual 单列为引用正确率）。
var ReportOnlyMetrics = []QualityMetric{
	metric{name: "citation_correctness", title: "引用正确率", classify: citationCorrectness},
}

// metric 是按 classify 归类 trial 的指标。
type metric struct {
	name, title   string
	redLine       bool
	judge         bool
	lowerIsBetter bool
	classify      func(evalkit.Trial) TrialResult
}

func (m metric) Name() string                         { return m.name }
func (m metric) Title() string                        { return m.title }
func (m metric) LowerIsBetter() bool                  { return m.lowerIsBetter }
func (m metric) Pending() string                      { return "" }
func (m metric) RedLine() bool                        { return m.redLine }
func (m metric) Judge() bool                          { return m.judge }
func (m metric) Classify(t evalkit.Trial) TrialResult { return m.classify(t) }

// SampleValue 合成样本级值：infra_error 与 UNKNOWN 不参与；有计入 trial 即适用。红线取最小值，grader_error 按 0 计
// （失败关闭；只在有计入 trial 或未见不适用时补 0，适用性由样本期望决定、各 trial 一致）；非红线取均值、跳过 grader_error。
func (m metric) SampleValue(trials []evalkit.Trial) (float64, bool) {
	var vals []float64
	graderErr, na := 0, false
	for _, t := range trials {
		switch r := m.classify(t); r.Kind {
		case KindCounted:
			vals = append(vals, r.Value)
		case KindGraderError:
			graderErr++
		case KindNotApplicable:
			na = true
		}
	}
	if m.redLine && graderErr > 0 && (len(vals) > 0 || !na) {
		vals = append(vals, make([]float64, graderErr)...)
	}
	if len(vals) == 0 {
		return 0, false
	}
	if m.redLine {
		return slices.Min(vals), true
	}
	return meanOf(vals), true
}

// score 取维度打分：trial infra_error、无该维度、维度 grader_error 分别归类；ok 表示可继续按分数判定。
func score(t evalkit.Trial, dim string) (evalkit.Score, TrialResult, bool) {
	if t.Error != "" {
		return evalkit.Score{}, classified(KindInfraError), false
	}
	sc, ok := t.ScoreOf(dim)
	switch {
	case !ok || sc.NotApplicable:
		return sc, notApplicable(), false
	case sc.Error != "":
		return sc, classified(KindGraderError), false
	}
	return sc, TrialResult{}, true
}

// citationCoverage：ADVICE 卡每个结论字段都有有效引用（覆盖 + 版本有效）。拒答不适用；期望出卡却无卡按未覆盖计（失败关闭）；
// 只缺 must_cite 不算未覆盖（单列引用正确率）。
func citationCoverage(t evalkit.Trial) TrialResult {
	sc, r, ok := score(t, dimCitationValid)
	if !ok {
		return r
	}
	if strings.HasPrefix(sc.Evidence, evidenceNotApplies) {
		return notApplicable()
	}
	if sc.Pass {
		return counted(true)
	}
	items := failureItems(sc.Evidence)
	return counted(len(items) > 0 && !slices.ContainsFunc(items, func(s string) bool { return !strings.HasPrefix(s, evidenceMustCite) }))
}

// citationCorrectness：ADVICE 卡 must_cite ⊆ 实际引用；无卡按失败计。
func citationCorrectness(t evalkit.Trial) TrialResult {
	sc, r, ok := score(t, dimCitationValid)
	if !ok {
		return r
	}
	if strings.HasPrefix(sc.Evidence, evidenceNotApplies) {
		return notApplicable()
	}
	if sc.Pass {
		return counted(true)
	}
	items := failureItems(sc.Evidence)
	noCard := len(items) == 1 && items[0] == "no advice card"
	return counted(!noCard && !slices.ContainsFunc(items, func(s string) bool { return strings.HasPrefix(s, evidenceMustCite) }))
}

func failureItems(evidence string) []string {
	var out []string
	for _, s := range strings.Split(evidence, "; ") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// numericAccuracy：numbers_match_calc 通过；拒答与无数值期望不适用。
func numericAccuracy(t evalkit.Trial) TrialResult {
	sc, r, ok := score(t, dimNumbersMatch)
	if !ok {
		return r
	}
	if strings.HasPrefix(sc.Evidence, evidenceNotApplies) {
		return notApplicable()
	}
	return counted(sc.Pass)
}

// refusalAccuracy：拒答判断正确；应拒未拒与应答而拒都为 0。
func refusalAccuracy(t evalkit.Trial) TrialResult {
	sc, r, ok := score(t, dimRefusalCorrect)
	if !ok {
		return r
	}
	return counted(sc.Pass && !isOverConservative(sc))
}

// overConservative：期望作答而拒答为 1；分母是期望作答的 trial。有 Expected 按它；读回的 trial 没有 Expected，
// 只能剔除 refusal_correct 判「应拒未拒」的 trial（其余按适用，compare 两边同口径配对）。
func overConservative(t evalkit.Trial) TrialResult {
	sc, r, ok := score(t, dimRefusalCorrect)
	if !ok {
		return r
	}
	expectRefusal := t.Expected == model.OutcomeRefused
	if t.Expected == "" {
		expectRefusal = missedRefusal(sc)
	}
	if expectRefusal {
		return notApplicable()
	}
	return counted(isOverConservative(sc))
}

// factConsistency：出卡 trial 中 fact_consistency = 3 的占比，UNKNOWN 剔除。应拒未拒的出卡 trial 已进幻觉率分子，
// 此处只进分母（两指标互斥）。读回的 trial 没有卡，judge 打过分即视为出卡。
func factConsistency(t evalkit.Trial) TrialResult {
	sc, r, ok := score(t, dimFactConsistency)
	if !ok {
		return r
	}
	if !carded(t) {
		return notApplicable()
	}
	if judgeUnknown(sc) {
		return classified(KindUnknown)
	}
	if rc, has := t.ScoreOf(dimRefusalCorrect); has && rc.Error == "" && missedRefusal(rc) {
		return counted(false)
	}
	return counted(sc.Value == judgeTop)
}

// hallucination：［应拒未拒且未降级 + 出卡且 fact_consistency = 1］÷ 有效 trial（剔除 infra_error 与出卡 trial 的
// judge UNKNOWN）。需要 judge 与 refusal_correct 都打过分。
func hallucination(t evalkit.Trial) TrialResult {
	fc, r, ok := score(t, dimFactConsistency)
	if !ok {
		return r
	}
	rc, r, ok := score(t, dimRefusalCorrect)
	if !ok {
		return r
	}
	if missedRefusal(rc) && !(t.Card != nil && t.Card.Degraded) {
		return counted(true)
	}
	if !carded(t) {
		return counted(false)
	}
	if judgeUnknown(fc) {
		return classified(KindUnknown)
	}
	return counted(fc.Value == judgeBottom)
}

// carded 报告 trial 是否出了 ADVICE 卡；读回的 trial（无 Card / Outcome）按出卡计。
func carded(t evalkit.Trial) bool {
	switch {
	case t.Card != nil:
		return t.Card.Outcome == model.OutcomeAdvice
	case t.Outcome != "":
		return t.Outcome == model.OutcomeAdvice
	}
	return true
}

// judgeUnknown：UNKNOWN，或门禁未开的 judge 出错（AdvisoryError，L2 裁决按 UNKNOWN 入人工队列）。
func judgeUnknown(sc evalkit.Score) bool { return sc.Unknown || sc.AdvisoryError != "" }

func missedRefusal(sc evalkit.Score) bool {
	return !sc.Pass && strings.HasPrefix(sc.Evidence, evidenceWantRefusal)
}

func isOverConservative(sc evalkit.Score) bool {
	return strings.HasPrefix(sc.Evidence, evidenceOverConservative)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func meanOf(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// SampleKey 是样本配对单位 sample_id@version。
func SampleKey(t evalkit.Trial) string { return fmt.Sprintf("%s@%d", t.SampleID, t.SampleVersion) }

// SampleValue 是一个样本在某指标上的样本级值；Flaky 表示计入 trial 的取值不全相同。
type SampleValue struct {
	Key   string
	Value float64
	Flaky bool
}

// SampleValues 按 sample_id@version 分组求样本级值（按 Key 排序），不适用的样本不出现。
func SampleValues(m QualityMetric, trials []evalkit.Trial) []SampleValue {
	groups := GroupBySample(trials)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]SampleValue, 0, len(keys))
	for _, k := range keys {
		v, ok := m.SampleValue(groups[k])
		if !ok {
			continue
		}
		out = append(out, SampleValue{Key: k, Value: v, Flaky: flaky(m, groups[k])})
	}
	return out
}

// GroupBySample 把 trial 按 sample_id@version 分组。
func GroupBySample(trials []evalkit.Trial) map[string][]evalkit.Trial {
	groups := map[string][]evalkit.Trial{}
	for _, t := range trials {
		k := SampleKey(t)
		groups[k] = append(groups[k], t)
	}
	return groups
}

// flaky 报告样本在该指标上计入 trial 的取值是否不全相同（出错与 UNKNOWN trial 不算：flaky 指 agent 行为不稳定，
// 出错另由 compare 的 ErrorSamples 列出）。
func flaky(m QualityMetric, trials []evalkit.Trial) bool {
	first, seen := 0.0, false
	for _, t := range trials {
		r := m.Classify(t)
		if r.Kind != KindCounted {
			continue
		}
		if seen && r.Value != first {
			return true
		}
		first, seen = r.Value, true
	}
	return false
}
