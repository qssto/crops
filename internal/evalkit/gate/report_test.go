package gate

import (
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
)

// TestFlips：配对样本在指标上的样本级值变化逐条列出（regressed / improved），方向按指标好坏口径。
func TestFlips(t *testing.T) {
	ids := sampleIDs(4)
	base := side(ids, 3, func(id string, _ int) []evalkit.Score {
		if id == "s03" {
			return []evalkit.Score{passed(dimCitation), passed(dimNumbers), overConservative()}
		}
		return healthy()
	})
	cand := side(ids, 3, func(id string, _ int) []evalkit.Score {
		if id == "s01" {
			return []evalkit.Score{passed(dimCitation), passed(dimNumbers), overConservative()}
		}
		return healthy()
	})
	fs := Flips(flipInput(base, cand, pairedKeys(base, cand)))
	var got []string
	for _, f := range fs {
		got = append(got, f.Metric+" "+f.Key+" "+string(f.Direction))
	}
	want := []string{
		"refusal_accuracy s01@1 regressed", "refusal_accuracy s03@1 improved",
		"over_conservative_rate s01@1 regressed", "over_conservative_rate s03@1 improved",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("flips = %v, want %v", got, want)
	}
	if Flips(flipInput(base, base, pairedKeys(base, base))) != nil {
		t.Fatal("identical sides must have no flips")
	}
	// 评审 P3：翻转清单复用 compare 的样本级配对（只在 Paired 交集内），交集外样本即使两边都有 trial 也不列。
	var paired []string
	for _, k := range pairedKeys(base, cand) {
		if k != "s01@1" {
			paired = append(paired, k)
		}
	}
	for _, f := range Flips(flipInput(base, cand, paired)) {
		if f.Key == "s01@1" {
			t.Fatalf("unpaired sample listed as flip: %+v", f)
		}
	}
}

// flipInput 是翻转清单用的配对输入（与 compare.Run 同一份 Input）。
func flipInput(base, cand []evalkit.Trial, paired []string) compare.Input {
	return compare.Input{Mode: compare.ModeFactor, Baseline: compare.Side{ID: 1, Done: true, Trials: base},
		Candidate: compare.Side{ID: 2, Done: true, Trials: cand}, Paired: paired}
}

// TestRender：首行是门禁结论；翻转样本逐条、flaky 清单、unexpected_call 清单依次出现；holdout 只给 ID。
func TestRender(t *testing.T) {
	ids := sampleIDs(10)
	base := side(ids, 5, allHealthy)
	cand := side(ids, 5, regressedRefusal(ids))
	cand[len(cand)-1].Scores = []evalkit.Score{passed(dimCitation), passed(dimNumbers), overConservative()} // s09 flaky
	in := build(t, base, cand)
	in.Compare.Candidate.UnexpectedCalls = []evalkit.UnexpectedCallRef{
		{SampleID: "s02", TrialIndex: 1, RunID: 7, Tools: []string{"get_weather"}},
		{SampleID: "h01", TrialIndex: 0},
	}
	d := mustEvaluate(t, in)
	var b strings.Builder
	err := Render(&b, ReportInput{Decision: d, Compare: in.Compare, Flips: Flips(flipInput(base, cand, pairedKeys(base, cand))),
		Cost: in.Cost, Signoff: "无需签字", Holdout: map[string]bool{"h01": true}})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	first, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(first, string(BlockQuality)) || !strings.Contains(first, "baseline 1") || !strings.Contains(first, "candidate 2") {
		t.Fatalf("first line = %q", first)
	}
	order := []string{"## 规则判定", "trust_regression", "## 翻转样本", "regressed refusal_accuracy s00@1",
		"## flaky", "s09@1", "## unexpected_call", "s02#1 run=7 get_weather", "h01#0（holdout，只给 ID）", "## 成本与延迟"}
	at := 0
	for _, s := range order {
		i := strings.Index(out[at:], s)
		if i < 0 {
			t.Fatalf("missing %q after offset %d in:\n%s", s, at, out)
		}
		at += i + len(s)
	}
	if strings.Contains(out, "improved") {
		t.Fatalf("no improved flips expected:\n%s", out)
	}
}

func TestRender_RequiresDecision(t *testing.T) {
	if err := Render(&strings.Builder{}, ReportInput{}); err == nil {
		t.Fatal("render without decision must fail")
	}
}
