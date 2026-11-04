package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
)

// kcA / kcB 是两份知识内容哈希（knowledge_ref 的 kc: 格式）。
var (
	kcA = evalkit.KnowledgeContentPrefix + strings.Repeat("a", 64)
	kcB = evalkit.KnowledgeContentPrefix + strings.Repeat("b", 64)
)

func snap() evalkit.Snapshot {
	return evalkit.Snapshot{GitSHA: strings.Repeat("a", 40), PromptVersion: "v1", ModelTier: "standard", Model: evalkit.ModelStub,
		KnowledgeRef: kcA, SamplesHash: "h1", LoaderStrategy: evalkit.LoaderV0, RetrievalStrategy: evalkit.RetrievalProgressive,
		JudgeVersions: map[string]string{}, Suites: []string{"regression"}, Tags: []string{}, N: 5, WallTime: "1m0s"}
}

func TestDiff_RegistryCount(t *testing.T) {
	b, c := snap(), snap()
	c.GitSHA = strings.Repeat("b", 40) // 不是因子
	c.TokenUsed, c.N, c.WallTime = 99, 3, "2m0s"
	if d := Diff(Factors, b, c); len(d) != 0 {
		t.Fatalf("run params / git_sha must not be factors: %+v", d)
	}
	c.PromptVersion, c.LoaderStrategy = "v2", "v1"
	d := Diff(Factors, b, c)
	if len(d) != 2 || d[0] != (FactorDiff{"prompt_version", "v1", "v2"}) || d[1].Name != "loader_strategy" {
		t.Fatalf("diff = %+v", d)
	}
	// OCP：新因子只加注册项。
	extra := append(append([]Factor(nil), Factors...), Factor{Name: "enable_compaction",
		Value: func(s evalkit.Snapshot) string { return s.WallTime }})
	if got := len(Diff(extra, b, c)); got != 3 {
		t.Fatalf("extra registry diff = %d, want 3", got)
	}
}

func TestCheck_Rejects(t *testing.T) {
	kb := snap()
	kb.KnowledgeRef = kcB
	judge := snap()
	judge.JudgeVersions = map[string]string{"fact": "j1"}
	hash := snap()
	hash.SamplesHash = "h2"
	tests := []struct {
		name string
		mode Mode
		c    evalkit.Snapshot
		ok   bool
	}{
		{"factor knowledge_ref", ModeFactor, kb, false},
		{"factor judge_versions", ModeFactor, judge, false},
		{"phase judge_versions", ModePhase, judge, false},
		{"phase samples_hash", ModePhase, hash, false},
		{"phase knowledge_ref allowed", ModePhase, kb, true},
		{"factor samples_hash differs (paired on intersection)", ModeFactor, hash, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Check(Factors, tt.mode, snap(), tt.c)
			if tt.ok != (err == nil) || (!tt.ok && !errors.Is(err, ErrIncomparable)) {
				t.Fatalf("err = %v, want ok=%t", err, tt.ok)
			}
		})
	}
}

// trials 造一个实验：每个样本 n 次 trial，citation_valid 按 pass 给出。
func trials(n int, pass map[string]bool) []evalkit.Trial {
	var out []evalkit.Trial
	for id, p := range pass {
		for i := range n {
			out = append(out, evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: i,
				Scores: []evalkit.Score{{Name: "citation_valid", RedLine: true, Pass: p, Value: boolF(p)}}})
		}
	}
	return out
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// TestRun_MultiFactorNotAttributable 是验收 §6.1-2：同时改提示词与加载策略。
func TestRun_MultiFactorNotAttributable(t *testing.T) {
	cs := snap()
	cs.PromptVersion, cs.LoaderStrategy = "v2", "v1"
	tr := trials(2, map[string]bool{"a": true, "b": true})
	in := Input{Mode: ModeFactor, Baseline: Side{Done: true, ID: 1, Snapshot: snap(), Trials: tr}, Candidate: Side{Done: true, ID: 2, Snapshot: cs, Trials: tr},
		Paired: []string{"a@1", "b@1"}, Seed: 7}
	r, err := Run(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Conclusion != ConclusionNotAttributable || r.Releasable() {
		t.Fatalf("conclusion = %s releasable = %t", r.Conclusion, r.Releasable())
	}
	var b strings.Builder
	if err := Render(&b, r); err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(b.String(), "\n")
	if first != MultiFactorWarning || !strings.Contains(first, "多因子对比，结论不可归因") {
		t.Fatalf("first line = %q", first)
	}
	if !strings.Contains(b.String(), "不可放行") {
		t.Fatal("report must say 不可放行")
	}
	diff, ok, err := r.Record()
	if err != nil || !ok || !strings.Contains(string(diff), `"name":"prompt_version"`) {
		t.Fatalf("record = %s %t %v", diff, ok, err)
	}

	// 同一对比 --mode phase：只出整体结论，不写门禁字段。
	in.Mode = ModePhase
	in.KnowledgeChanges = []string{"knowledge/rice/a.md"}
	p, err := Run(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Conclusion != "" || p.Releasable() || p.Overall != VerdictNoSig {
		t.Fatalf("phase conclusion = %q releasable = %t overall = %s", p.Conclusion, p.Releasable(), p.Overall)
	}
	if _, ok, _ := p.Record(); ok {
		t.Fatal("phase mode must not record gate fields")
	}
	b.Reset()
	if err := Render(&b, p); err != nil {
		t.Fatal(err)
	}
	first, _, _ = strings.Cut(b.String(), "\n")
	if !strings.HasPrefix(first, "knowledge_ref：baseline `"+kcA+"` | candidate `"+kcA+"`") || !strings.Contains(first, "knowledge/rice/a.md") {
		t.Fatalf("phase first line = %q", first)
	}
	if strings.Contains(b.String(), "gate_verdict") || strings.Contains(b.String(), "放行") || strings.Contains(b.String(), MultiFactorWarning) {
		t.Fatal("phase report must not carry gate fields")
	}
}

func TestRun_SingleFactorRegression(t *testing.T) {
	cs := snap()
	cs.PromptVersion = "v2"
	b := Side{Done: true, ID: 1, Snapshot: snap(), Trials: trials(2, map[string]bool{"a": true, "b": true, "c": true, "d": true})}
	c := Side{Done: true, ID: 2, Snapshot: cs, Trials: trials(2, map[string]bool{"a": false, "b": false, "c": false, "d": false})}
	r, err := Run(Input{Mode: ModeFactor, Baseline: b, Candidate: c, Paired: []string{"a@1", "b@1", "c@1", "d@1"}, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Attributable() || r.Conclusion != ConclusionRegressed || r.Releasable() {
		t.Fatalf("conclusion = %s", r.Conclusion)
	}
	c.Trials, c.Snapshot = b.Trials, snap()
	r, err = Run(Input{Mode: ModeFactor, Baseline: b, Candidate: c, Paired: []string{"a@1", "b@1", "c@1", "d@1"}, Seed: 1})
	if err != nil || r.Conclusion != ConclusionNoSig || !r.Releasable() {
		t.Fatalf("same config: %v %s", err, r.Conclusion)
	}
}

func TestRun_IntersectionAndExcluded(t *testing.T) {
	b := trials(1, map[string]bool{"a": true, "b": true, "only-b": true})
	c := trials(1, map[string]bool{"a": true, "b": false})
	c = append(c, evalkit.Trial{SampleID: "a", SampleVersion: 2, Scores: b[0].Scores}) // 版本不同不配对
	r, err := Run(Input{Mode: ModeFactor, Baseline: Side{Done: true, ID: 1, Snapshot: snap(), Trials: b},
		Candidate: Side{Done: true, ID: 2, Snapshot: snap(), Trials: c}, Paired: []string{"a@1", "b@1"}, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	if r.Paired != 2 || len(r.Excluded) != 2 || r.Excluded[0].Key != "a@2" || r.Excluded[1].Key != "only-b@1" {
		t.Fatalf("paired = %d excluded = %+v", r.Paired, r.Excluded)
	}
	if m := r.Metrics[0]; m.Samples != 2 || m.BaselineMean != 1 || m.CandidateMean != 0.5 {
		t.Fatalf("citation metric = %+v", m)
	}
	var sb strings.Builder
	if err := Render(&sb, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"配对样本（(sample_id, sample_version) 交集）：2", "被排除样本：2", "a@2（仅 candidate）",
		"only-b@1（仅 baseline）", "seed 3", "未接入（L2/L3）"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("report missing %q", want)
		}
	}
}

func TestPairedBootstrap_KnownAnswer(t *testing.T) {
	// 常数差值：任何重抽均值都等于该常数。
	if iv := PairedBootstrap([]float64{0.25, 0.25, 0.25}, DefaultResamples, 1); iv != (Interval{0.25, 0.25, 0.25}) {
		t.Fatalf("constant = %+v", iv)
	}
	// 固定 seed：结果可复现，且区间含均值。
	diffs := []float64{-1, 0, 0, 1, 1, 1}
	a, b := PairedBootstrap(diffs, DefaultResamples, 42), PairedBootstrap(diffs, DefaultResamples, 42)
	if a != b || a.Mean != 1.0/3 || a.Low > a.Mean || a.High < a.Mean || a.Low < -1 || a.High > 1 {
		t.Fatalf("bootstrap = %+v vs %+v", a, b)
	}
	if slices.Equal(bootstrapMeans(diffs, 50, 42), bootstrapMeans(diffs, 50, 43)) {
		t.Fatal("different seed should resample differently")
	}
	// 已知答案（PCG 固定 seed 42，1000 次，重抽均值为 k/6）：换随机源或分位口径时须有意识地更新。
	if wantLow, wantHigh := -2.0/6, 5.0/6; a.Low != wantLow || a.High != wantHigh {
		t.Fatalf("seed 42 interval = [%v, %v], want [%v, %v]", a.Low, a.High, wantLow, wantHigh)
	}
	if got := Percentile([]float64{1, 2, 3, 4}, 0.95); got != 4 {
		t.Fatalf("p95 = %v", got)
	}
}

// overCons 造一个实验：每个样本 2 次 trial，refusal_correct 中 over 次标过度保守。
func overCons(over int, ids ...string) []evalkit.Trial {
	var out []evalkit.Trial
	for _, id := range ids {
		for i := range 2 {
			sc := evalkit.Score{Name: "refusal_correct", RedLine: true, Pass: true, Value: 1}
			if i < over {
				sc.Value, sc.Evidence = 0, "over_conservative: x"
			}
			out = append(out, evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: i, Scores: []evalkit.Score{sc}})
		}
	}
	return out
}

func TestAA_KnownAnswer(t *testing.T) {
	// 第 1、2 次相同，第 3 次每个样本 2 次中 1 次过度保守：过度保守率样本级差值恒为 0.5。
	// 合并三对重抽均值的绝对值：1-2 对 100 个 0，1-3、2-3 对 200 个 0.5 → 95% 分位 = 0.5。
	same := overCons(0, "a", "b", "c")
	runs := []Side{{Done: true, ID: 1, Snapshot: snap(), Trials: same}, {Done: true, ID: 2, Snapshot: snap(), Trials: same},
		{Done: true, ID: 3, Snapshot: snap(), Trials: overCons(1, "a", "b", "c")}}
	th, err := AA(runs, 5, 100, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Metrics) != 6 || th.SignedBy != "" || th.Quantile != 0.95 || len(th.Experiments) != 3 || th.Tags == nil || len(th.Tags) != 0 {
		t.Fatalf("thresholds = %+v", th)
	}
	byName := map[string]MetricThreshold{}
	for _, m := range th.Metrics {
		byName[m.Name] = m
	}
	oc := byName["over_conservative_rate"]
	if oc.Threshold == nil || *oc.Threshold != 0.5 || len(oc.PairwiseDiffs) != 3 || oc.PairwiseDiffs[0].Mean != 0 || oc.PairwiseDiffs[1].Mean != 0.5 {
		t.Fatalf("over_conservative threshold = %+v", oc)
	}
	// 拒答正确率是红线：样本任一 trial 过度保守即 0，差值恒为 1。
	if ra := byName["refusal_accuracy"]; ra.Threshold == nil || *ra.Threshold != 1 {
		t.Fatalf("refusal_accuracy threshold = %+v", ra)
	}
	// 无 citation_valid 维度：无适用样本、null。
	if c := byName["citation_coverage"]; c.Threshold != nil || c.Note != "无适用样本" {
		t.Fatalf("citation = %+v", c)
	}
	for _, name := range []string{"fact_consistency", "hallucination_rate"} {
		if m := byName[name]; m.Threshold != nil || !strings.Contains(m.Note, "L2") {
			t.Fatalf("judge metric %s must be null pending L2: %+v", name, m)
		}
	}
	// 只有一对：两次相同 → 阈值 0（stub 下的情形）。
	th, err = AA(runs[:2], 5, 100, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v := th.Metrics[2].Threshold; v == nil || *v != 0 || !strings.Contains(strings.Join(th.Notes, ""), "A/A 差值为 0") {
		t.Fatalf("identical runs threshold = %v notes = %v", v, th.Notes)
	}
	other := snap()
	other.PromptVersion = "v2"
	if _, err := AA([]Side{{Done: true, ID: 1, Snapshot: snap(), Trials: same}, {Done: true, ID: 2, Snapshot: other, Trials: same}},
		5, 10, time.Unix(0, 0)); !errors.Is(err, ErrAANotSameSnapshot) {
		t.Fatalf("err = %v, want ErrAANotSameSnapshot", err)
	}
}

func TestFactorTable(t *testing.T) {
	good := `factors:
  - {name: a, layer: L4, baseline: b, candidate: c, metrics: [m], suites: [s], rollback: r}
`
	if _, err := ParseFactorTable([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"missing rollback": "factors:\n  - {name: a, baseline: b, candidate: c, metrics: [m], suites: [s]}\n",
		"empty metrics":    "factors:\n  - {name: a, baseline: b, candidate: c, metrics: [], suites: [s], rollback: r}\n",
		"blank suite":      "factors:\n  - {name: a, baseline: b, candidate: c, metrics: [m], suites: [\" \"], rollback: r}\n",
		"no factors":       "factors: []\n",
		"duplicate":        "factors:\n  - {name: a, baseline: b, candidate: c, metrics: [m], suites: [s], rollback: r}\n  - {name: a, baseline: b, candidate: c, metrics: [m], suites: [s], rollback: r}\n",
	}
	for name, raw := range bad {
		if _, err := ParseFactorTable([]byte(raw)); !errors.Is(err, ErrFactorIncomplete) {
			t.Errorf("%s: err = %v, want ErrFactorIncomplete", name, err)
		}
	}
	if _, err := ParseFactorTable([]byte("factors:\n  - {name: a, rolback: r}\n")); err == nil {
		t.Error("unknown field must fail")
	}
}

// TestRepoFactorTable 校验入库的 eval/gates/factors.yaml 每行五项齐全（验收 §6.1-4）。
func TestRepoFactorTable(t *testing.T) {
	path := filepath.Join("..", "..", "..", "eval", "gates", "factors.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	ft, err := LoadFactorTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ft.Factors) != 8 {
		t.Fatalf("want 8 candidate factors (§8: 3 rule offload + loader + compaction + prompt + tier + judge), got %d", len(ft.Factors))
	}
}

// done 造一个 DONE 的对比边。
func done(id uint64, s evalkit.Snapshot, tr []evalkit.Trial) Side {
	return Side{Done: true, ID: id, Snapshot: s, Trials: tr}
}

// TestRun_FailClosed：数据不完整或不可比时 Run 返回哨兵错误，不给出任何结论（更谈不上放行）。
func TestRun_FailClosed(t *testing.T) {
	tr := trials(2, map[string]bool{"a": true, "b": true})
	pairs := []string{"a@1", "b@1"}
	v2 := make([]evalkit.Trial, len(tr))
	for i, x := range tr {
		x.SampleVersion = 2
		v2[i] = x
	}
	naOnly := []evalkit.Trial{{SampleID: "a", SampleVersion: 1,
		Scores: []evalkit.Score{{Name: "citation_valid", Pass: true, NotApplicable: true}}}}
	var fromNull, fromEmpty evalkit.Snapshot
	if err := json.Unmarshal([]byte("null"), &fromNull); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte("{}"), &fromEmpty); err != nil {
		t.Fatal(err)
	}
	n3, suites2, hash2 := snap(), snap(), snap()
	n3.N = 3
	suites2.Suites = []string{"regression", "redteam"}
	hash2.SamplesHash = "h2"
	running := done(9, snap(), tr)
	running.Done = false
	tests := []struct {
		name string
		in   Input
		want error
	}{
		{"candidate RUNNING with 0 trials", Input{Mode: ModeFactor, Baseline: done(1, snap(), tr),
			Candidate: Side{ID: 2, Snapshot: snap()}, Paired: pairs}, ErrNotDone},
		{"baseline has no trials", Input{Mode: ModeFactor, Baseline: done(1, snap(), nil), Candidate: done(2, snap(), tr),
			Paired: pairs}, ErrNoTrials},
		{"all sample_versions differ", Input{Mode: ModeFactor, Baseline: done(1, snap(), tr), Candidate: done(2, snap(), v2)},
			ErrNoPairs},
		{"no metric applies", Input{Mode: ModeFactor, Baseline: done(1, snap(), naOnly), Candidate: done(2, snap(), naOnly),
			Paired: []string{"a@1"}}, ErrNoMetric},
		{"snapshot JSON null", Input{Mode: ModeFactor, Baseline: done(1, fromNull, tr), Candidate: done(2, snap(), tr),
			Paired: pairs}, ErrNoSnapshot},
		{"snapshot {}", Input{Mode: ModePhase, Baseline: done(1, snap(), tr), Candidate: done(2, fromEmpty, tr),
			Paired: pairs}, ErrNoSnapshot},
		{"factor n differs", Input{Mode: ModeFactor, Baseline: done(1, snap(), tr), Candidate: done(2, n3, tr),
			Paired: pairs}, ErrIncomparable},
		{"factor suites differ", Input{Mode: ModeFactor, Baseline: done(1, snap(), tr), Candidate: done(2, suites2, tr),
			Paired: pairs}, ErrIncomparable},
		{"phase suites differ", Input{Mode: ModePhase, Baseline: done(1, snap(), tr), Candidate: done(2, suites2, tr),
			Paired: pairs}, ErrIncomparable},
		{"phase n differs", Input{Mode: ModePhase, Baseline: done(1, snap(), tr), Candidate: done(2, n3, tr),
			Paired: pairs}, ErrIncomparable},
		{"reference in factor mode", Input{Mode: ModeFactor, Baseline: done(1, snap(), tr), Candidate: done(2, snap(), tr),
			Reference: &Side{Done: true, ID: 3, Snapshot: snap(), Trials: tr}, Paired: pairs}, ErrReferencePhaseOnly},
		{"reference RUNNING", Input{Mode: ModePhase, Baseline: done(1, snap(), tr), Candidate: done(2, snap(), tr),
			Reference: &running, Paired: pairs}, ErrNotDone},
		{"reference samples_hash differs", Input{Mode: ModePhase, Baseline: done(1, snap(), tr), Candidate: done(2, snap(), tr),
			Reference: &Side{Done: true, ID: 3, Snapshot: hash2, Trials: tr}, Paired: pairs}, ErrIncomparable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Run(tt.in)
			if !errors.Is(err, tt.want) || r != nil {
				t.Fatalf("Run = %+v, %v; want %v", r, err, tt.want)
			}
		})
	}
	// suites 只是顺序不同：可比。
	both, swapped := snap(), snap()
	both.Suites, swapped.Suites = []string{"redteam", "regression"}, []string{"regression", "redteam"}
	if _, err := Check(Factors, ModeFactor, both, swapped); err != nil {
		t.Fatalf("suites order must not matter: %v", err)
	}
}

// crashed 造 n 次 infra_error trial（样本 id、version 1）。
func crashed(id string, from, n int) []evalkit.Trial {
	out := make([]evalkit.Trial, 0, n)
	for i := range n {
		out = append(out, evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: from + i, Error: "boom"})
	}
	return out
}

// TestRun_ErrorTrials_FourOfFiveCrash：candidate 一个样本 5 次中 4 次 infra_error、1 次通过 → 红线按失败、结论 INCOMPLETE、
// 不可放行，报告列出受影响样本。
func TestRun_ErrorTrials_FourOfFiveCrash(t *testing.T) {
	ids := map[string]bool{"b": true, "c": true, "d": true}
	base := append(trials(5, ids), trials(5, map[string]bool{"a": false})...)
	cand := append(trials(5, ids), trials(1, map[string]bool{"a": true})...)
	cand = append(cand, crashed("a", 1, 4)...)
	r, err := Run(Input{Mode: ModeFactor, Baseline: done(1, snap(), base), Candidate: done(2, snap(), cand),
		Paired: []string{"a@1", "b@1", "c@1", "d@1"}, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Conclusion != ConclusionIncomplete || r.Releasable() {
		t.Fatalf("conclusion = %s releasable = %t", r.Conclusion, r.Releasable())
	}
	if got := r.ErrorSamples[sideCandidate]; len(got) != 1 || got[0] != "a@1" {
		t.Fatalf("candidate error samples = %v", got)
	}
	if m := r.Metrics[0]; m.Samples != 4 || m.CandidateMean != 0.75 {
		t.Fatalf("citation metric = %+v (crashed trials must count as failures)", m)
	}
	var b strings.Builder
	if err := Render(&b, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"candidate：infra_error 4、grader_error 0；受影响样本 1", "a@1", "INCOMPLETE"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report missing %q", want)
		}
	}
}

// TestRun_ErrorTrials_AllCrash：candidate 10 个样本 5 次全部 infra_error → 样本不从配对里消失（红线按失败计 0），
// 结论不可放行（退化优先于不完整），10 个样本都列为受影响。
func TestRun_ErrorTrials_AllCrash(t *testing.T) {
	pass := map[string]bool{}
	var cand []evalkit.Trial
	var pairs []string
	for i := range 10 {
		id := fmt.Sprintf("s%02d", i)
		pass[id] = true
		cand = append(cand, crashed(id, 0, 5)...)
		pairs = append(pairs, id+"@1")
	}
	r, err := Run(Input{Mode: ModeFactor, Baseline: done(1, snap(), trials(5, pass)), Candidate: done(2, snap(), cand),
		Paired: pairs, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Releasable() || r.Conclusion != ConclusionRegressed {
		t.Fatalf("conclusion = %s releasable = %t", r.Conclusion, r.Releasable())
	}
	if m := r.Metrics[0]; m.Samples != 10 || m.CandidateMean != 0 {
		t.Fatalf("citation metric = %+v, all-crash samples must stay paired", m)
	}
	if n := len(r.ErrorSamples[sideCandidate]); n != 10 || !r.Incomplete() {
		t.Fatalf("error samples = %d", n)
	}
}

// TestAA_FailClosed：A/A 重复 id、不足两次、未 DONE、有出错 trial、n / suites / samples_hash 不同都拒绝。
func TestAA_FailClosed(t *testing.T) {
	same := overCons(0, "a", "b")
	ok := func(id uint64) Side { return done(id, snap(), same) }
	n3, suites2, hash2 := snap(), snap(), snap()
	n3.N = 3
	suites2.Suites = []string{"regression", "redteam"}
	hash2.SamplesHash = "h2"
	notDone := ok(2)
	notDone.Done = false
	tests := []struct {
		name string
		runs []Side
		want error
	}{
		{"duplicate", []Side{ok(1), ok(2), ok(1)}, ErrAADuplicate},
		{"one run", []Side{ok(1)}, ErrAATooFew},
		{"not done", []Side{ok(1), notDone}, ErrNotDone},
		{"no trials", []Side{ok(1), done(2, snap(), nil)}, ErrNoTrials},
		{"error trials", []Side{ok(1), done(2, snap(), append(slices.Clone(same), crashed("a", 2, 1)...))}, ErrAAErrors},
		{"n differs", []Side{ok(1), done(2, n3, same)}, ErrAANotSameSnapshot},
		{"suites differ", []Side{ok(1), done(2, suites2, same)}, ErrAANotSameSnapshot},
		{"samples_hash differs", []Side{ok(1), done(2, hash2, same)}, ErrAANotSameSnapshot},
		{"empty snapshot", []Side{ok(1), done(2, evalkit.Snapshot{}, same)}, ErrNoSnapshot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := AA(tt.runs, 5, 10, time.Unix(0, 0)); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestRun_BaselineUncovered_Incomplete：baseline 有而 candidate 未配对的样本（candidate 只跑一部分、或样本只改了版本）
// 数 > 0 时单因子结论为 INCOMPLETE、不可放行，报告逐条列出；candidate 多出的样本不影响结论。
func TestRun_BaselineUncovered_Incomplete(t *testing.T) {
	all := map[string]bool{"a": true, "b": true, "c": true, "d": true}
	base := trials(2, all)
	subset := trials(2, map[string]bool{"a": true, "b": true})
	bumped := trials(2, map[string]bool{"b": true, "c": true, "d": true})
	bumped = append(bumped, evalkit.Trial{SampleID: "a", SampleVersion: 2, Scores: base[0].Scores})
	extra := append(trials(2, all), trials(2, map[string]bool{"new": true})...)
	tests := []struct {
		name      string
		cand      []evalkit.Trial
		paired    []string
		uncovered []string
		want      Conclusion
	}{
		{"candidate ran a subset", subset, []string{"a@1", "b@1"}, []string{"c@1", "d@1"}, ConclusionIncomplete},
		{"sample version bumped", bumped, []string{"b@1", "c@1", "d@1"}, []string{"a@1"}, ConclusionIncomplete},
		{"candidate has extra samples", extra, []string{"a@1", "b@1", "c@1", "d@1"}, nil, ConclusionNoSig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Run(Input{Mode: ModeFactor, Baseline: done(1, snap(), base), Candidate: done(2, snap(), tt.cand),
				Paired: tt.paired, Seed: 1})
			if err != nil {
				t.Fatal(err)
			}
			if r.Conclusion != tt.want || r.Releasable() != (tt.want == ConclusionNoSig) {
				t.Fatalf("conclusion = %s releasable = %t, want %s", r.Conclusion, r.Releasable(), tt.want)
			}
			if got := r.Uncovered(); !slices.Equal(got, tt.uncovered) {
				t.Fatalf("uncovered = %v, want %v", got, tt.uncovered)
			}
			var b strings.Builder
			if err := Render(&b, r); err != nil {
				t.Fatal(err)
			}
			if len(tt.uncovered) > 0 {
				want := fmt.Sprintf("baseline 样本未被 candidate 配对：%d：%s", len(tt.uncovered), strings.Join(tt.uncovered, "、"))
				if !strings.Contains(b.String(), want) {
					t.Fatalf("report missing %q:\n%s", want, b.String())
				}
			}
		})
	}
	// 超过 maxListed 也全部列出（门禁据此判 INCOMPLETE，清单不截断）。
	many := map[string]bool{}
	for i := range maxListed + 5 {
		many[fmt.Sprintf("s%03d", i)] = true
	}
	r, err := Run(Input{Mode: ModeFactor, Baseline: done(1, snap(), trials(1, many)), Candidate: done(2, snap(), trials(1, map[string]bool{"s000": true})),
		Paired: []string{"s000@1"}, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := Render(&b, r); err != nil {
		t.Fatal(err)
	}
	if n := len(r.Uncovered()); n != maxListed+4 || !strings.Contains(b.String(), fmt.Sprintf("s%03d@1", maxListed+4)) {
		t.Fatalf("uncovered = %d, last sample must be listed", n)
	}
}

// TestCheck_TagsRunParam：快照记录样本筛选条件 tags，作为运行参数在两种模式下都要求一致（顺序无关），不算因子；
// 未记录 tags（nil）的快照按缺字段拒绝。
func TestCheck_TagsRunParam(t *testing.T) {
	tagged := snap()
	tagged.Tags = []string{"L2", "protect"}
	swapped := snap()
	swapped.Tags = []string{"protect", "L2"}
	for _, mode := range []Mode{ModeFactor, ModePhase} {
		if _, err := Check(Factors, mode, snap(), tagged); !errors.Is(err, ErrIncomparable) || !strings.Contains(err.Error(), "tags") {
			t.Fatalf("%s: tags differ: err = %v, want ErrIncomparable", mode, err)
		}
		if d, err := Check(Factors, mode, tagged, swapped); err != nil || len(d) != 0 {
			t.Fatalf("%s: tags order must not matter: %v %v", mode, d, err)
		}
	}
	if len(Diff(Factors, snap(), tagged)) != 0 {
		t.Fatal("tags must not be a factor")
	}
	noTags := snap()
	noTags.Tags = nil
	if _, err := Check(Factors, ModeFactor, snap(), noTags); !errors.Is(err, ErrNoSnapshot) || !strings.Contains(err.Error(), "tags") {
		t.Fatalf("nil tags: err = %v, want ErrNoSnapshot", err)
	}
}

// TestRequireComplete_Format：knowledge_ref 每段须以 kc: 开头，suites 每个元素非空；格式不对按快照不完整拒绝。
func TestRequireComplete_Format(t *testing.T) {
	tests := map[string]func(*evalkit.Snapshot){
		"knowledge_ref WORKTREE":       func(s *evalkit.Snapshot) { s.KnowledgeRef = "WORKTREE" },
		"knowledge_ref second segment": func(s *evalkit.Snapshot) { s.KnowledgeRef = kcA + ",WORKTREE" },
		"knowledge_ref empty segment":  func(s *evalkit.Snapshot) { s.KnowledgeRef = kcA + "," },
		"knowledge_ref bare prefix":    func(s *evalkit.Snapshot) { s.KnowledgeRef = evalkit.KnowledgeContentPrefix },
		"suites empty element":         func(s *evalkit.Snapshot) { s.Suites = []string{"regression", ""} },
		"suites blank element":         func(s *evalkit.Snapshot) { s.Suites = []string{" "} },
		"tags blank element":           func(s *evalkit.Snapshot) { s.Tags = []string{"L2", ""} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := snap()
			mutate(&s)
			if err := RequireComplete("candidate", s); !errors.Is(err, ErrNoSnapshot) {
				t.Fatalf("err = %v, want ErrNoSnapshot", err)
			}
		})
	}
	multi := snap()
	multi.KnowledgeRef = kcA + "," + kcB
	if err := RequireComplete("candidate", multi); err != nil {
		t.Fatalf("multi kc: refs must pass: %v", err)
	}
}

// exp104Snapshot 是共享库 exp 104 的快照形状（跑在 4684a8d：knowledge_ref 仍是 WORKTREE，没有 tags）。
func exp104Snapshot() evalkit.Snapshot {
	s := snap()
	s.GitSHA = "4684a8d" + strings.Repeat("0", 33)
	s.KnowledgeRef = "WORKTREE"
	s.Tags = nil
	return s
}

// TestRun_RejectsOldFormatSnapshot_Exp104：旧格式快照（exp 104）作为 compare 任一方都被拒绝（ErrNoSnapshot），不给结论。
func TestRun_RejectsOldFormatSnapshot_Exp104(t *testing.T) {
	tr := trials(2, map[string]bool{"a": true})
	onlyRef := exp104Snapshot()
	onlyRef.Tags = []string{}
	for name, old := range map[string]evalkit.Snapshot{"exp 104 as stored": exp104Snapshot(), "knowledge_ref only": onlyRef} {
		for _, side := range []string{sideBaseline, sideCandidate} {
			t.Run(name+"/"+side, func(t *testing.T) {
				in := Input{Mode: ModeFactor, Baseline: done(1, snap(), tr), Candidate: done(104, snap(), tr), Paired: []string{"a@1"}}
				if side == sideBaseline {
					in.Baseline.Snapshot = old
				} else {
					in.Candidate.Snapshot = old
				}
				r, err := Run(in)
				if !errors.Is(err, ErrNoSnapshot) || r != nil || !strings.Contains(err.Error(), "knowledge_ref") {
					t.Fatalf("Run = %+v, %v; want ErrNoSnapshot naming knowledge_ref", r, err)
				}
			})
		}
	}
}
