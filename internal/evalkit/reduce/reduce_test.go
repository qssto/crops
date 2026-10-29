package reduce

import (
	"math"
	"reflect"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// 打分构造：red 为红线维度，q 为质量维度。
func red(name string, pass bool) evalkit.Score {
	v := 0.0
	if pass {
		v = 1
	}
	return evalkit.Score{Name: name, RedLine: true, Pass: pass, Value: v}
}

func graderErr(name string) evalkit.Score {
	return evalkit.Score{Name: name, RedLine: true, Error: "grader " + name + ": boom"}
}

func q(name string, pass bool, v float64) evalkit.Score {
	return evalkit.Score{Name: name, Pass: pass, Value: v}
}

// gateScores 是 Gate 0 全部红线维度的打分；fail 中的维度记失败。
func gateScores(fail ...string) []evalkit.Score {
	bad := map[string]bool{}
	for _, f := range fail {
		bad[f] = true
	}
	var out []evalkit.Score
	for _, it := range Gate0Auto {
		for _, d := range it.Dims {
			out = append(out, red(d, !bad[d]))
		}
	}
	return out
}

func trial(id string, i int, scores ...evalkit.Score) evalkit.Trial {
	return evalkit.Trial{SampleID: id, TrialIndex: i, RunID: uint64(100 + i), Outcome: model.OutcomeRefused, Scores: scores}
}

func TestReduceSamples(t *testing.T) {
	infra := evalkit.Trial{SampleID: "s", TrialIndex: 2, Error: "trial s#2: db down"}
	tests := []struct {
		name        string
		trials      []evalkit.Trial
		wantAll     int
		wantN       int
		wantPassK   float64
		wantFlaky   []string // flaky 的红线维度；nil 表示不 flaky
		wantInfra   int
		wantGrader  int
		wantVoid    bool
		wantUnexpct int
	}{
		{name: "all pass", trials: []evalkit.Trial{
			trial("s", 0, red("schema_valid", true)), trial("s", 1, red("schema_valid", true)), trial("s", 2, red("schema_valid", true)),
		}, wantAll: 3, wantN: 3, wantPassK: 1},
		{name: "flaky red line", trials: []evalkit.Trial{
			trial("s", 0, red("schema_valid", true), red("pii_clean", true)),
			trial("s", 1, red("schema_valid", true), red("pii_clean", false)),
			trial("s", 2, red("schema_valid", true), red("pii_clean", true)),
		}, wantAll: 2, wantN: 3, wantPassK: 0, wantFlaky: []string{"pii_clean"}},
		{name: "all fail is not flaky", trials: []evalkit.Trial{
			trial("s", 0, red("pii_clean", false)), trial("s", 1, red("pii_clean", false)),
		}, wantAll: 0, wantN: 2, wantPassK: 0},
		{name: "quality dim failure does not break red line", trials: []evalkit.Trial{
			trial("s", 0, red("schema_valid", true), q("outcome_match", false, 0)),
		}, wantAll: 1, wantN: 1, wantPassK: 1},
		{name: "infra error counts as failure", trials: []evalkit.Trial{
			trial("s", 0, red("schema_valid", true)), trial("s", 1, red("schema_valid", true)), infra,
		}, wantAll: 2, wantN: 3, wantPassK: 0, wantFlaky: []string{"schema_valid"}, wantInfra: 1},
		{name: "grader error counts as failure and voids", trials: []evalkit.Trial{
			trial("s", 0, red("schema_valid", true)), trial("s", 1, graderErr("schema_valid")),
		}, wantAll: 1, wantN: 2, wantPassK: 0, wantFlaky: []string{"schema_valid"}, wantGrader: 1, wantVoid: true},
		{name: "unexpected call listed", trials: []evalkit.Trial{
			func() evalkit.Trial {
				tr := trial("s", 0, red("schema_valid", true))
				tr.UnexpectedCalls = []string{"device.list"}
				return tr
			}(),
		}, wantAll: 1, wantN: 1, wantPassK: 1, wantUnexpct: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Reduce(tt.trials)
			if len(r.Samples) != 1 {
				t.Fatalf("samples = %d, want 1", len(r.Samples))
			}
			s := r.Samples[0]
			if s.AllPass != tt.wantAll || s.N != tt.wantN {
				t.Fatalf("all/n = %d/%d, want %d/%d", s.AllPass, s.N, tt.wantAll, tt.wantN)
			}
			if got := s.PassK(); math.Abs(got-tt.wantPassK) > 1e-9 {
				t.Fatalf("pass^k = %v, want %v", got, tt.wantPassK)
			}
			var gotFlaky []string
			if len(r.Flaky) > 0 {
				gotFlaky = r.Flaky[0].Dims
			}
			if !reflect.DeepEqual(gotFlaky, tt.wantFlaky) {
				t.Fatalf("flaky dims = %v, want %v", gotFlaky, tt.wantFlaky)
			}
			if len(r.InfraErrors) != tt.wantInfra || len(r.GraderErrors) != tt.wantGrader || r.Void() != tt.wantVoid {
				t.Fatalf("infra/grader/void = %d/%d/%v, want %d/%d/%v", len(r.InfraErrors), len(r.GraderErrors), r.Void(),
					tt.wantInfra, tt.wantGrader, tt.wantVoid)
			}
			if len(r.UnexpectedCalls) != tt.wantUnexpct {
				t.Fatalf("unexpected calls = %v", r.UnexpectedCalls)
			}
		})
	}
}

func TestReduceDims(t *testing.T) {
	// s-1 两次 value 1、0 → 0.5；s-2 一次 1 → 1；样本均值 0.75（不是 trial 均值 0.67）。
	r := Reduce([]evalkit.Trial{
		trial("s-1", 0, red("schema_valid", true), q("outcome_match", true, 1)),
		trial("s-1", 1, graderErr("schema_valid"), q("outcome_match", false, 0)),
		trial("s-2", 0, red("schema_valid", true), q("outcome_match", true, 1)),
	})
	want := []Dim{
		{Name: "schema_valid", RedLine: true, Pass: 2, GraderErrors: 1, Mean: 1},
		{Name: "outcome_match", Pass: 2, Fail: 1, Mean: 0.75},
	}
	if len(r.Dims) != len(want) {
		t.Fatalf("dims = %d, want %d", len(r.Dims), len(want))
	}
	for i, w := range want {
		if got := *r.Dims[i]; got.Name != w.Name || got.RedLine != w.RedLine || got.Pass != w.Pass || got.Fail != w.Fail ||
			got.GraderErrors != w.GraderErrors || math.Abs(got.Mean-w.Mean) > 1e-9 {
			t.Fatalf("dim[%d] = %+v, want %+v", i, got, w)
		}
	}
	if !reflect.DeepEqual(r.RedLines, []string{"schema_valid"}) {
		t.Fatalf("red lines = %v", r.RedLines)
	}
}

func TestMetrics(t *testing.T) {
	card := &model.AdviceCard{Citations: []model.Citation{{Type: model.CitationType("KB")}}}
	detail := &service.RunDetail{Steps: []service.StepDetail{{LLMCalls: []service.LLMCallDetail{
		{LLMCallPayload: model.LLMCallPayload{PromptTokens: 100, CompletionTokens: 20}},
	}}}}
	r := Reduce([]evalkit.Trial{
		{SampleID: "a", Outcome: model.OutcomeAdvice, Card: card, Latency: 10 * time.Millisecond, Detail: detail, Scores: []evalkit.Score{red("x", true)}},
		{SampleID: "b", Outcome: model.OutcomeAdvice, Card: &model.AdviceCard{}, Latency: 30 * time.Millisecond, Scores: []evalkit.Score{red("x", true)}},
		{SampleID: "c", Outcome: model.OutcomeRefused, Latency: 20 * time.Millisecond, Scores: []evalkit.Score{red("x", false)}},
		{SampleID: "d", Error: "infra", Latency: time.Hour},
	})
	m := r.Metrics
	if m.CitationCoverage != (Ratio{Num: 1, Den: 2}) || m.RefusalRate != (Ratio{Num: 1, Den: 3}) {
		t.Fatalf("coverage %+v refusal %+v", m.CitationCoverage, m.RefusalRate)
	}
	if m.LatencyP95 != 30*time.Millisecond {
		t.Fatalf("p95 = %v, want 30ms (infra trial excluded)", m.LatencyP95)
	}
	if !m.TokensRecorded || m.Tokens != 120 {
		t.Fatalf("tokens = %d recorded=%v", m.Tokens, m.TokensRecorded)
	}
	if math.Abs(m.RedLinePassK-0.5) > 1e-9 { // a、b 通过，c、d 失败
		t.Fatalf("red line pass^k mean = %v, want 0.5", m.RedLinePassK)
	}
	if (Ratio{}).Value() != 0 || (Ratio{Num: 1, Den: 4}).Value() != 0.25 {
		t.Fatal("Ratio.Value")
	}
}

func TestGate0(t *testing.T) {
	three := func(id string, scores ...[]evalkit.Score) []evalkit.Trial {
		var out []evalkit.Trial
		for i, sc := range scores {
			out = append(out, trial(id, i, sc...))
		}
		return out
	}
	ok := gateScores()
	cat := func(ts ...[]evalkit.Trial) []evalkit.Trial {
		var out []evalkit.Trial
		for _, t := range ts {
			out = append(out, t...)
		}
		return out
	}
	tests := []struct {
		name    string
		trials  []evalkit.Trial
		want    []Verdict // #1 #2 #5
		failed1 []string
	}{
		{name: "all pass", trials: cat(three("a", ok, ok, ok), three("b", ok, ok, ok)),
			want: []Verdict{VerdictPass, VerdictPass, VerdictPass}},
		{name: "one flaky citation fails #1 only", trials: cat(three("a", ok, gateScores("citation_valid"), ok), three("b", ok, ok, ok)),
			want: []Verdict{VerdictFail, VerdictPass, VerdictPass}, failed1: []string{"a"}},
		{name: "n < 3 undecidable", trials: three("a", ok, ok),
			want: []Verdict{VerdictUndecidable, VerdictUndecidable, VerdictUndecidable}},
		{name: "grader error voids", trials: three("a", ok, ok, append(gateScores(), graderErr("schema_valid"))),
			want: []Verdict{VerdictUndecidable, VerdictUndecidable, VerdictUndecidable}},
		{name: "missing dim undecidable", trials: three("a", []evalkit.Score{red("pii_clean", true)}, []evalkit.Score{red("pii_clean", true)},
			[]evalkit.Score{red("pii_clean", true)}),
			want: []Verdict{VerdictUndecidable, VerdictUndecidable, VerdictPass}},
		{name: "no trials", want: []Verdict{VerdictUndecidable, VerdictUndecidable, VerdictUndecidable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Reduce(tt.trials).Gate0()
			if len(got) != len(tt.want) {
				t.Fatalf("gate items = %d, want %d", len(got), len(tt.want))
			}
			for i, w := range tt.want {
				if got[i].Verdict != w {
					t.Fatalf("#%d = %s (%s), want %s", got[i].Item.No, got[i].Verdict, got[i].Reason, w)
				}
				if w == VerdictUndecidable && got[i].Reason == "" {
					t.Fatalf("#%d undecidable without reason", got[i].Item.No)
				}
			}
			if !reflect.DeepEqual(got[0].Failed, tt.failed1) {
				t.Fatalf("#1 failed = %v, want %v", got[0].Failed, tt.failed1)
			}
		})
	}
}

func TestPassHatK(t *testing.T) {
	tests := []struct {
		name    string
		n, c, k int
		want    float64
	}{
		{name: "all pass", n: 3, c: 3, k: 3, want: 1},
		{name: "one fail with k=n", n: 3, c: 2, k: 3, want: 0},
		{name: "c=2 n=4 k=2", n: 4, c: 2, k: 2, want: 1.0 / 6},
		{name: "k > n invalid", n: 2, c: 2, k: 3, want: 0},
		{name: "k = 0 invalid", n: 2, c: 2, k: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PassHatK(tt.n, tt.c, tt.k); math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("pass^k = %v, want %v", got, tt.want)
			}
		})
	}
}
