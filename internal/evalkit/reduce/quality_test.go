package reduce

import (
	"testing"

	"agri-steward/internal/evalkit"
)

func metricByName(t *testing.T, name string) QualityMetric {
	t.Helper()
	for _, m := range QualityMetrics {
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

func TestQualityMetrics_Registry(t *testing.T) {
	if len(QualityMetrics) != 6 {
		t.Fatalf("want 6 quality metrics, got %d", len(QualityMetrics))
	}
	pending := 0
	seen := map[string]bool{}
	for _, m := range QualityMetrics {
		if seen[m.Name()] || m.Title() == "" {
			t.Fatalf("bad metric %q", m.Name())
		}
		seen[m.Name()] = true
		if m.Pending() != "" {
			pending++
			if _, ok := m.SampleValue([]evalkit.Trial{scored("a", 0)}); ok {
				t.Errorf("pending metric %s must not be applicable", m.Name())
			}
		}
	}
	if pending != 2 {
		t.Fatalf("want 2 pending (judge) metrics, got %d", pending)
	}
}

func TestSampleValues(t *testing.T) {
	cite := func(pass bool) evalkit.Score { return evalkit.Score{Name: dimCitationValid, RedLine: true, Pass: pass} }
	refusal := func(pass bool, ev string) evalkit.Score {
		return evalkit.Score{Name: dimRefusalCorrect, RedLine: true, Pass: pass, Evidence: ev}
	}
	trials := []evalkit.Trial{
		// a：引用 flaky（1 次失败）→ 红线按失败，样本值 0。
		scored("a", 0, cite(true), refusal(true, "")), scored("a", 1, cite(false), refusal(true, "over_conservative: x")),
		// b：第 1 次通过、第 2 次 infra_error → 红线按失败计 0（失败关闭）；非红线指标跳过出错 trial；出错不算 flaky。
		scored("b", 0, cite(true), refusal(true, "")), {SampleID: "b", SampleVersion: 1, TrialIndex: 1, Error: "boom"},
		// c：引用不适用（默认放行）与 grader_error，都不参与。
		scored("c", 0, evalkit.Score{Name: dimCitationValid, Pass: true, NotApplicable: true}),
		scored("c", 1, evalkit.Score{Name: dimCitationValid, Error: "x"}),
	}
	tests := []struct {
		metric string
		want   []SampleValue
	}{
		{"citation_coverage", []SampleValue{{Key: "a@1", Value: 0, Flaky: true}, {Key: "b@1", Value: 0}}},
		{"refusal_accuracy", []SampleValue{{Key: "a@1", Value: 0, Flaky: true}, {Key: "b@1", Value: 0}}},
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
	v, ok := m.SampleValue([]evalkit.Trial{scored("a", 0, evalkit.Score{Name: dimRefusalCorrect, RedLine: true})})
	if !ok || v != 0 {
		t.Fatalf("missed refusal = %v %v, want 0 true", v, ok)
	}
	if !metricByName(t, "over_conservative_rate").LowerIsBetter() || m.LowerIsBetter() {
		t.Fatal("lower-is-better flags wrong")
	}
}

// TestRedLine_ErrorTrialsFail：红线指标里出错 trial 按失败计。5 次中 4 次 infra_error、1 次通过 → 0；
// 全部出错（适用性未知）→ 适用、值 0，样本不会从配对里消失；某维度 grader_error 同样按失败；非红线指标只看正常 trial。
func TestRedLine_ErrorTrialsFail(t *testing.T) {
	cite := metricByName(t, "citation_coverage")
	pass := evalkit.Score{Name: dimCitationValid, RedLine: true, Pass: true}
	crash := func(i int) evalkit.Trial {
		return evalkit.Trial{SampleID: "a", SampleVersion: 1, TrialIndex: i, Error: "boom"}
	}
	fourCrash := []evalkit.Trial{scored("a", 0, pass), crash(1), crash(2), crash(3), crash(4)}
	if v, ok := cite.SampleValue(fourCrash); !ok || v != 0 {
		t.Fatalf("4/5 crash = %v %v, want 0 true", v, ok)
	}
	allCrash := []evalkit.Trial{crash(0), crash(1), crash(2), crash(3), crash(4)}
	if v, ok := cite.SampleValue(allCrash); !ok || v != 0 {
		t.Fatalf("5/5 crash = %v %v, want 0 true (must not drop out)", v, ok)
	}
	graderErr := []evalkit.Trial{scored("a", 0, pass), scored("a", 1, evalkit.Score{Name: dimCitationValid, RedLine: true, Error: "x"})}
	if v, ok := cite.SampleValue(graderErr); !ok || v != 0 {
		t.Fatalf("grader_error = %v %v, want 0 true", v, ok)
	}
	if got := SampleValues(cite, fourCrash); len(got) != 1 || got[0].Flaky {
		t.Fatalf("error trials must not mark flaky: %+v", got)
	}
	over := metricByName(t, "over_conservative_rate")
	if v, ok := over.SampleValue(allCrash); ok {
		t.Fatalf("non-red-line metric on all-crash sample = %v, want not applicable", v)
	}
}
