package report

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// quarantinedExperiment：一个样本 3 次全过 + 一个隔离样本（未运行，无 trial）。
func quarantinedExperiment() *evalkit.Experiment {
	pass := []evalkit.Score{{Name: "schema_valid", RedLine: true, Pass: true, Value: 1}}
	return &evalkit.Experiment{ID: "x", N: 3, Samples: 1,
		Trials: []evalkit.Trial{
			{SampleID: "a", TrialIndex: 0, Scores: pass},
			{SampleID: "a", TrialIndex: 1, Scores: pass},
			{SampleID: "a", TrialIndex: 2, Scores: pass},
		},
		Quarantined: []evalkit.QuarantinedSample{{ID: "rice-rcpt-0001", Suite: "regression", Reason: "需 REAL 评测设施|见 dev-log"}},
	}
}

// TestRender_Quarantined（P1-L4 审查修复）：报告单列隔离样本与理由，总览给出隔离数，门禁行注明未覆盖；
// 隔离样本不进 PASS / INFRA_ERROR 计数、不进红线 pass^k、不进 Gate 0。应失败：隔离样本被当通过或 INFRA_ERROR。
func TestRender_Quarantined(t *testing.T) {
	exp := quarantinedExperiment()
	var b strings.Builder
	if err := Render(&b, exp); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"infra_error 0 / 3；隔离样本 1 个未运行，不计入门禁与红线统计（见「隔离样本」）\n",
		"| 样本数 | 1 |\n| 隔离样本（未运行，不计入 N / 通过 / INFRA_ERROR） | 1 |",
		"| PASS / FAIL / INFRA_ERROR / GRADER_ERROR | 3 / 0 / 0 / 0 |",
		"## 隔离样本\n\n",
		`| rice-rcpt-0001 | regression | 需 REAL 评测设施\|见 dev-log |`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "rice-rcpt-0001") != 1 {
		t.Fatalf("quarantined sample must only appear in its own section:\n%s", out)
	}
	r := reduce.Reduce(exp.Trials)
	for _, s := range r.Samples {
		if s.ID == "rice-rcpt-0001" {
			t.Fatalf("quarantined sample counted in reduce: %+v", s)
		}
	}
	for _, g := range r.Gate0() {
		if slices.Contains(g.Failed, "rice-rcpt-0001") {
			t.Fatalf("gate %d covers quarantined sample", g.Item.No)
		}
	}
	b.Reset()
	exp.Quarantined = nil
	if err := Render(&b, exp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "## 隔离样本") || strings.Contains(b.String(), "隔离样本 0 个") {
		t.Fatalf("no quarantine section expected:\n%s", b.String())
	}
}

// TestNewBaseline_Quarantined：baseline 记下隔离样本 id，samples / trials 不含它们。
func TestNewBaseline_Quarantined(t *testing.T) {
	b := NewBaseline("b", "bucket", quarantinedExperiment(), time.Unix(0, 0))
	if b.Samples != 1 || b.Trials != 3 || !slices.Equal(b.Quarantined, []string{"rice-rcpt-0001"}) {
		t.Fatalf("baseline samples = %d trials = %d quarantined = %v", b.Samples, b.Trials, b.Quarantined)
	}
}
