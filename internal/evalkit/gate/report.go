package gate

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/reduce"
)

// Direction 是翻转样本的方向。
type Direction string

// 翻转方向。
const (
	Regressed Direction = "regressed"
	Improved  Direction = "improved"
)

// flipEpsilon 以下的样本级值差视为未变（样本级值是 trial 均值，浮点误差内相等）。
const flipEpsilon = 1e-9

// Flip 是一个配对样本在一个质量指标上的样本级值变化。Key 是 sample_id@version（holdout 也只给 ID）。
type Flip struct {
	Metric              string
	Key                 string
	Baseline, Candidate float64
	Direction           Direction
}

// Flips 按 reduce.QualityMetrics 顺序、样本键排序列出两边都适用的配对样本上样本级值变了的条目（未接入指标跳过）。
// 配对复用 compare.PairedValues（in.Paired 交集，与 compare 指标计算同一口径；交集外样本不列）。
func Flips(in compare.Input) []Flip {
	var out []Flip
	for _, m := range reduce.QualityMetrics {
		if m.Pending() != "" {
			continue
		}
		for _, v := range compare.PairedValues(m, in) {
			if math.Abs(v.Candidate-v.Baseline) < flipEpsilon {
				continue
			}
			worse := v.Candidate < v.Baseline
			if m.LowerIsBetter() {
				worse = v.Candidate > v.Baseline
			}
			dir := Improved
			if worse {
				dir = Regressed
			}
			out = append(out, Flip{Metric: m.Name(), Key: v.Key, Baseline: v.Baseline, Candidate: v.Candidate, Direction: dir})
		}
	}
	return out
}

// errNoDecision 表示报告缺门禁结论或 compare 结果。
var errNoDecision = errors.New("gate: report needs decision and compare result")

// ReportInput 是配对报告的输入。Holdout 是 holdout 样本 ID 集（未声明调用只给 ID，不列工具）。
type ReportInput struct {
	Decision *Decision
	Compare  *compare.Result
	Flips    []Flip
	Cost     CostInput
	// Signoff 是签字状态说明（无需签字 / 已签字 / 未签字原因）。
	Signoff string
	Holdout map[string]bool
}

// Render 写配对报告：首行门禁结论，其后规则判定、翻转样本（逐条）、flaky 清单、unexpected_call 清单、成本与延迟，
// 最后附 compare 明细（切片结果只进报告，不进结论）。
func Render(w io.Writer, in ReportInput) error {
	if in.Decision == nil || in.Compare == nil {
		return errNoDecision
	}
	var b strings.Builder
	r := in.Compare
	fmt.Fprintf(&b, "# 门禁结论：%s（baseline %d / candidate %d）\n\n", in.Decision.Verdict, r.BaselineID, r.CandidateID)
	if in.Signoff != "" {
		fmt.Fprintf(&b, "签字：%s\n\n", in.Signoff)
	}
	renderFindings(&b, in.Decision)
	renderFlips(&b, in.Flips)
	renderFlaky(&b, r)
	renderUnexpected(&b, r.Candidate.UnexpectedCalls, in.Holdout)
	if err := renderCost(&b, in.Cost); err != nil {
		return err
	}
	b.WriteString("## 单因子对比明细\n\n")
	if err := compare.Render(&b, r); err != nil {
		return err
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("gate: write report: %w", err)
	}
	return nil
}

func renderFindings(b *strings.Builder, d *Decision) {
	b.WriteString("## 规则判定\n\n| 规则 | 命中结论 | 命中 | 原因 |\n|---|---|---|---|\n")
	for _, f := range d.Findings {
		hit := "否"
		if f.Hit {
			hit = "是"
		}
		reasons := "-"
		if len(f.Reasons) > 0 {
			reasons = strings.ReplaceAll(strings.Join(f.Reasons, "；"), "|", "\\|")
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", f.Rule, f.Verdict, hit, reasons)
	}
	b.WriteString("\n")
}

func renderFlips(b *strings.Builder, flips []Flip) {
	fmt.Fprintf(b, "## 翻转样本（%d）\n\n", len(flips))
	if len(flips) == 0 {
		b.WriteString("无\n\n")
		return
	}
	for _, dir := range []Direction{Regressed, Improved} {
		for _, f := range flips {
			if f.Direction == dir {
				fmt.Fprintf(b, "- %s %s %s：%.3f → %.3f\n", f.Direction, f.Metric, f.Key, f.Baseline, f.Candidate)
			}
		}
	}
	b.WriteString("\n")
}

// renderFlaky 列任一指标上任一边 flaky 的配对样本（照样本级均值参与判定）。
func renderFlaky(b *strings.Builder, r *compare.Result) {
	var keys []string
	for _, m := range r.Metrics {
		for _, k := range m.Flaky {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	fmt.Fprintf(b, "## flaky（%d，按样本级均值参与判定）\n\n", len(keys))
	if len(keys) == 0 {
		b.WriteString("无\n\n")
		return
	}
	for _, k := range keys {
		fmt.Fprintf(b, "- %s\n", k)
	}
	b.WriteString("\n")
}

func renderUnexpected(b *strings.Builder, calls []evalkit.UnexpectedCallRef, holdout map[string]bool) {
	fmt.Fprintf(b, "## unexpected_call（candidate，%d）\n\n", len(calls))
	if len(calls) == 0 {
		b.WriteString("无\n\n")
		return
	}
	for _, c := range calls {
		if holdout[c.SampleID] {
			fmt.Fprintf(b, "- %s#%d（holdout，只给 ID）\n", c.SampleID, c.TrialIndex)
			continue
		}
		fmt.Fprintf(b, "- %s#%d run=%d %s\n", c.SampleID, c.TrialIndex, c.RunID, strings.Join(c.Tools, ","))
	}
	b.WriteString("\n")
}

func renderCost(b *strings.Builder, c CostInput) error {
	rows, err := CostRows(c)
	if err != nil {
		return err
	}
	b.WriteString("## 成本与延迟\n\n| 指标 | baseline | candidate | 上限 | 劣化 | 判定 |\n|---|---|---|---|---|---|\n")
	for _, r := range rows {
		worse, mode := "否", "判定"
		if r.Worse {
			worse = "是"
		}
		if r.ReportOnly {
			mode = "只出报告（桩模型）"
		}
		base, cand, limit := fmt.Sprintf("%.3f", r.Baseline), fmt.Sprintf("%.3f", r.Candidate), fmt.Sprintf("%.3f", r.Limit)
		switch {
		case r.BaselineMissing:
			base, limit, worse, mode = "基线无数据", "-", "-", "不参与判定（基线无数据）"
		case r.CandidateMissing:
			cand, limit, worse = "候选无数据", "-", "无法判定"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", r.Title, base, cand, limit, worse, mode)
	}
	b.WriteString("\n")
	return nil
}
