package reduce

import (
	"slices"
	"testing"

	"agri-steward/internal/evalkit"
)

func obsTrial(id string, idx int, o *evalkit.LoadObs) evalkit.Trial {
	return evalkit.Trial{SampleID: id, TrialIndex: idx, Load: o}
}

// loadTrials 是已知答案的 trial 集：
//   - p1（protect）：must_cite 2 条中 1 条被加载；条目 MUST/RANKED(引)/RANKED/COMMON，1 条无关；
//   - p1 第二次（protect）：同上但 RANKED 都被引用；
//   - f1（fertilize）：KB_MISS，无 must_cite、无条目；
//   - f2（fertilize）：must_cite 1 条加载；P2 前事件（pick_reason 空，按 RANKED）1 条未引用、无关；
//   - x1：INFRA_ERROR，不计；x2：无观测，不计。
func loadTrials() []evalkit.Trial {
	p := func(cited2 bool) *evalkit.LoadObs {
		return &evalkit.LoadObs{QuestionType: "protect", Topic: "protect.dosage", Strategies: []string{"v1"},
			MustCite: []string{"kb:a@v1", "kb:z@v1"},
			Items: []evalkit.LoadedItem{
				{Ref: "kb:s@v1", PickReason: "MUST_INCLUDE"},
				{Ref: "kb:a@v1", PickReason: "RANKED", Cited: true},
				{Ref: "kb:b@v1", PickReason: "RANKED", Cited: cited2, Irrelevant: true},
				{Ref: "kb:c@v1", PickReason: "COMMON", Cited: true},
			}}
	}
	infra := obsTrial("x1", 0, &evalkit.LoadObs{QuestionType: "protect", MustCite: []string{"kb:q@v1"}})
	infra.Error = "boom"
	return []evalkit.Trial{
		obsTrial("p1", 0, p(false)),
		obsTrial("p1", 1, p(true)),
		obsTrial("f1", 0, &evalkit.LoadObs{QuestionType: "fertilize", Topic: "fertilize.rate", KBMiss: true}),
		obsTrial("f2", 0, &evalkit.LoadObs{QuestionType: "fertilize", Topic: "fertilize.rate", Strategies: []string{"v0"},
			MustCite: []string{"kb:n@v1"}, Items: []evalkit.LoadedItem{{Ref: "kb:n@v1", Cited: false, Irrelevant: true}}}),
		infra,
		obsTrial("x2", 0, nil),
	}
}

func TestReduceLoad_KnownAnswer(t *testing.T) {
	r := ReduceLoad(loadTrials())
	if r.Observed != 4 || !slices.Equal(r.Strategies, []string{"v0", "v1"}) {
		t.Fatalf("observed = %d strategies = %v", r.Observed, r.Strategies)
	}
	// 列序：命中率、精度、无关注入率。
	want := map[string]struct {
		samples, trials int
		ratios          []Ratio
	}{
		"fertilize": {2, 2, []Ratio{{1, 1}, {0, 1}, {1, 1}}},
		"protect":   {1, 2, []Ratio{{2, 4}, {3, 4}, {2, 8}}},
		"合计":        {3, 4, []Ratio{{3, 5}, {3, 5}, {3, 9}}},
	}
	rows := append(slices.Clone(r.ByType), r.Total)
	if len(rows) != 3 || r.ByType[0].Key != "fertilize" || r.ByType[1].Key != "protect" {
		t.Fatalf("rows = %+v", rows)
	}
	for _, row := range rows {
		w := want[row.Key]
		if row.Samples != w.samples || row.Trials != w.trials || !slices.Equal(row.Ratios, w.ratios) {
			t.Errorf("%s = %+v, want %+v", row.Key, row, w)
		}
	}
}

func TestLoadMetrics_Registry(t *testing.T) {
	var names []string
	for _, m := range LoadMetrics {
		names = append(names, m.Name())
	}
	if !slices.Equal(names, []string{MetricLoadHitRate, MetricLoadPrecision, MetricLoadIrrelevant}) {
		t.Fatalf("names = %v", names)
	}
	if LoadMetrics[0].LowerIsBetter() || LoadMetrics[1].LowerIsBetter() || !LoadMetrics[2].LowerIsBetter() {
		t.Fatal("only irrelevant rate is lower-is-better")
	}
	for _, q := range QualityMetrics {
		if slices.Contains(names, q.Name()) {
			t.Fatalf("load metric %s leaked into QualityMetrics", q.Name())
		}
	}
	if r := ReduceLoad(nil); r.Observed != 0 || r.Total.Trials != 0 || len(r.ByType) != 0 {
		t.Fatalf("empty = %+v", r)
	}
}

func TestReduceLoadGaps_KnownAnswer(t *testing.T) {
	g := ReduceLoadGaps(loadTrials())
	if len(g.KBMiss) != 1 || g.KBMiss[0] != (KBMissGap{SampleID: "f1", QuestionType: "fertilize", Topic: "fertilize.rate", Trials: 1}) {
		t.Fatalf("kb_miss = %+v", g.KBMiss)
	}
	// protect.dosage 2/4 = 0.5 < 0.9 入清单；fertilize.rate 1/1 不入。
	if len(g.LowHit) != 1 {
		t.Fatalf("low_hit = %+v", g.LowHit)
	}
	lh := g.LowHit[0]
	if lh.Topic != "protect.dosage" || lh.Hit != (Ratio{2, 4}) || lh.HitRate != 0.5 ||
		!slices.Equal(lh.Missing, []string{"kb:z@v1"}) || !slices.Equal(lh.Samples, []string{"p1"}) || !slices.Equal(lh.Strategy, []string{"v1"}) {
		t.Fatalf("low_hit[0] = %+v", lh)
	}
	empty := ReduceLoadGaps(nil)
	if empty.KBMiss == nil || empty.LowHit == nil {
		t.Fatal("empty gaps must marshal as [] not null")
	}
}
