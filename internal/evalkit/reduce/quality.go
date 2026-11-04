package reduce

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"agri-steward/internal/evalkit"
)

// QualityMetric 是一个可信度质量指标（P2 L0，评测设计 §8.2）：名字 + 样本级取值 + 是否适用。
// compare / aa 只经此接口取值；L3 在这里定最终口径，不在调用方散写。新指标只加注册项（QualityMetrics）。
type QualityMetric interface {
	// Name 是指标键（thresholds.yaml / 报告用）。
	Name() string
	// Title 是中文名。
	Title() string
	// LowerIsBetter 为 true 表示值越低越好（如过度保守率）。
	LowerIsBetter() bool
	// Pending 非空表示未接入（依赖的打分器尚未上线），返回原因；此时 SampleValue 恒不适用。
	Pending() string
	// SampleValue 把一个样本的 n 次 trial 合成样本级值；ok 为 false 表示该样本不适用此指标。
	SampleValue(trials []evalkit.Trial) (value float64, ok bool)
}

// 指标依赖的代码打分器维度名（与 grader/code 的 Name() 一致）。
const (
	dimCitationValid  = "citation_valid"
	dimNumbersMatch   = "numbers_match_calc"
	dimRefusalCorrect = "refusal_correct"
	// evidenceOverConservative 与 code.EvidenceOverConservative 一致（reduce 不依赖打分器实现包）。
	evidenceOverConservative = "over_conservative"
	// pendingJudge 是依赖 LLM-judge 的指标在报告与阈值表里的说明。
	pendingJudge = "未接入（L2/L3）"
)

// QualityMetrics 是六个可信度指标的注册表（顺序即报告顺序）。
var QualityMetrics = []QualityMetric{
	scoreMetric{name: "citation_coverage", title: "引用覆盖率", dim: dimCitationValid, redLine: true, value: passValue},
	scoreMetric{name: "numeric_accuracy", title: "数值准确率", dim: dimNumbersMatch, redLine: true, value: passValue},
	scoreMetric{name: "refusal_accuracy", title: "拒答正确率", dim: dimRefusalCorrect, redLine: true, value: refusalCorrectValue},
	// 过度保守率暂以全部适用 trial 为分母（库里不存期望结局）；最终口径由 L3 定。
	scoreMetric{name: "over_conservative_rate", title: "过度保守率", dim: dimRefusalCorrect, lowerIsBetter: true, value: overConservativeValue},
	pendingMetric{name: "fact_consistency", title: "事实一致率"},
	pendingMetric{name: "hallucination_rate", title: "幻觉率", lowerIsBetter: true},
}

// scoreMetric 是基于一个代码打分器维度的指标：trial 值由 value 给出；
// redLine 时样本级取最小值（任一适用 trial 失败即样本失败，红线 flaky 按失败），否则取均值（flaky 照均值参与）。
// 红线指标把出错 trial（infra_error，或本维度 grader_error）按失败计 0，与「红线 flaky 按失败」一致（失败关闭）；
// 非红线指标跳过出错 trial（取值未知，不进均值）。
type scoreMetric struct {
	name, title, dim string
	redLine          bool
	lowerIsBetter    bool
	value            func(evalkit.Score) float64
}

func (m scoreMetric) Name() string        { return m.name }
func (m scoreMetric) Title() string       { return m.title }
func (m scoreMetric) LowerIsBetter() bool { return m.lowerIsBetter }
func (m scoreMetric) Pending() string     { return "" }

// SampleValue 合成样本级值。适用性：有任一正常打分的 trial 即适用；正常 trial 都是 NotApplicable（或没打该维度）即不适用
// （适用性由样本期望决定，同一样本各 trial 一致）；全部 trial 出错时适用性未知，红线指标按适用、值 0（失败关闭）。
func (m scoreMetric) SampleValue(trials []evalkit.Trial) (float64, bool) {
	o := m.outcomes(trials)
	vals := o.values
	if m.redLine && o.errored > 0 && (len(vals) > 0 || !o.notApplicable) {
		vals = append(vals, make([]float64, o.errored)...)
	}
	if len(vals) == 0 {
		return 0, false
	}
	if m.redLine {
		return slices.Min(vals), true
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals)), true
}

// TrialErrored 报告该 trial 在本维度上出错（infra_error 或 grader_error），实现 trialErrorer。
func (m scoreMetric) TrialErrored(t evalkit.Trial) bool {
	return m.outcomes([]evalkit.Trial{t}).errored > 0
}

// trialOutcomes 是一个样本的 trial 在某维度上的归类。
type trialOutcomes struct {
	// values 是正常打分且适用的 trial 取值。
	values []float64
	// errored 是 infra_error 或本维度 grader_error 的 trial 数。
	errored int
	// notApplicable 表示见过正常 trial 判该维度不适用（NotApplicable 放行或未打该维度）。
	notApplicable bool
}

func (m scoreMetric) outcomes(trials []evalkit.Trial) trialOutcomes {
	o := trialOutcomes{values: make([]float64, 0, len(trials))}
	for _, t := range trials {
		if t.Error != "" {
			o.errored++
			continue
		}
		sc, ok := t.ScoreOf(m.dim)
		switch {
		case ok && sc.Error != "":
			o.errored++
		case !ok || sc.NotApplicable:
			o.notApplicable = true
		default:
			o.values = append(o.values, m.value(sc))
		}
	}
	return o
}

// passValue：通过为 1。
func passValue(sc evalkit.Score) float64 { return boolValue(sc.Pass) }

// refusalCorrectValue：拒答判断正确为 1；应拒未拒（红线失败）与应答而拒（过度保守）都为 0。
func refusalCorrectValue(sc evalkit.Score) float64 {
	return boolValue(sc.Pass && !isOverConservative(sc))
}

// overConservativeValue：应答而拒为 1。
func overConservativeValue(sc evalkit.Score) float64 { return boolValue(isOverConservative(sc)) }

func isOverConservative(sc evalkit.Score) bool {
	return strings.HasPrefix(sc.Evidence, evidenceOverConservative)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// pendingMetric 是依赖 LLM-judge、尚未接入的指标（留注册位）。
type pendingMetric struct {
	name, title   string
	lowerIsBetter bool
}

func (m pendingMetric) Name() string                                { return m.name }
func (m pendingMetric) Title() string                               { return m.title }
func (m pendingMetric) LowerIsBetter() bool                         { return m.lowerIsBetter }
func (m pendingMetric) Pending() string                             { return pendingJudge }
func (m pendingMetric) SampleValue([]evalkit.Trial) (float64, bool) { return 0, false }

// SampleKey 是样本配对单位 sample_id@version。
func SampleKey(t evalkit.Trial) string { return fmt.Sprintf("%s@%d", t.SampleID, t.SampleVersion) }

// SampleValue 是一个样本在某指标上的样本级值；Flaky 表示适用 trial 的取值不全相同。
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

// trialErrorer 是 QualityMetric 的可选扩展：报告 trial 在该指标上是否出错。出错 trial 不算 flaky
// （flaky 指 agent 行为不稳定；出错另由 compare 的 ErrorSamples 列出）。
type trialErrorer interface {
	TrialErrored(t evalkit.Trial) bool
}

// flaky 报告样本在该指标上的适用 trial 取值是否不全相同（逐 trial 求样本值比较，跳过出错 trial）。
func flaky(m QualityMetric, trials []evalkit.Trial) bool {
	te, _ := m.(trialErrorer)
	first, seen := 0.0, false
	for i := range trials {
		if te != nil && te.TrialErrored(trials[i]) {
			continue
		}
		v, ok := m.SampleValue(trials[i : i+1])
		if !ok {
			continue
		}
		if seen && v != first {
			return true
		}
		first, seen = v, true
	}
	return false
}
