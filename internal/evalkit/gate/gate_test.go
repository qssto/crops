package gate

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/store"
)

// TestRules_Order 固定注册表顺序：BLOCK_REDLINE → BLOCK_QUALITY → NOT_ATTRIBUTABLE → NEED_SIGNOFF（简报 L7-b 第 1 条）。
func TestRules_Order(t *testing.T) {
	var names []string
	var verdicts []Verdict
	for _, r := range Rules {
		names = append(names, r.Name)
		verdicts = append(verdicts, r.Verdict)
	}
	want := []string{RuleRedLine, RuleFullCoverage, RuleIncomplete, RuleTrustRegression, RuleNotAttributable,
		RuleCostLatency, RuleJudgeUnrated}
	if !slices.Equal(names, want) {
		t.Fatalf("rules = %v, want %v", names, want)
	}
	wantV := []Verdict{BlockRedLine, BlockQuality, BlockQuality, BlockQuality, NotAttributable, NeedSignoff, NeedSignoff}
	if !slices.Equal(verdicts, wantV) {
		t.Fatalf("verdicts = %v, want %v", verdicts, wantV)
	}
	for _, v := range wantV {
		if !Valid(string(v)) {
			t.Fatalf("%s not in Verdicts", v)
		}
	}
	if !Valid(string(Pass)) || Valid("PASSED") || Valid("") {
		t.Fatal("Valid whitelist broken")
	}
	// store 的 gate_verdict 白名单与本包一致（store 不依赖 gate 包）。
	var vs []string
	for _, v := range Verdicts {
		vs = append(vs, string(v))
	}
	if !slices.Equal(vs, store.GateVerdicts) {
		t.Fatalf("Verdicts %v != store.GateVerdicts %v", vs, store.GateVerdicts)
	}
}

func TestEvaluate_Pass(t *testing.T) {
	ids := sampleIDs(6)
	tr := side(ids, 5, allHealthy)
	d := mustEvaluate(t, build(t, tr, tr))
	if d.Verdict != Pass {
		t.Fatalf("verdict = %s, findings %+v", d.Verdict, d.Findings)
	}
	if len(d.Findings) != len(Rules) {
		t.Fatalf("findings = %d, want every rule evaluated (%d)", len(d.Findings), len(Rules))
	}
}

// TestEvaluate_RedLine：任一样本红线 pass^3 < 1 → BLOCK_REDLINE（先于其他规则）。
func TestEvaluate_RedLine(t *testing.T) {
	ids := sampleIDs(6)
	base := side(ids, 5, allHealthy)
	cand := side(ids, 5, func(id string, i int) []evalkit.Score {
		if id == "s03" && i == 4 {
			return []evalkit.Score{passed(dimCitation), passed(dimNumbers), {Name: dimRefusal, RedLine: true, Evidence: "want refusal"}}
		}
		return healthy()
	})
	d := mustEvaluate(t, build(t, base, cand))
	if d.Verdict != BlockRedLine {
		t.Fatalf("verdict = %s", d.Verdict)
	}
	if f := finding(t, d, RuleRedLine); !f.Hit || !strings.Contains(strings.Join(f.Reasons, " "), "s03") {
		t.Fatalf("red_line finding = %+v", f)
	}
}

// TestEvaluate_RedLine_TooFewTrials：n < 3 也算不过（失败关闭）。
func TestEvaluate_RedLine_TooFewTrials(t *testing.T) {
	ids := sampleIDs(4)
	tr := side(ids, 2, allHealthy)
	if d := mustEvaluate(t, build(t, tr, tr)); d.Verdict != BlockRedLine {
		t.Fatalf("verdict = %s, want BLOCK_REDLINE for n=2", d.Verdict)
	}
}

// TestEvaluate_RedLine_ExcludesUserSim 是验收 4：user_sim 样本（trial tag 或按 sample_id 判定）不进 gate 的红线判定。
func TestEvaluate_RedLine_ExcludesUserSim(t *testing.T) {
	ids := sampleIDs(6)
	base := side(ids, 5, allHealthy)
	failing := func(id string, i int) []evalkit.Score {
		if id == "s05" {
			return []evalkit.Score{passed(dimCitation), passed(dimNumbers), {Name: dimRefusal, RedLine: true, Evidence: "want refusal"}}
		}
		return healthy()
	}
	// mark 把 s05 的 trial 放进 suite，tagged 时加 user_sim tag。
	mark := func(suite string, tagged bool) []evalkit.Trial {
		tr := side(ids, 5, failing)
		for i := range tr {
			if tr[i].SampleID == "s05" {
				tr[i].Suite = suite
				if tagged {
					tr[i].Tags = []string{TagUserSim}
				}
			}
		}
		return tr
	}
	in := build(t, base, mark(SuiteCapability, true))
	if d := mustEvaluate(t, in); d.Verdict == BlockRedLine || finding(t, d, RuleRedLine).Hit {
		t.Fatalf("tagged user_sim sample gated red line: %s", d.Verdict)
	}
	// 读回的 trial 没有 Tags：按调用方给的 sample_id 判定。
	in = build(t, base, mark(SuiteCapability, false))
	if d := mustEvaluate(t, in); d.Verdict != BlockRedLine {
		t.Fatalf("without predicate verdict = %s, want BLOCK_REDLINE", d.Verdict)
	}
	in.UserSim = func(tr evalkit.Trial) bool { return tr.SampleID == "s05" }
	if d := mustEvaluate(t, in); finding(t, d, RuleRedLine).Hit {
		t.Fatalf("predicate user_sim sample gated red line: %+v", d.Findings)
	}
	// 评审 P2：排除还要求 suite = capability（user_sim 样本只进能力套件）。regression 等套件里带 tag / 被谓词命中的
	// 样本照常进红线（误打 tag 或样本清单漂移都不能绕过红线）。
	for _, tagged := range []bool{true, false} {
		in = build(t, base, mark("regression", tagged))
		in.UserSim = func(tr evalkit.Trial) bool { return tr.SampleID == "s05" }
		if d := mustEvaluate(t, in); d.Verdict != BlockRedLine {
			t.Fatalf("regression-suite user_sim (tagged=%v) verdict = %s, want BLOCK_REDLINE", tagged, d.Verdict)
		}
	}
}

// 评审 P2：排除 user_sim 后没有可进红线门禁的样本（候选全是 user_sim 或没有 trial）时不给结论（返回错误），
// 不能因为「没有样本失败」判红线 / 全覆盖通过。
func TestEvaluate_RedLine_NoGatedSamples(t *testing.T) {
	ids := sampleIDs(3)
	tr := side(ids, 5, allHealthy)
	allSim := slices.Clone(tr)
	for i := range allSim {
		allSim[i].Suite, allSim[i].Tags = SuiteCapability, []string{TagUserSim}
	}
	in := build(t, tr, allSim)
	if _, err := Evaluate(in); !errors.Is(err, ErrNoGatedSamples) {
		t.Fatalf("all user_sim candidate: err = %v, want ErrNoGatedSamples", err)
	}
	in = build(t, tr, tr)
	in.Candidate = nil
	if _, err := Evaluate(in); !errors.Is(err, ErrNoGatedSamples) {
		t.Fatalf("empty candidate: err = %v, want ErrNoGatedSamples", err)
	}
}

func TestRedLineFailures(t *testing.T) {
	ids := sampleIDs(3)
	tr := side(ids, 3, func(id string, _ int) []evalkit.Score {
		if id == "s01" {
			return []evalkit.Score{{Name: dimCitation, RedLine: true, Evidence: "missing"}}
		}
		return healthy()
	})
	if got := RedLineFailures(tr, nil); !slices.Equal(got, []string{"s01"}) {
		t.Fatalf("failures = %v", got)
	}
	isS01 := func(t evalkit.Trial) bool { return t.SampleID == "s01" }
	if got := RedLineFailures(tr, isS01); !slices.Equal(got, []string{"s01"}) {
		t.Fatalf("regression-suite trial excluded as user_sim: failures = %v", got)
	}
	for i := range tr {
		if tr[i].SampleID == "s01" {
			tr[i].Suite = SuiteCapability
		}
	}
	if got := RedLineFailures(tr, isS01); len(got) != 0 {
		t.Fatalf("user_sim excluded failures = %v", got)
	}
	if !IsUserSim(evalkit.Trial{Tags: []string{"x", TagUserSim}}) || IsUserSim(evalkit.Trial{Tags: []string{"x"}}) {
		t.Fatal("IsUserSim by tag broken")
	}
}

// TestEvaluate_FullCoverage：引用覆盖率 / 数值准确率 < 100% 无条件 BLOCK_QUALITY（与 baseline 无关）。
// 构造数据把 citation_valid / numbers_match_calc 标为非红线，避免被红线规则先拦，单测本规则。
func TestEvaluate_FullCoverage(t *testing.T) {
	for _, dim := range []string{dimCitation, dimNumbers} {
		t.Run(dim, func(t *testing.T) {
			ids := sampleIDs(6)
			scores := func(id string, i int) []evalkit.Score {
				out := healthy()
				for j := range out {
					if out[j].Name == dim {
						out[j].RedLine = false
						if id == "s02" && i == 0 {
							out[j].Pass, out[j].Value, out[j].Evidence = false, 0, "构造：结论字段无引用"
						}
					}
				}
				return out
			}
			tr := side(ids, 5, scores)
			d := mustEvaluate(t, build(t, tr, tr))
			if d.Verdict != BlockQuality {
				t.Fatalf("verdict = %s, findings %+v", d.Verdict, d.Findings)
			}
			if f := finding(t, d, RuleFullCoverage); !f.Hit || !strings.Contains(strings.Join(f.Reasons, " "), "s02@1") {
				t.Fatalf("full_coverage = %+v", f)
			}
		})
	}
}

// TestEvaluate_Incomplete：baseline 有样本未被 candidate 配对 → BLOCK_QUALITY（失败关闭，不能在子集上放行）。
func TestEvaluate_Incomplete(t *testing.T) {
	base := side(sampleIDs(6), 5, allHealthy)
	cand := side(sampleIDs(5), 5, allHealthy)
	d := mustEvaluate(t, build(t, base, cand))
	if d.Verdict != BlockQuality || !finding(t, d, RuleIncomplete).Hit {
		t.Fatalf("verdict = %s, findings %+v", d.Verdict, d.Findings)
	}
}

// regressedRefusal 造候选：一半以上样本全部 trial 过度保守（拒答正确率、过度保守率都退化；红线不失败）。
func regressedRefusal(ids []string) func(string, int) []evalkit.Score {
	return func(id string, _ int) []evalkit.Score {
		if slices.Index(ids, id) < 6 {
			return []evalkit.Score{passed(dimCitation), passed(dimNumbers), overConservative()}
		}
		return healthy()
	}
}

// TestEvaluate_TrustRegression：可信度指标退化（区间不跨 0）且降幅超阈值 → BLOCK_QUALITY；阈值放宽到降幅以上则不拦。
func TestEvaluate_TrustRegression(t *testing.T) {
	ids := sampleIDs(10)
	base := side(ids, 5, allHealthy)
	cand := side(ids, 5, regressedRefusal(ids))
	in := build(t, base, cand)
	d := mustEvaluate(t, in)
	if d.Verdict != BlockQuality {
		t.Fatalf("verdict = %s, findings %+v", d.Verdict, d.Findings)
	}
	f := finding(t, d, RuleTrustRegression)
	reasons := strings.Join(f.Reasons, " ")
	if !f.Hit || !strings.Contains(reasons, "refusal_accuracy") || !strings.Contains(reasons, "over_conservative_rate") {
		t.Fatalf("trust_regression = %+v", f)
	}
	if finding(t, d, RuleRedLine).Hit {
		t.Fatal("over_conservative must not fail red line")
	}
	// 阈值高于降幅（0.6）：不拦。
	for _, name := range []string{"refusal_accuracy", "over_conservative_rate"} {
		in.Thresholds[name] = ratio(0.9)
	}
	if d := mustEvaluate(t, in); finding(t, d, RuleTrustRegression).Hit {
		t.Fatalf("drop below threshold must not block: %+v", d.Findings)
	}
}

// TestEvaluate_TrustRegression_NoSig：差异不显著（区间跨 0）不拦。
func TestEvaluate_TrustRegression_NoSig(t *testing.T) {
	ids := sampleIDs(10)
	base := side(ids, 5, allHealthy)
	cand := side(ids, 5, func(id string, i int) []evalkit.Score {
		if id == "s00" && i == 0 {
			return []evalkit.Score{passed(dimCitation), passed(dimNumbers), overConservative()}
		}
		return healthy()
	})
	d := mustEvaluate(t, build(t, base, cand))
	if finding(t, d, RuleTrustRegression).Hit {
		t.Fatalf("insignificant drop blocked: %+v", d.Findings)
	}
}

// TestEvaluate_TrustRegression_MissingThreshold：参与判定的指标没有阈值 → 报错（失败关闭，不给结论）。
func TestEvaluate_TrustRegression_MissingThreshold(t *testing.T) {
	ids := sampleIDs(6)
	tr := side(ids, 5, allHealthy)
	in := build(t, tr, tr)
	delete(in.Thresholds, "numeric_accuracy")
	if _, err := Evaluate(in); !errors.Is(err, ErrNoThreshold) {
		t.Fatalf("err = %v, want ErrNoThreshold", err)
	}
	in = build(t, tr, tr)
	in.Thresholds["numeric_accuracy"] = nil
	if _, err := Evaluate(in); !errors.Is(err, ErrNoThreshold) {
		t.Fatalf("null threshold err = %v, want ErrNoThreshold", err)
	}
}

// TestEvaluate_NotAttributable：多因子差异 → NOT_ATTRIBUTABLE。
func TestEvaluate_NotAttributable(t *testing.T) {
	ids := sampleIDs(6)
	tr := side(ids, 5, allHealthy)
	d := mustEvaluate(t, build(t, tr, tr, func(_, c *evalkit.Snapshot) { c.PromptVersion, c.LoaderStrategy = "v2", "v1" }))
	if d.Verdict != NotAttributable {
		t.Fatalf("verdict = %s", d.Verdict)
	}
	// 单因子差异可归因。
	d = mustEvaluate(t, build(t, tr, tr, func(_, c *evalkit.Snapshot) { c.PromptVersion = "v2" }))
	if d.Verdict != Pass {
		t.Fatalf("single factor verdict = %s, findings %+v", d.Verdict, d.Findings)
	}
}

// TestEvaluate_PhaseRefused：阶段级对比不给门禁结论。
func TestEvaluate_PhaseRefused(t *testing.T) {
	ids := sampleIDs(4)
	tr := side(ids, 5, allHealthy)
	r, err := compare.Run(compare.Input{Mode: compare.ModePhase, Baseline: compare.Side{ID: 1, Done: true, Snapshot: snap(), Trials: tr},
		Candidate: compare.Side{ID: 2, Done: true, Snapshot: snap(), Trials: tr}, Paired: pairedKeys(tr, tr)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(Input{Compare: r, Candidate: tr}); !errors.Is(err, ErrPhase) {
		t.Fatalf("err = %v, want ErrPhase", err)
	}
	if _, err := Evaluate(Input{}); !errors.Is(err, ErrNoCompare) {
		t.Fatalf("err = %v, want ErrNoCompare", err)
	}
}

// TestEvaluate_CostLatency：token / 压缩触发率劣化超阈值 → NEED_SIGNOFF；P95 在桩模型下只出报告，真实模型下判定。
func TestEvaluate_CostLatency(t *testing.T) {
	ids := sampleIDs(6)
	tr := side(ids, 5, allHealthy)
	base := Usage{Sessions: 30, Tokens: 3000, Compactions: intp(0), LatencyP95: time.Second}
	tests := []struct {
		name string
		cand Usage
		stub bool
		want Verdict
		rule string
	}{
		{"tokens +50%", Usage{Sessions: 30, Tokens: 4500, Compactions: intp(0), LatencyP95: time.Second}, true, NeedSignoff, MetricTokensPerSession},
		{"tokens +5% within", Usage{Sessions: 30, Tokens: 3150, Compactions: intp(0), LatencyP95: time.Second}, true, Pass, ""},
		{"compaction from 0", Usage{Sessions: 30, Tokens: 3000, Compactions: intp(3), LatencyP95: time.Second}, true, NeedSignoff, MetricCompactionRate},
		{"p95 x3 on stub report only", Usage{Sessions: 30, Tokens: 3000, Compactions: intp(0), LatencyP95: 3 * time.Second}, true, Pass, ""},
		{"p95 x3 real model", Usage{Sessions: 30, Tokens: 3000, Compactions: intp(0), LatencyP95: 3 * time.Second}, false, NeedSignoff, MetricLatencyP95},
		{"cheaper is fine", Usage{Sessions: 30, Tokens: 1000, Compactions: intp(0), LatencyP95: time.Second / 2}, false, Pass, ""},
		// 候选快照没记录压缩数：无法证明没劣化，失败关闭。
		{"candidate compactions unrecorded", Usage{Sessions: 30, Tokens: 3000, LatencyP95: time.Second}, true, NeedSignoff, MetricCompactionRate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := build(t, tr, tr)
			in.Cost.Baseline, in.Cost.Candidate, in.Cost.Stub = base, tt.cand, tt.stub
			d := mustEvaluate(t, in)
			if d.Verdict != tt.want {
				t.Fatalf("verdict = %s, want %s; findings %+v", d.Verdict, tt.want, d.Findings)
			}
			f := finding(t, d, RuleCostLatency)
			if tt.rule != "" && !strings.Contains(strings.Join(f.Reasons, " "), tt.rule) {
				t.Fatalf("cost finding = %+v, want %s", f, tt.rule)
			}
		})
	}
}

// TestEvaluate_CostLatency_NoLimits：成本阈值缺失 → 报错（失败关闭）。
func TestEvaluate_CostLatency_NoLimits(t *testing.T) {
	tr := side(sampleIDs(4), 5, allHealthy)
	in := build(t, tr, tr)
	in.Cost.Limits = in.Cost.Limits[:2]
	if _, err := Evaluate(in); !errors.Is(err, ErrBadCostLimits) {
		t.Fatalf("err = %v, want ErrBadCostLimits", err)
	}
}

func TestUsage(t *testing.T) {
	u := Usage{Sessions: 4, Tokens: 100, Compactions: intp(1)}
	if r, ok := u.CompactionRate(); u.TokensPerSession() != 25 || r != 0.25 || !ok {
		t.Fatalf("usage = %v %v %v", u.TokensPerSession(), r, ok)
	}
	if r, ok := (Usage{Compactions: intp(0)}).CompactionRate(); (Usage{}).TokensPerSession() != 0 || r != 0 || !ok {
		t.Fatal("empty usage must be 0")
	}
	if _, ok := (Usage{Sessions: 4}).CompactionRate(); ok {
		t.Fatal("unrecorded compactions reported as available")
	}
}

// 评审 P2：基线快照没记录压缩数（L7-b 之前的旧快照）≠ 0。该指标不参与判定（候选再多压缩也不因此 NEED_SIGNOFF），
// 报告写「基线无数据」；UsageOf 按快照字段是否存在区分。
func TestEvaluate_CostLatency_BaselineNoData(t *testing.T) {
	tr := side(sampleIDs(6), 5, allHealthy)
	in := build(t, tr, tr)
	in.Cost.Baseline = UsageOf(evalkit.Snapshot{TokenUsed: 1000}, tr)
	in.Cost.Candidate = UsageOf(evalkit.Snapshot{TokenUsed: 1000, Compactions: intp(9)}, tr)
	if in.Cost.Baseline.Compactions != nil || in.Cost.Candidate.Compactions == nil || *in.Cost.Candidate.Compactions != 9 {
		t.Fatalf("UsageOf compactions = %v / %v", in.Cost.Baseline.Compactions, in.Cost.Candidate.Compactions)
	}
	d := mustEvaluate(t, in)
	if d.Verdict != Pass || finding(t, d, RuleCostLatency).Hit {
		t.Fatalf("baseline without compactions gated: %s %+v", d.Verdict, finding(t, d, RuleCostLatency))
	}
	rows, err := CostRows(in.Cost)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderCost(&b, in.Cost); err != nil {
		t.Fatal(err)
	}
	if r := rows[2]; r.Name != MetricCompactionRate || !r.BaselineMissing || r.Worse {
		t.Fatalf("compaction row = %+v", r)
	}
	if !strings.Contains(b.String(), "基线无数据") {
		t.Fatalf("report:\n%s", b.String())
	}
}

// judgeSide 造 judge 维度打分：bad 中的样本 fact_consistency = 1，其余 3。
func judgeSide(ids []string, bad func(id string) bool) []evalkit.Trial {
	return side(ids, 5, func(id string, _ int) []evalkit.Score {
		v := 3.0
		if bad(id) {
			v = 1
		}
		return append(healthy(), fact(v))
	})
}

// TestEvaluate_Judge 用构造数据测达标与不达标两条路径（待真实模型 / 待专家：现网三维度均未达标）。
func TestEvaluate_Judge(t *testing.T) {
	ids := sampleIDs(10)
	base := judgeSide(ids, func(string) bool { return false })
	worse := judgeSide(ids, func(id string) bool { return slices.Index(ids, id) < 6 })
	judged := func(b, c *evalkit.Snapshot) {
		b.JudgeVersions = map[string]string{dimFact: "j1"}
		c.JudgeVersions = map[string]string{dimFact: "j1"}
	}

	t.Run("rated and regressed blocks", func(t *testing.T) {
		in := build(t, base, worse, judged)
		in.Thresholds = thresholds(zero())
		d := mustEvaluate(t, in)
		f := finding(t, d, RuleTrustRegression)
		if d.Verdict != BlockQuality || !strings.Contains(strings.Join(f.Reasons, " "), "fact_consistency") {
			t.Fatalf("verdict = %s, trust = %+v", d.Verdict, f)
		}
	})
	t.Run("rated without threshold fails closed", func(t *testing.T) {
		in := build(t, base, worse, judged)
		if _, err := Evaluate(in); !errors.Is(err, ErrNoThreshold) {
			t.Fatalf("err = %v, want ErrNoThreshold", err)
		}
	})
	t.Run("rated and flat passes", func(t *testing.T) {
		in := build(t, base, base, judged)
		in.Thresholds = thresholds(zero())
		if d := mustEvaluate(t, in); d.Verdict != Pass {
			t.Fatalf("verdict = %s, findings %+v", d.Verdict, d.Findings)
		}
	})
	t.Run("unrated skips dim and caps at NEED_SIGNOFF", func(t *testing.T) {
		in := build(t, base, worse, judged)
		in.Judge = map[string]JudgeDim{dimFact: {Why: "未评：校准未达标（待真实模型 / 待专家）"}}
		d := mustEvaluate(t, in)
		if d.Verdict != NeedSignoff || finding(t, d, RuleTrustRegression).Hit {
			t.Fatalf("verdict = %s, findings %+v", d.Verdict, d.Findings)
		}
		if f := finding(t, d, RuleJudgeUnrated); !f.Hit || !strings.Contains(strings.Join(f.Reasons, " "), "未评") {
			t.Fatalf("judge_unrated = %+v", f)
		}
	})
	t.Run("judge unavailable never passes", func(t *testing.T) {
		tr := side(ids, 5, allHealthy)
		in := build(t, tr, tr)
		in.Judge = nil
		if d := mustEvaluate(t, in); d.Verdict != NeedSignoff {
			t.Fatalf("verdict = %s, want NEED_SIGNOFF", d.Verdict)
		}
	})
	// 评审 P1：封顶覆盖 judges.yaml 全部维度。只评 fact_consistency、另两维缺项或未达标都不得 PASS。
	t.Run("other dims unrated cap at NEED_SIGNOFF", func(t *testing.T) {
		tr := side(ids, 5, allHealthy)
		for name, j := range map[string]map[string]JudgeDim{
			"missing":      {dimFact: {Rated: true}},
			"below target": {dimFact: {Rated: true}, "conclusion_match": {Why: "未评：校准未达标"}, "explanation_quality": {Rated: true}},
			"extra config dim": {dimFact: {Rated: true}, "conclusion_match": {Rated: true}, "explanation_quality": {Rated: true},
				"new_dim": {Why: "未评"}},
		} {
			in := build(t, tr, tr)
			in.Judge = j
			d := mustEvaluate(t, in)
			if f := finding(t, d, RuleJudgeUnrated); d.Verdict != NeedSignoff || !f.Hit {
				t.Errorf("%s: verdict = %s, judge_unrated = %+v", name, d.Verdict, f)
			}
		}
	})
	t.Run("block still wins over judge cap", func(t *testing.T) {
		in := build(t, base, judgeSide(ids, func(string) bool { return false }))
		in.Candidate = side(ids, 2, allHealthy)
		in.Judge = nil
		if d := mustEvaluate(t, in); d.Verdict != BlockRedLine {
			t.Fatalf("verdict = %s", d.Verdict)
		}
	})
}

// TestJudgeDims_CoverRegistry：每个 judge 指标都映射到 judge 维度（新 judge 指标漏登记即失败）。
func TestJudgeDims_CoverRegistry(t *testing.T) {
	for _, m := range reduce.QualityMetrics {
		if _, ok := JudgeDims[m.Name()]; ok != m.Judge() {
			t.Fatalf("metric %s judge=%t mapped=%t", m.Name(), m.Judge(), ok)
		}
	}
}
