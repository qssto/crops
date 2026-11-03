package reduce

import (
	"math"
	"reflect"
	"testing"

	"agri-steward/internal/evalkit"
)

func suiteTrial(suite, id string, i int, pass bool) evalkit.Trial {
	tr := trial(id, i, red("citation_valid", pass), q("must_call", false, 0))
	tr.Suite = suite
	return tr
}

// TestBySuite（P1 L8）：按套件聚合红线 pass^k；redteam 跑满 5 次才判 pass^5，非红线失败不计。
func TestBySuite(t *testing.T) {
	var trials []evalkit.Trial
	for i := range NightlyK {
		trials = append(trials,
			suiteTrial("regression", "a", i, true),
			suiteTrial("redteam", "x", i, true),
			suiteTrial("redteam", "y", i, i != 2), // 4/5
		)
	}
	trials = append(trials, suiteTrial("holdout", "h", 0, true)) // n=1 < 5，不判定
	got := Reduce(trials).Suites
	want := []SuiteStat{
		{Suite: "regression", Samples: 1, Trials: 5, MinN: 5, MeanPassK: 1, AllPassSamples: 1, PassKK: 1},
		{Suite: "redteam", Samples: 2, Trials: 10, MinN: 5, MeanPassK: 0.5, AllPassSamples: 1, PassKK: 0.5},
		{Suite: "holdout", Samples: 1, Trials: 1, MinN: 1, MeanPassK: 1, AllPassSamples: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("suites = %+v, want %d", got, len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Suite != w.Suite || g.Samples != w.Samples || g.Trials != w.Trials || g.MinN != w.MinN ||
			g.AllPassSamples != w.AllPassSamples || math.Abs(g.MeanPassK-w.MeanPassK) > 1e-9 ||
			math.Abs(g.PassKK-w.PassKK) > 1e-9 {
			t.Errorf("suite[%d] = %+v, want %+v", i, g, w)
		}
	}
	if got[2].Judged() || !got[1].Judged() {
		t.Errorf("judged: redteam=%v holdout=%v, want true/false", got[1].Judged(), got[2].Judged())
	}
}

// TestBySuite_InfraErrorFails：运行出错的 trial 计入 n、不计通过（套件 pass^k 不能因跳过出错而虚高）。
func TestBySuite_InfraErrorFails(t *testing.T) {
	trials := []evalkit.Trial{suiteTrial("redteam", "x", 0, true), suiteTrial("redteam", "x", 1, true)}
	trials[1].Error = "trial x#1: db down"
	got := Reduce(trials).Suites
	if len(got) != 1 || got[0].MeanPassK != 0 || got[0].AllPassSamples != 0 || got[0].Trials != 2 {
		t.Fatalf("suites = %+v, want pass^k 0 over 2 trials", got)
	}
}

// na 是默认放行的红线打分（样本未声明该维度期望）。
func na(name string) evalkit.Score {
	return evalkit.Score{Name: name, RedLine: true, Pass: true, Value: 1, NotApplicable: true}
}

// TestBySuite_ApplicabilityAndGraderErrors（P1-L8-A 审查修复）：套件内全是默认放行的红线列入 Inapplicable，
// 有一个适用 trial 即不列；grader_error 计入套件 GraderErrors；维度 Applicable 只数非默认放行。
func TestBySuite_ApplicabilityAndGraderErrors(t *testing.T) {
	mk := func(suite, id string, i int, scores ...evalkit.Score) evalkit.Trial {
		tr := trial(id, i, scores...)
		tr.Suite = suite
		return tr
	}
	r := Reduce([]evalkit.Trial{
		mk("redteam", "x", 0, red("citation_valid", true), na("injection_ignored"), na("forbidden_text")),
		mk("redteam", "y", 0, red("citation_valid", true), red("injection_ignored", true), na("forbidden_text")),
		mk("regression", "a", 0, red("citation_valid", true), na("injection_ignored"), graderErr("forbidden_text")),
	})
	tests := []struct {
		suite        string
		inapplicable []string
		graderErrors int
		allPass      bool
	}{
		{"redteam", []string{"forbidden_text"}, 0, true},
		{"regression", []string{"injection_ignored", "forbidden_text"}, 1, false}, // grader_error 不算通过
	}
	for i, tt := range tests {
		st := r.Suites[i]
		if st.Suite != tt.suite || !reflect.DeepEqual(st.Inapplicable, tt.inapplicable) || st.GraderErrors != tt.graderErrors ||
			st.AllPass() != tt.allPass {
			t.Errorf("suite[%d] = %+v, want %s inapplicable=%v grader_errors=%d all_pass=%v",
				i, st, tt.suite, tt.inapplicable, tt.graderErrors, tt.allPass)
		}
	}
	wantApplicable := map[string]int{"citation_valid": 3, "injection_ignored": 1, "forbidden_text": 0}
	for _, d := range r.Dims {
		if d.Applicable != wantApplicable[d.Name] {
			t.Errorf("dim %s applicable = %d, want %d", d.Name, d.Applicable, wantApplicable[d.Name])
		}
		if got, want := d.NoApplicable(), d.Name == "forbidden_text"; got != want {
			t.Errorf("dim %s NoApplicable = %v, want %v", d.Name, got, want)
		}
	}
}
