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

// stubNote 是 stub 模型的报告抬头标注（P1-L8-A 审查修复）：桩模型输出确定，n 次重复只复现同一结果。
const stubNote = "确定性回归（stub 模型，n>1 不提供稳定性信息）"

// noApplicable 标记没有一个适用 trial 的维度（全部默认放行）。
const noApplicable = "无适用样本"

// ProviderLine 是报告抬头第二行：模型 provider（快照未填为「未记录」），stub 时另标 stubNote。
func ProviderLine(s evalkit.Snapshot) string {
	provider := s.Model
	if provider == "" {
		provider = "未记录"
	}
	line := "模型 provider：" + provider
	if s.Model == evalkit.ModelStub {
		line += "；" + stubNote
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
	ew.printf("| knowledge_ref | %s |\n| samples_hash | `%s` |\n", dash(s.KnowledgeRef), s.SamplesHash)
	// P2 L0 字段：P1 及以前的快照没有，未填不输出。
	if s.LoaderStrategy != "" || s.RetrievalStrategy != "" || s.JudgeVersions != nil {
		ew.printf("| loader_strategy | %s |\n| retrieval_strategy | %s |\n| judge_versions | %s |\n",
			dash(s.LoaderStrategy), dash(s.RetrievalStrategy), judgeVersionsText(s))
	}
	if len(s.Suites) > 0 {
		ew.printf("| suites | %s |\n| n | %d |\n", strings.Join(s.Suites, ","), s.N)
	}
	if s.WallTime != "" {
		ew.printf("| token_used | %d |\n| cost | %.4f |\n| wall_time | %s |\n", s.TokenUsed, s.Cost, s.WallTime)
	}
	ew.printf("\n")
	if s.Model == evalkit.ModelStub {
		ew.printf("> 模型为桩（%s）：真实模型未验证（ONEAPI_* 未设置），本报告只反映编排与护栏行为。\n\n", evalkit.ModelStub)
	}
}

// judgeVersionsText 是 judge_versions 的报告文本；未接 judge 时标「无（未接 judge）」。
func judgeVersionsText(s evalkit.Snapshot) string {
	if k := s.JudgeVersionsKey(); k != "" {
		return k
	}
	return "无（未接 judge）"
}

// quarantineNote 附在门禁行后：隔离样本未运行，门禁与红线结论不覆盖它们（不能读成「全部通过」）。
func quarantineNote(exp *evalkit.Experiment) string {
	if len(exp.Quarantined) == 0 {
		return ""
	}
	return fmt.Sprintf("；隔离样本 %d 个未运行，不计入门禁与红线统计（见「隔离样本」）", len(exp.Quarantined))
}

// renderQuarantined 单列隔离样本（P1-L4 审查修复）：未运行，不计入 N / 通过 / INFRA_ERROR；无隔离样本时不输出。
func renderQuarantined(ew *errWriter, qs []evalkit.QuarantinedSample) {
	if len(qs) == 0 {
		return
	}
	ew.printf("## 隔离样本\n\n以下样本声明了 quarantine，本次未运行：不计入样本数 / trial / 通过 / INFRA_ERROR，")
	ew.printf("Gate 0 与红线 pass^k 不覆盖它们。\n\n| sample_id | 套件 | 隔离理由 |\n|-----------|------|----------|\n")
	for _, q := range qs {
		ew.printf("| %s | %s | %s |\n", q.ID, q.Suite, escapeCell(q.Reason))
	}
	ew.printf("\n")
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
	ew.printf("| 维度 | 红线 | 适用 trial | 通过 | 失败 | grader_error | 平均 value（样本均值） |\n")
	ew.printf("|------|------|------------|------|------|--------------|------------------------|\n")
	for _, d := range r.Dims {
		applicable := strconv.Itoa(d.Applicable)
		if d.NoApplicable() {
			applicable = noApplicable
		}
		ew.printf("| %s | %s | %s | %d | %d | %d | %.2f |\n", d.Name, yesNo(d.RedLine), applicable, d.Pass, d.Fail,
			d.GraderErrors, d.Mean)
	}
	ew.printf("\n适用 trial 不含默认放行（样本未声明该维度期望）；标「%s」的维度在本次实验里全部默认放行，通过数不代表该项被验证。\n\n",
		noApplicable)
	renderAdvisoryErrors(ew, r)
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

// renderAdvisoryErrors 列门禁未开的 judge 出错次数（L2 评审裁决）：不计入上表通过 / 失败 / grader_error，
// 不改 trial 状态，已按 UNKNOWN 转人工队列；没有时不输出。
func renderAdvisoryErrors(ew *errWriter, r *reduce.Result) {
	var parts []string
	for _, d := range r.Dims {
		if d.AdvisoryErrors > 0 {
			parts = append(parts, fmt.Sprintf("%s %d 次", d.Name, d.AdvisoryErrors))
		}
	}
	if len(parts) == 0 {
		return
	}
	ew.printf("judge_error（门禁未开，不改 trial 状态，按 UNKNOWN 转人工队列）：%s。\n\n", strings.Join(parts, "，"))
}

// suiteRedteam 是红队套件名：夜间全量中单列 pass^5（P1 L8）。
const suiteRedteam = "redteam"

// renderSuites 输出「按套件红线 pass^k」（P1 L8）：多套件或含 redteam 时才写，单套件报告不变。
// P1-L8-A 审查修复：
//   - pass^5 列只在套件内每个样本都跑满 5 次时判定，否则写「n<5 不判定」；套件内有 grader_error 时两列 pass^k 都写「不可判定」；
//   - 只有 n 次全过样本数 = 样本数时写 1.00，否则四舍五入到 1.00 的值写「<1.00」；
//   - 「隔离数」列是该套件声明 quarantine 未运行的样本数；「无适用样本」列是套件内全部默认放行的红线；
//   - 按 tag 过滤的实验在 redteam 单列行标「子集」，不能读成全量结论。
func renderSuites(ew *errWriter, r *reduce.Result, exp *evalkit.Experiment) {
	if len(r.RedLines) == 0 || !(len(r.Suites) > 1 || hasSuite(r.Suites, suiteRedteam)) {
		return
	}
	quarantined := map[string]int{}
	for _, q := range exp.Quarantined {
		quarantined[q.Suite]++
	}
	ew.printf("## 按套件红线 pass^k\n\n")
	ew.printf("| 套件 | 样本 | trial | 最小 n | 隔离数 | 红线 pass^k 均值（k = n） | n 次全过样本 | pass^%d | 无适用样本的红线 |\n", reduce.NightlyK)
	ew.printf("|------|------|-------|--------|--------|---------------------------|--------------|--------|------------------|\n")
	for _, st := range r.Suites {
		passKK := fmt.Sprintf("n<%d 不判定", reduce.NightlyK)
		if st.Judged() || st.GraderErrors > 0 {
			passKK = fmtSuiteValue(st, st.PassKK)
		}
		ew.printf("| %s | %d | %d | %d | %d | %s | %d / %d | %s | %s |\n", dash(st.Suite), st.Samples, st.Trials, st.MinN,
			quarantined[st.Suite], fmtSuiteValue(st, st.MeanPassK), st.AllPassSamples, st.Samples, passKK,
			dash(strings.Join(st.Inapplicable, ", ")))
	}
	ew.printf("\n")
	subset := ""
	if len(exp.Tags) > 0 {
		subset = fmt.Sprintf("；子集（tag 过滤：%s），不代表全量", strings.Join(exp.Tags, ", "))
	}
	for _, st := range r.Suites {
		if st.Suite != suiteRedteam {
			continue
		}
		ew.printf("redteam 单列：%d 个样本 n 次红线全过 %d 个，pass^%d %s（夜间全量要求 N ≥ %d）%s。\n\n",
			st.Samples, st.AllPassSamples, reduce.NightlyK, fmtRedteamPassKK(st), reduce.NightlyK, subset)
		if len(st.Inapplicable) > 0 {
			ew.printf("redteam 中%s的红线：%s（这些维度全部默认放行，pass^k 不覆盖它们）。\n\n", noApplicable,
				strings.Join(st.Inapplicable, ", "))
		}
	}
}

// fmtSuiteValue 输出套件级 pass^k：有 grader_error 为「不可判定」；不是每个样本都全过却四舍五入成 1.00 时写「<1.00」。
func fmtSuiteValue(st reduce.SuiteStat, v float64) string {
	if st.GraderErrors > 0 {
		return fmt.Sprintf("不可判定（grader_error %d）", st.GraderErrors)
	}
	s := fmt.Sprintf("%.2f", v)
	if s == "1.00" && !st.AllPass() {
		return "<1.00"
	}
	return s
}

func fmtRedteamPassKK(st reduce.SuiteStat) string {
	switch {
	case st.GraderErrors > 0:
		return fmtSuiteValue(st, st.PassKK)
	case !st.Judged():
		return "不判定"
	}
	return "= " + fmtSuiteValue(st, st.PassKK)
}

func hasSuite(suites []reduce.SuiteStat, name string) bool {
	for _, st := range suites {
		if st.Suite == name {
			return true
		}
	}
	return false
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
