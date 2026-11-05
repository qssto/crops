package gate

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/reduce"
	qjudge "agri-steward/internal/service/quality/judge"
)

// 构造数据用到的打分维度名（与 grader/code 的 Name() 一致）。
const (
	dimCitation = "citation_valid"
	dimNumbers  = "numbers_match_calc"
	dimRefusal  = "refusal_correct"
	dimFact     = "fact_consistency"
)

func snap() evalkit.Snapshot {
	return evalkit.Snapshot{GitSHA: strings.Repeat("a", 40), PromptVersion: "v1", ModelTier: "standard", Model: evalkit.ModelStub,
		KnowledgeRef: evalkit.KnowledgeContentPrefix + strings.Repeat("a", 64), SamplesHash: "h1", LoaderStrategy: evalkit.LoaderV0,
		RetrievalStrategy: evalkit.RetrievalProgressive, JudgeVersions: map[string]string{}, Suites: []string{"regression"},
		Tags: []string{}, N: 5, WallTime: "1m0s"}
}

func passed(dim string) evalkit.Score {
	return evalkit.Score{Name: dim, RedLine: true, Pass: true, Value: 1}
}

// overConservative 是 refusal_correct「应答而拒答」：Pass 为 true（不算红线失败），但拒答正确率记 0、过度保守率记 1。
func overConservative() evalkit.Score {
	return evalkit.Score{Name: dimRefusal, RedLine: true, Pass: true, Value: 1, Evidence: "over_conservative: 构造"}
}

// fact 是 judge 维度 fact_consistency 的打分（1-3 有序量表）。
func fact(v float64) evalkit.Score {
	return evalkit.Score{Name: dimFact, Pass: true, Value: v}
}

// healthy 是三项红线维度全过的打分。
func healthy() []evalkit.Score {
	return []evalkit.Score{passed(dimCitation), passed(dimNumbers), passed(dimRefusal)}
}

// side 造一边实验：ids 中每个样本 n 次 trial，打分取 scores(id, trialIndex)。
func side(ids []string, n int, scores func(id string, i int) []evalkit.Score) []evalkit.Trial {
	var out []evalkit.Trial
	for _, id := range ids {
		for i := range n {
			out = append(out, evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: i, Suite: "regression",
				Latency: 100 * time.Millisecond, Scores: scores(id, i)})
		}
	}
	return out
}

func allHealthy(string, int) []evalkit.Score { return healthy() }

func sampleIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%02d", i)
	}
	return ids
}

// pairedKeys 是两边 sample_id@version 的交集（同 store.PairedSamples）。
func pairedKeys(b, c []evalkit.Trial) []string {
	cs := map[string]bool{}
	for _, t := range c {
		cs[reduce.SampleKey(t)] = true
	}
	var out []string
	for _, t := range b {
		if k := reduce.SampleKey(t); cs[k] && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

func zero() *float64 { v := 0.0; return &v }

func ratio(v float64) *float64 { return &v }

// thresholds 是 thresholds.yaml 口径：非 judge 指标 0，judge 指标按 judgeTh（nil 即 yaml null）。
func thresholds(judgeTh *float64) map[string]*float64 {
	out := map[string]*float64{}
	for _, m := range reduce.QualityMetrics {
		if m.Judge() {
			out[m.Name()] = judgeTh
			continue
		}
		out[m.Name()] = zero()
	}
	return out
}

// limits 是 cost_latency.yaml 口径：三项都 10%，P95 在桩模型下只出报告。
func limits() []CostLimit {
	return []CostLimit{
		{Name: MetricTokensPerSession, MaxIncreaseRatio: ratio(0.1)},
		{Name: MetricLatencyP95, MaxIncreaseRatio: ratio(0.1), ReportOnlyOnStub: true},
		{Name: MetricCompactionRate, MaxIncreaseRatio: ratio(0.1)},
	}
}

func rated() map[string]JudgeDim {
	out := map[string]JudgeDim{}
	for _, d := range qjudge.Dimensions {
		out[d] = JudgeDim{Rated: true, Why: "构造：校准达标"}
	}
	return out
}

// build 用 compare.Run 在两边上算单因子对比，并组装门禁输入（judge 全部达标、成本持平、阈值同 thresholds.yaml）。
func build(t *testing.T, base, cand []evalkit.Trial, mut ...func(b, c *evalkit.Snapshot)) Input {
	t.Helper()
	bs, cs := snap(), snap()
	for _, m := range mut {
		m(&bs, &cs)
	}
	r, err := compare.Run(compare.Input{Mode: compare.ModeFactor,
		Baseline:  compare.Side{ID: 1, Done: true, Snapshot: bs, Trials: base},
		Candidate: compare.Side{ID: 2, Done: true, Snapshot: cs, Trials: cand},
		Paired:    pairedKeys(base, cand), Seed: compare.DefaultSeed})
	if err != nil {
		t.Fatalf("compare.Run: %v", err)
	}
	u := Usage{Sessions: 10, Tokens: 1000, Compactions: intp(0), LatencyP95: time.Second}
	return Input{Compare: r, Candidate: cand, Thresholds: thresholds(nil), Judge: rated(),
		Cost: CostInput{Limits: limits(), Baseline: u, Candidate: u, Stub: true}}
}

func intp(n int) *int { return &n }

func mustEvaluate(t *testing.T, in Input) *Decision {
	t.Helper()
	d, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return d
}

// finding 取规则 name 的判定记录（不存在即测试失败）。
func finding(t *testing.T, d *Decision, name string) Finding {
	t.Helper()
	for _, f := range d.Findings {
		if f.Rule == name {
			return f
		}
	}
	t.Fatalf("no finding for rule %s: %+v", name, d.Findings)
	return Finding{}
}
