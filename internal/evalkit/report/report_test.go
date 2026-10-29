package report

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// 各 trial 的打分：schema_valid 为红线，outcome_match 不是。
var (
	scPass  = []evalkit.Score{{Name: "schema_valid", RedLine: true, Pass: true, Value: 1}, {Name: "outcome_match", Pass: true, Value: 1}}
	scFail  = []evalkit.Score{{Name: "schema_valid", RedLine: true, Pass: true, Value: 1}, {Name: "outcome_match", Evidence: `outcome = "ADVICE"`}}
	scGrErr = []evalkit.Score{{Name: "schema_valid", RedLine: true, Error: "grader schema_valid on s-3#0: boom"}, {Name: "outcome_match", Pass: true, Value: 1}}
)

func testExperiment() *evalkit.Experiment {
	start := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	return &evalkit.Experiment{
		ID: "20261028T010000-abcdef", Suites: []string{"regression"}, N: 2, Samples: 2,
		StartedAt: start, FinishedAt: start.Add(time.Second),
		Trials: []evalkit.Trial{
			{SampleID: "s-1", Tags: []string{"L4", "protect.timing"}, TrialIndex: 0, Expected: model.OutcomeRefused,
				Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS", Latency: 2 * time.Millisecond, Scores: scPass,
				RunID: 41, Transcript: "transcript/20261028T010000-abcdef/s-1/0.jsonl"},
			{SampleID: "s-1", Tags: []string{"L4", "protect.timing"}, TrialIndex: 1, Expected: model.OutcomeRefused,
				Outcome: model.OutcomeAdvice, Latency: 4 * time.Millisecond, UnexpectedCalls: []string{"device.list", "weather.alert"}, Scores: scFail,
				RunID: 42},
			{SampleID: "s-2", Tags: []string{"L2"}, TrialIndex: 0, Expected: model.OutcomeAdvice,
				Error: "trial s-2#0: a|b\nc", Latency: time.Millisecond},
			{SampleID: "s-3", TrialIndex: 0, Expected: model.OutcomeRefused, Outcome: model.OutcomeRefused, Scores: scGrErr},
		},
	}
}

func TestRender(t *testing.T) {
	var b strings.Builder
	if err := Render(&b, testExperiment()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	tests := []struct {
		name string
		want string
	}{
		{name: "title", want: "# 评测报告 20261028T010000-abcdef"},
		{name: "overview counts", want: "| ADVICE / REFUSED / error | 1 / 2 / 1 |"},
		{name: "matched", want: "| outcome 与期望一致 | 2 / 4 |"},
		{name: "status counts", want: "| PASS / FAIL / INFRA_ERROR / GRADER_ERROR | 1 / 1 / 1 / 1 |"},
		{name: "dim schema_valid", want: "| schema_valid | 是 | 2 | 0 | 1 | 1.00 |"},
		{name: "dim outcome_match", want: "| outcome_match | 否 | 2 | 1 | 0 | 0.67 |"},
		{name: "red line header", want: "| sample_id | schema_valid | 全部红线 |"},
		{name: "pass^k all passed", want: "| s-1 | 2/2 → 1.00 | 2/2 → 1.00 |"},
		{name: "pass^k infra error counts as fail", want: "| s-2 | 0/1 → 0.00 | 0/1 → 0.00 |"},
		{name: "pass^k grader error counts as fail", want: "| s-3 | 0/1 → 0.00 | 0/1 → 0.00 |"},
		{name: "tag L4", want: "| L4 | 1 | 2 | 1 | 1 | 0 | 1 | 3.000ms |"},
		{name: "tag L2", want: "| L2 | 1 | 1 | 0 | 0 | 1 | 0 | 1.000ms |"},
		{name: "trial row", want: "| s-1 | 0 | [41](transcript/20261028T010000-abcdef/s-1/0.jsonl) | REFUSED | REFUSED | KB_MISS | 2.000ms | - | - | PASS | - |"},
		{name: "unexpected calls", want: "| s-1 | 1 | 42 | REFUSED | ADVICE | - | 4.000ms | device.list, weather.alert | - | FAIL | outcome_match: outcome = \"ADVICE\" |"},
		{name: "error escaped", want: `| s-2 | 0 | - | ADVICE | - | - | 1.000ms | - | trial s-2#0: a\|b c | INFRA_ERROR | - |`},
		{name: "grader error row", want: "| GRADER_ERROR | schema_valid: grader_error grader schema_valid on s-3#0: boom |"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(out, tt.want) {
				t.Fatalf("report missing %q:\n%s", tt.want, out)
			}
		})
	}
}

func TestWriteFile(t *testing.T) {
	root := t.TempDir()
	path, err := WriteFile(root, testExperiment())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "20261028T010000-abcdef", FileName); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# 评测报告") {
		t.Fatalf("unexpected content: %s", raw)
	}

	exp := testExperiment()
	exp.ID = ""
	if _, err := WriteFile(root, exp); err == nil {
		t.Fatal("want error for empty experiment id")
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
			if got := passHatK(tt.n, tt.c, tt.k); math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("pass^k = %v, want %v", got, tt.want)
			}
		})
	}
}
