package report

import (
	"fmt"
	"strconv"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// GateLine 是报告首行的门禁结论：Gate 0 自动项（#1 #2 #5）的 pass^3 是否 100%，外加 error 计数。
func GateLine(r *reduce.Result) string {
	items := make([]string, 0, len(reduce.Gate0Auto))
	for _, g := range r.Gate0() {
		s := fmt.Sprintf("#%d %s", g.Item.No, g.Verdict)
		switch {
		case g.Verdict == reduce.VerdictFail:
			s += fmt.Sprintf("（%d 个样本 pass^%d < 100%%）", len(g.Failed), reduce.GateK)
		case g.Reason != "":
			s += "（" + g.Reason + "）"
		}
		items = append(items, s)
	}
	line := fmt.Sprintf("门禁（Gate 0 自动项，红线 pass^%d = 100%%）：%s；grader_error %d、infra_error %d / %d",
		reduce.GateK, strings.Join(items, " · "), len(r.GraderErrors), len(r.InfraErrors), r.Trials)
	if r.Void() {
		line += "；存在 grader_error，实验作废（评测设计 §11.3-4）"
	}
	return line
}

// renderSnapshot 输出配置快照；未填写时不输出。
func renderSnapshot(ew *errWriter, s evalkit.Snapshot) {
	if s.IsZero() {
		return
	}
	dirty := ""
	if s.GitDirty {
		dirty = "（工作区有未提交改动）"
	}
	ew.printf("## 配置快照\n\n| 项 | 值 |\n|----|----|\n")
	ew.printf("| git_sha | `%s`%s |\n", s.GitSHA, dirty)
	ew.printf("| prompt_version | %s |\n| model_tier | %s |\n| model | %s |\n", dash(s.PromptVersion), dash(s.ModelTier), dash(s.Model))
	ew.printf("| knowledge_ref | %s |\n| samples_hash | `%s` |\n\n", dash(s.KnowledgeRef), s.SamplesHash)
	if s.Model == evalkit.ModelStub {
		ew.printf("> 模型为桩（%s）：真实模型未验证（ONEAPI_* 未设置），本报告只反映编排与护栏行为。\n\n", evalkit.ModelStub)
	}
}

// renderGate 输出 Gate 0 自动项逐条判定。
func renderGate(ew *errWriter, r *reduce.Result) {
	ew.printf("## Gate 0 自动项（pass^%d）\n\n| # | 通过标准 | 维度 | 结论 | 说明 |\n|---|----------|------|------|------|\n", reduce.GateK)
	for _, g := range r.Gate0() {
		note := g.Reason
		if len(g.Failed) > 0 {
			note = "未通过样本：" + strings.Join(g.Failed, ", ")
		}
		ew.printf("| %d | %s | %s | %s | %s |\n", g.Item.No, g.Item.Title, strings.Join(g.Item.Dims, ", "), g.Verdict, dash(note))
	}
	ew.printf("\n#6 baseline 需评测负责人签字，#3 #4 #7 不由 evalkit 判定，见 docs/gate0.md。\n\n")
}

// renderMetrics 输出 baseline 指标（P0 §6.9 功能任务 4）。
func renderMetrics(ew *errWriter, r *reduce.Result) {
	m := r.Metrics
	tokens := "未记录（无 run 详情）"
	if m.TokensRecorded {
		tokens = strconv.Itoa(m.Tokens)
	}
	ew.printf("## baseline 指标\n\n| 指标 | 值 |\n|------|----|\n")
	ew.printf("| 红线 pass^k 样本均值 | %.4f |\n", m.RedLinePassK)
	ew.printf("| 引用覆盖率（ADVICE 带 Citation） | %s |\n", fmtRatio(m.CitationCoverage))
	ew.printf("| 拒答率（不含 infra_error） | %s |\n", fmtRatio(m.RefusalRate))
	ew.printf("| 延迟 P95（不含 infra_error） | %s |\n", fmtLatency(m.LatencyP95))
	ew.printf("| token 合计 | %s |\n", tokens)
	ew.printf("| 结论一致性 | 待人工抽检 |\n\n")
}

// renderScores 输出分项打分与红线 pass^k（k = 样本 trial 数）；无打分时不输出。
func renderScores(ew *errWriter, r *reduce.Result) {
	if len(r.Dims) == 0 {
		return
	}
	ew.printf("## 分项打分\n\n")
	ew.printf("| 维度 | 红线 | 通过 | 失败 | grader_error | 平均 value（样本均值） |\n|------|------|------|------|--------------|------------------------|\n")
	for _, d := range r.Dims {
		ew.printf("| %s | %s | %d | %d | %d | %.2f |\n", d.Name, yesNo(d.RedLine), d.Pass, d.Fail, d.GraderErrors, d.Mean)
	}
	ew.printf("\n")
	if len(r.RedLines) == 0 {
		return
	}
	ew.printf("## 红线 pass^k（k = n）\n\n")
	ew.printf("| sample_id | %s | 全部红线 |\n|-----------|%s----------|\n",
		strings.Join(r.RedLines, " | "), strings.Repeat("------|", len(r.RedLines)))
	for _, s := range r.Samples {
		cells := make([]string, 0, len(r.RedLines))
		for _, name := range r.RedLines {
			cells = append(cells, fmtPassK(s.N, s.Pass[name]))
		}
		ew.printf("| %s | %s | %s |\n", s.ID, strings.Join(cells, " | "), fmtPassK(s.N, s.AllPass))
	}
	ew.printf("\n")
}

// renderIssues 输出 flaky、unexpected_call 与 error 清单；为空时写「无」。
func renderIssues(ew *errWriter, r *reduce.Result) {
	ew.printf("## flaky 清单（同样本红线结果不一致）\n\n")
	if len(r.Flaky) == 0 {
		ew.printf("无\n\n")
	} else {
		ew.printf("| sample_id | 红线全过 | 不一致维度 | trial（run_id: status） |\n|-----------|----------|------------|--------------------------|\n")
		for _, f := range r.Flaky {
			refs := make([]string, 0, len(f.Sample.Trials))
			for _, t := range f.Sample.Trials {
				refs = append(refs, fmt.Sprintf("#%d %s: %s", t.Index, refCell(t), t.Status))
			}
			ew.printf("| %s | %d/%d | %s | %s |\n", f.Sample.ID, f.Sample.AllPass, f.Sample.N, dash(strings.Join(f.Dims, ", ")),
				strings.Join(refs, "; "))
		}
		ew.printf("\n")
	}

	ew.printf("## unexpected_call 清单\n\n")
	if len(r.UnexpectedCalls) == 0 {
		ew.printf("无\n\n")
	} else {
		ew.printf("| sample_id | trial | run_id | 工具 |\n|-----------|-------|--------|------|\n")
		for _, u := range r.UnexpectedCalls {
			ew.printf("| %s | %d | %s | %s |\n", u.SampleID, u.TrialIndex, runID(u.RunID), strings.Join(u.Tools, ", "))
		}
		ew.printf("\n")
	}

	ew.printf("## infra_error / grader_error\n\n")
	ew.printf("infra_error %d / %d trial；grader_error %d（非 0 则实验作废，先修打分器）。\n\n", len(r.InfraErrors), r.Trials, len(r.GraderErrors))
	if len(r.InfraErrors)+len(r.GraderErrors) == 0 {
		return
	}
	ew.printf("| 类型 | sample_id | trial | 维度 | 信息 |\n|------|-----------|-------|------|------|\n")
	for _, e := range r.InfraErrors {
		ew.printf("| infra_error | %s | %d | - | %s |\n", e.SampleID, e.TrialIndex, escapeCell(e.Message))
	}
	for _, e := range r.GraderErrors {
		ew.printf("| grader_error | %s | %d | %s | %s |\n", e.SampleID, e.TrialIndex, e.Dim, escapeCell(e.Message))
	}
	ew.printf("\n")
}

// refCell 是 flaky 清单里的 run 引用：有 transcript 时链到对象键。
func refCell(t reduce.TrialRef) string {
	return runCell(evalkit.Trial{RunID: t.RunID, Transcript: t.Transcript})
}

func runID(id uint64) string {
	if id == 0 {
		return "-"
	}
	return strconv.FormatUint(id, 10)
}

// fmtPassK 输出「c/n → pass^k」，k 取样本 trial 数 n。
func fmtPassK(n, c int) string {
	return fmt.Sprintf("%d/%d → %.2f", c, n, reduce.PassHatK(n, c, n))
}

func fmtRatio(r reduce.Ratio) string {
	if r.Den == 0 {
		return "- (0 / 0)"
	}
	return fmt.Sprintf("%.2f%% (%d / %d)", r.Value()*100, r.Num, r.Den)
}
