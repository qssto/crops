package gate

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"agri-steward/internal/evalkit/compare"
	qjudge "agri-steward/internal/service/quality/judge"
)

// JudgeDims 是 judge 指标 → judge 维度（reduce 的幻觉率、事实一致率都由 fact_consistency 维度导出）。
// 维度校准达标（Input.Judge[dim].Rated）时指标才进可信度退化判定；新增 judge 指标须在此登记（测试兜底）。
var JudgeDims = map[string]string{
	"fact_consistency":   "fact_consistency",
	"hallucination_rate": "fact_consistency",
}

// checkIncomplete：任一边有出错 trial，或 baseline 有样本未被 candidate 配对 → 数据不完整，不得放行（复用 compare 判定）。
func checkIncomplete(in *Input) ([]string, error) {
	r := in.Compare
	var out []string
	for _, side := range []string{"baseline", "candidate"} {
		if s := r.ErrorSamples[side]; len(s) > 0 {
			out = append(out, fmt.Sprintf("%s has infra_error / grader_error trials on %d samples: %s", side, len(s),
				strings.Join(s, " ")))
		}
	}
	if u := r.Uncovered(); len(u) > 0 {
		out = append(out, fmt.Sprintf("%d baseline samples not paired by candidate: %s", len(u), strings.Join(u, " ")))
	}
	return out, nil
}

// checkTrustRegression：all 切片（全部配对样本）上任一可信度指标退化（配对 bootstrap 区间不跨 0、方向变差）且
// 降幅 |Δ| 超过 thresholds.yaml 阈值 → 命中。judge 指标只在其维度校准达标时参与；参与判定的指标缺阈值即报错。
func checkTrustRegression(in *Input) ([]string, error) {
	var out []string
	for _, m := range in.Compare.Metrics {
		if m.Pending != "" || m.Samples == 0 || (m.NotGating && !judgeRated(in, m.Name)) {
			continue
		}
		th, ok := in.Thresholds[m.Name]
		if !ok || th == nil {
			return nil, fmt.Errorf("%w: %s", ErrNoThreshold, m.Name)
		}
		if m.Verdict == compare.VerdictRegressed && math.Abs(m.Diff.Mean) > *th {
			out = append(out, fmt.Sprintf("%s: %.3f → %.3f, Δ %+.3f [%+.3f, %+.3f] exceeds threshold %.3f", m.Name,
				m.BaselineMean, m.CandidateMean, m.Diff.Mean, m.Diff.Low, m.Diff.High, *th))
		}
	}
	return out, nil
}

// judgeRated 报告 judge 指标 name 的维度是否校准达标；未登记维度按未评（失败关闭）。
func judgeRated(in *Input, name string) bool {
	dim, ok := JudgeDims[name]
	return ok && in.Judge[dim].Rated
}

// checkNotAttributable：L0 因子检查得出多于一个差异因子 → 不可归因。
func checkNotAttributable(in *Input) ([]string, error) {
	if in.Compare.Attributable() {
		return nil, nil
	}
	names := make([]string, 0, len(in.Compare.Diffs))
	for _, d := range in.Compare.Diffs {
		names = append(names, d.Name)
	}
	return []string{fmt.Sprintf("%d factors differ: %s", len(names), strings.Join(names, ", "))}, nil
}

// checkJudgeUnrated：judges.yaml 的任一 judge 维度（全部登记维度，加上 Input.Judge 中的其他维度）未评或未达标
// （judge 不可用、judges.yaml 未开、校准未达标、缺项）→ 结论上限 NEED_SIGNOFF。
func checkJudgeUnrated(in *Input) ([]string, error) {
	var out []string
	for _, dim := range judgeDimOrder(in.Judge) {
		st, ok := in.Judge[dim]
		if ok && st.Rated {
			continue
		}
		why := st.Why
		if !ok || why == "" {
			why = "未评：judge 不可用（待真实模型 / 待专家）"
		}
		out = append(out, fmt.Sprintf("judge %s %s", dim, why))
	}
	return out, nil
}

// judgeDimOrder 是全部登记 judge 维度（qjudge.Dimensions 顺序），其后是 judge 中其余维度（字典序）。
func judgeDimOrder(judge map[string]JudgeDim) []string {
	out := slices.Clone(qjudge.Dimensions)
	var extra []string
	for d := range judge {
		if !slices.Contains(out, d) {
			extra = append(extra, d)
		}
	}
	slices.Sort(extra)
	return append(out, extra...)
}
