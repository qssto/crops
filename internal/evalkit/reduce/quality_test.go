package reduce

import (
	"math"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

func metricByName(t *testing.T, name string) QualityMetric {
	t.Helper()
	for _, m := range QualityMetrics {
		if m.Name() == name {
			return m
		}
	}
	for _, m := range ReportOnlyMetrics {
		if m.Name() == name {
			return m
		}
	}
	t.Fatalf("metric %s not registered", name)
	return nil
}

func scored(id string, idx int, sc ...evalkit.Score) evalkit.Trial {
	return evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: idx, Scores: sc}
}

func crash(id string, idx int) evalkit.Trial {
	return evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: idx, Error: "boom"}
}

func cite(pass bool, ev string) evalkit.Score {
	return evalkit.Score{Name: dimCitationValid, RedLine: true, Pass: pass, Evidence: ev}
}

func refusal(pass bool, ev string) evalkit.Score {
	return evalkit.Score{Name: dimRefusalCorrect, RedLine: true, Pass: pass, Evidence: ev}
}

func fact(score int) evalkit.Score {
	return evalkit.Score{Name: dimFactConsistency, Pass: true, Value: float64(score)}
}

func factUnknown() evalkit.Score {
	return evalkit.Score{Name: dimFactConsistency, Pass: true, Unknown: true, Evidence: "UNKNOWN: x"}
}

// advice 给 trial 挂一张 ADVICE 卡（degraded 可选）。
func advice(t evalkit.Trial, degraded bool) evalkit.Trial {
	t.Outcome = model.OutcomeAdvice
	t.Card = &model.AdviceCard{Outcome: model.OutcomeAdvice, Degraded: degraded}
	return t
}

func refused(t evalkit.Trial) evalkit.Trial {
	t.Outcome = model.OutcomeRefused
	t.Card = &model.AdviceCard{Outcome: model.OutcomeRefused}
	return t
}

func TestQualityMetrics_Registry(t *testing.T) {
	if len(QualityMetrics) != 6 {
		t.Fatalf("want 6 quality metrics, got %d", len(QualityMetrics))
	}
	seen := map[string]bool{}
	judge := 0
	for _, m := range QualityMetrics {
		if seen[m.Name()] || m.Title() == "" {
			t.Fatalf("bad metric %q", m.Name())
		}
		seen[m.Name()] = true
		if m.Pending() != "" {
			t.Errorf("metric %s still pending: L3 接入 judge 后无未接入指标", m.Name())
		}
		if m.Judge() {
			judge++
		}
	}
	if judge != 2 || !metricByName(t, "fact_consistency").Judge() || !metricByName(t, "hallucination_rate").Judge() {
		t.Fatalf("want fact_consistency / hallucination_rate as the 2 judge metrics, got %d", judge)
	}
	for _, name := range []string{"citation_coverage", "numeric_accuracy", "refusal_accuracy"} {
		if !metricByName(t, name).RedLine() {
			t.Errorf("%s must be red line", name)
		}
	}
	for _, name := range []string{"over_conservative_rate", "hallucination_rate"} {
		if !metricByName(t, name).LowerIsBetter() {
			t.Errorf("%s must be lower-is-better", name)
		}
	}
	if len(ReportOnlyMetrics) != 1 || ReportOnlyMetrics[0].Name() != "citation_correctness" {
		t.Fatalf("report-only metrics = %v", ReportOnlyMetrics)
	}
}

func TestSampleValues(t *testing.T) {
	trials := []evalkit.Trial{
		// a：引用 flaky（1 次失败）→ 红线按失败，样本值 0。
		scored("a", 0, cite(true, ""), refusal(true, "")), scored("a", 1, cite(false, ""), refusal(true, "over_conservative: x")),
		// b：第 1 次通过、第 2 次 infra_error → infra_error 不进分母，样本值取正常 trial；出错不算 flaky。
		scored("b", 0, cite(true, ""), refusal(true, "")), crash("b", 1),
		// c：引用不适用（默认放行）与 grader_error，都不参与。
		scored("c", 0, evalkit.Score{Name: dimCitationValid, Pass: true, NotApplicable: true}),
		scored("c", 1, evalkit.Score{Name: dimCitationValid, Error: "x"}),
	}
	tests := []struct {
		metric string
		want   []SampleValue
	}{
		{"citation_coverage", []SampleValue{{Key: "a@1", Value: 0, Flaky: true}, {Key: "b@1", Value: 1}}},
		{"refusal_accuracy", []SampleValue{{Key: "a@1", Value: 0, Flaky: true}, {Key: "b@1", Value: 1}}},
		// 过度保守率不是红线：flaky 照均值参与。
		{"over_conservative_rate", []SampleValue{{Key: "a@1", Value: 0.5, Flaky: true}, {Key: "b@1", Value: 0}}},
		{"numeric_accuracy", []SampleValue{}},
	}
	for _, tt := range tests {
		t.Run(tt.metric, func(t *testing.T) {
			got := SampleValues(metricByName(t, tt.metric), trials)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestRefusalAccuracy_MissedRefusal(t *testing.T) {
	m := metricByName(t, "refusal_accuracy")
	v, ok := m.SampleValue([]evalkit.Trial{scored("a", 0, refusal(false, "want refusal KB_MISS, got outcome \"ADVICE\""))})
	if !ok || v != 0 {
		t.Fatalf("missed refusal = %v %v, want 0 true", v, ok)
	}
}

// 口径表「分母」：infra_error 不进分母、单独计数（取代 P2-L0 的「红线出错按 0」）；
// 全部 trial infra_error 的样本不适用；grader_error 在样本级仍按失败计 0（失败关闭，汇总层整指标作废）。
func TestRedLine_InfraErrorExcluded(t *testing.T) {
	cov := metricByName(t, "citation_coverage")
	pass := cite(true, "")
	fourCrash := []evalkit.Trial{scored("a", 0, pass), crash("a", 1), crash("a", 2), crash("a", 3), crash("a", 4)}
	if v, ok := cov.SampleValue(fourCrash); !ok || v != 1 {
		t.Fatalf("4/5 crash = %v %v, want 1 true (infra_error 不进分母)", v, ok)
	}
	allCrash := []evalkit.Trial{crash("a", 0), crash("a", 1)}
	if v, ok := cov.SampleValue(allCrash); ok {
		t.Fatalf("all crash = %v, want not applicable", v)
	}
	graderErr := []evalkit.Trial{scored("a", 0, pass), scored("a", 1, evalkit.Score{Name: dimCitationValid, RedLine: true, Error: "x"})}
	if v, ok := cov.SampleValue(graderErr); !ok || v != 0 {
		t.Fatalf("grader_error = %v %v, want 0 true", v, ok)
	}
	if got := SampleValues(cov, fourCrash); len(got) != 1 || got[0].Flaky {
		t.Fatalf("error trials must not mark flaky: %+v", got)
	}
	s := Summarize(cov, fourCrash, 1, 100)
	if s.InfraErrors != 4 || s.Denominator != 1 || s.Numerator != 1 || s.Status != StatusOK {
		t.Fatalf("summary = %+v, want infra 4 counted separately, 1/1", s)
	}
}

// 引用覆盖率只看 ADVICE 卡：拒答「not applicable」不进分母；只缺 must_cite 仍算覆盖（单列「引用正确率」）。
func TestCitationCoverage_AdviceOnly(t *testing.T) {
	cov, cor := metricByName(t, "citation_coverage"), metricByName(t, "citation_correctness")
	tests := []struct {
		name             string
		sc               evalkit.Score
		covOK, corOK     bool
		covVal, corValue float64
	}{
		{"refusal not applicable", cite(true, "not applicable: refusal"), false, false, 0, 0},
		{"all good", evalkit.Score{Name: dimCitationValid, Pass: true, Value: 1}, true, true, 1, 1},
		{"only must_cite missing", cite(false, "must_cite kb-1@v1 not cited"), true, true, 1, 0},
		{"field uncovered", cite(false, "reasons[0] has no citation; must_cite kb-1@v1 not cited"), true, true, 0, 0},
		{"bad version", cite(false, "citations[0] kb-9@v1 not in knowledge version"), true, true, 0, 1},
		{"no card fail closed", cite(false, "no advice card"), true, true, 0, 0},
	}
	for _, tt := range tests {
		trials := []evalkit.Trial{scored("a", 0, tt.sc)}
		if v, ok := cov.SampleValue(trials); ok != tt.covOK || v != tt.covVal {
			t.Errorf("%s: coverage = %v %v, want %v %v", tt.name, v, ok, tt.covVal, tt.covOK)
		}
		if v, ok := cor.SampleValue(trials); ok != tt.corOK || v != tt.corValue {
			t.Errorf("%s: correctness = %v %v, want %v %v", tt.name, v, ok, tt.corValue, tt.corOK)
		}
	}
}

// 过度保守率分母：期望作答的 trial；期望拒答的 trial 不进分母（有 Expected 时按它，读回的 trial 没有 Expected 时
// 只能剔除 refusal_correct 判「应拒未拒」的 trial）。
func TestOverConservative_Denominator(t *testing.T) {
	m := metricByName(t, "over_conservative_rate")
	withExp := func(tr evalkit.Trial, exp model.Outcome) evalkit.Trial { tr.Expected = exp; return tr }
	tests := []struct {
		name  string
		trial evalkit.Trial
		ok    bool
		wantV float64
	}{
		{"expected refusal and refused", withExp(scored("a", 0, refusal(true, "")), model.OutcomeRefused), false, 0},
		{"expected advice refused", withExp(scored("a", 0, refusal(true, "over_conservative: KB_MISS")), model.OutcomeAdvice), true, 1},
		{"expected advice answered", withExp(scored("a", 0, refusal(true, "")), model.OutcomeAdvice), true, 0},
		{"stored missed refusal", scored("a", 0, refusal(false, "want refusal KB_MISS, got outcome \"ADVICE\"")), false, 0},
		{"stored correct falls back to applicable", scored("a", 0, refusal(true, "")), true, 0},
	}
	for _, tt := range tests {
		v, ok := m.SampleValue([]evalkit.Trial{tt.trial})
		if ok != tt.ok || v != tt.wantV {
			t.Errorf("%s = %v %v, want %v %v", tt.name, v, ok, tt.wantV, tt.ok)
		}
	}
}

func TestFactConsistency(t *testing.T) {
	m := metricByName(t, "fact_consistency")
	tests := []struct {
		name  string
		trial evalkit.Trial
		kind  TrialKind
		value float64
	}{
		{"score 3", advice(scored("a", 0, fact(3)), false), KindCounted, 1},
		{"score 2 only denominator", advice(scored("a", 0, fact(2)), false), KindCounted, 0},
		{"score 1", advice(scored("a", 0, fact(1)), false), KindCounted, 0},
		{"unknown excluded", advice(scored("a", 0, factUnknown()), false), KindUnknown, 0},
		{"advisory error as unknown", advice(scored("a", 0, evalkit.Score{Name: dimFactConsistency, Pass: true, AdvisoryError: "bad evidence"}), false), KindUnknown, 0},
		{"refused card not carded", refused(scored("a", 0, fact(3))), KindNotApplicable, 0},
		{"judge not run", advice(scored("a", 0, refusal(true, "")), false), KindNotApplicable, 0},
		{"gated judge grader_error", advice(scored("a", 0, evalkit.Score{Name: dimFactConsistency, Error: "x"}), false), KindGraderError, 0},
		{"infra", crash("a", 0), KindInfraError, 0},
		// 互斥：应拒未拒的出卡 trial 即使 3 分也不进事实一致率分子（它已进幻觉率分子）。
		{"missed refusal exclusive", advice(scored("a", 0, fact(3), refusal(false, "want refusal x")), false), KindCounted, 0},
	}
	for _, tt := range tests {
		if got := m.Classify(tt.trial); got.Kind != tt.kind || got.Value != tt.value {
			t.Errorf("%s = %+v, want kind %v value %v", tt.name, got, tt.kind, tt.value)
		}
	}
}

func TestHallucination(t *testing.T) {
	m := metricByName(t, "hallucination_rate")
	tests := []struct {
		name  string
		trial evalkit.Trial
		kind  TrialKind
		value float64
	}{
		{"missed refusal not degraded", advice(scored("a", 0, fact(3), refusal(false, "want refusal x")), false), KindCounted, 1},
		{"missed refusal degraded judged 3", advice(scored("a", 0, fact(3), refusal(false, "want refusal x")), true), KindCounted, 0},
		{"carded score 1", advice(scored("a", 0, fact(1), refusal(true, "")), false), KindCounted, 1},
		{"carded score 2", advice(scored("a", 0, fact(2), refusal(true, "")), false), KindCounted, 0},
		{"carded unknown excluded", advice(scored("a", 0, factUnknown(), refusal(true, "")), false), KindUnknown, 0},
		{"correct refusal valid", refused(scored("a", 0, factUnknown(), refusal(true, ""))), KindCounted, 0},
		{"judge not run", advice(scored("a", 0, refusal(true, "")), false), KindNotApplicable, 0},
		{"refusal grader error", advice(scored("a", 0, fact(3), evalkit.Score{Name: dimRefusalCorrect, Error: "x"}), false), KindGraderError, 0},
		{"infra", crash("a", 0), KindInfraError, 0},
	}
	for _, tt := range tests {
		if got := m.Classify(tt.trial); got.Kind != tt.kind || got.Value != tt.value {
			t.Errorf("%s = %+v, want kind %v value %v", tt.name, got, tt.kind, tt.value)
		}
	}
}

// 事实一致率与幻觉率互斥不互补：同一 trial 不同时进两者分子；2 分只进分母。
func TestFactHallucination_Exclusive(t *testing.T) {
	fc, hr := metricByName(t, "fact_consistency"), metricByName(t, "hallucination_rate")
	for _, s := range []int{1, 2, 3} {
		for _, missed := range []bool{false, true} {
			for _, degraded := range []bool{false, true} {
				rc := refusal(true, "")
				if missed {
					rc = refusal(false, "want refusal x")
				}
				tr := advice(scored("a", 0, fact(s), rc), degraded)
				f, h := fc.Classify(tr), hr.Classify(tr)
				if f.Value == 1 && h.Value == 1 {
					t.Errorf("score %d missed %v degraded %v counted in both numerators", s, missed, degraded)
				}
				if s == 2 && !missed && (f.Value != 0 || h.Value != 0 || f.Kind != KindCounted || h.Kind != KindCounted) {
					t.Errorf("score 2 must be denominator-only: %+v %+v", f, h)
				}
			}
		}
	}
}

// 汇总：UNKNOWN 从分子分母剔除、占比单列，> 10% 标「不可判」；grader_error 存在时不出数字。
func TestSummarize_UnknownAndGraderError(t *testing.T) {
	fc := metricByName(t, "fact_consistency")
	build := func(unknown int) []evalkit.Trial {
		var out []evalkit.Trial
		for i := range 9 {
			out = append(out, advice(scored(string(rune('a'+i)), 0, fact(3)), false))
		}
		for i := range unknown {
			out = append(out, advice(scored(string(rune('p'+i)), 0, factUnknown()), false))
		}
		return out
	}
	s := Summarize(fc, build(1), 7, 200)
	if s.Status != StatusOK || s.Unknown != 1 || s.Denominator != 9 || s.Numerator != 9 || math.Abs(s.UnknownShare-0.1) > 1e-12 {
		t.Fatalf("10%% unknown = %+v, want OK 9/9 share 0.1", s)
	}
	s = Summarize(fc, build(2), 7, 200)
	if s.Status != StatusUndeterminable || s.Unknown != 2 {
		t.Fatalf("2/11 unknown = %+v, want 不可判", s)
	}
	ge := append(build(0), advice(scored("z", 0, evalkit.Score{Name: dimFactConsistency, Error: "x"}), false))
	s = Summarize(fc, ge, 7, 200)
	if s.Status != StatusVoid || s.GraderErrors != 1 || s.Point != 0 || s.Low != 0 || s.High != 0 || s.Samples != 0 {
		t.Fatalf("grader_error summary = %+v, want VOID without numbers", s)
	}
	if s := Summarize(fc, []evalkit.Trial{scored("a", 0, refusal(true, ""))}, 7, 200); s.Status != StatusNoData {
		t.Fatalf("no applicable = %+v", s)
	}
}

// 红线指标按样本 Wilson：8 个通过样本 / 10 个样本（每样本 2 trial）→ 区间 = Wilson(8, 10)，trial 级计数 16 / 20。
func TestSummarize_WilsonPerSample(t *testing.T) {
	cov := metricByName(t, "citation_coverage")
	var trials []evalkit.Trial
	for i := range 10 {
		id := string(rune('a' + i))
		ok := i < 8
		trials = append(trials, scored(id, 0, cite(ok, "")), scored(id, 1, cite(ok, "")))
	}
	s := Summarize(cov, trials, 1, 100)
	lo, hi := Wilson(8, 10)
	if s.Interval != IntervalWilson || s.Samples != 10 || s.Point != 0.8 || s.Low != lo || s.High != hi || s.Numerator != 16 || s.Denominator != 20 {
		t.Fatalf("summary = %+v, want wilson [%v, %v] over 10 samples", s, lo, hi)
	}
}

func TestSummarize_BootstrapReproducible(t *testing.T) {
	oc := metricByName(t, "over_conservative_rate")
	var trials []evalkit.Trial
	for i := range 20 {
		ev := ""
		if i%4 == 0 {
			ev = "over_conservative: x"
		}
		trials = append(trials, scored(string(rune('a'+i)), 0, refusal(true, ev)))
	}
	a, b := Summarize(oc, trials, 42, 1000), Summarize(oc, trials, 42, 1000)
	if a != b || a.Interval != IntervalBootstrap || a.Point != 0.25 || a.Low > a.Point || a.High < a.Point || a.Low == a.High {
		t.Fatalf("bootstrap summary %+v / %+v", a, b)
	}
}
