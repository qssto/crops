package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

func testExperiment() *evalkit.Experiment {
	start := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	return &evalkit.Experiment{
		ID: "20261028T010000-abcdef", Suites: []string{"regression"}, N: 2, Samples: 2,
		StartedAt: start, FinishedAt: start.Add(time.Second),
		Trials: []evalkit.Trial{
			{SampleID: "s-1", Tags: []string{"L4", "protect.timing"}, TrialIndex: 0, Expected: model.OutcomeRefused,
				Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS", Latency: 2 * time.Millisecond},
			{SampleID: "s-1", Tags: []string{"L4", "protect.timing"}, TrialIndex: 1, Expected: model.OutcomeRefused,
				Outcome: model.OutcomeAdvice, Latency: 4 * time.Millisecond, UnexpectedCalls: []string{"device.list", "weather.alert"}},
			{SampleID: "s-2", Tags: []string{"L2"}, TrialIndex: 0, Expected: model.OutcomeAdvice,
				Error: "trial s-2#0: a|b\nc", Latency: time.Millisecond},
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
		{name: "overview counts", want: "| ADVICE / REFUSED / error | 1 / 1 / 1 |"},
		{name: "matched", want: "| outcome 与期望一致 | 1 / 3 |"},
		{name: "tag L4", want: "| L4 | 1 | 2 | 1 | 1 | 0 | 1 | 3.000ms |"},
		{name: "tag L2", want: "| L2 | 1 | 1 | 0 | 0 | 1 | 0 | 1.000ms |"},
		{name: "trial row", want: "| s-1 | 0 | REFUSED | REFUSED | KB_MISS | 2.000ms | - | - |"},
		{name: "unexpected calls", want: "| s-1 | 1 | REFUSED | ADVICE | - | 4.000ms | device.list, weather.alert | - |"},
		{name: "error escaped", want: `| s-2 | 0 | ADVICE | - | - | 1.000ms | - | trial s-2#0: a\|b c |`},
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
