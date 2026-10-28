package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
)

func TestTrialInsert(t *testing.T) {
	pass := []evalkit.Score{{Name: "outcome_match", Pass: true, Value: 1}}
	exp := &evalkit.Experiment{FinishedAt: time.Unix(0, 0).UTC(), Trials: []evalkit.Trial{
		{SampleID: "a", Suite: "regression", RunID: 42, Scores: pass},
		{SampleID: "a", Suite: "regression", TrialIndex: 1, Error: "x"},
		{SampleID: "a", Suite: "regression", TrialIndex: 2, Scores: []evalkit.Score{
			{Name: "schema_valid", RedLine: true, Error: "grader schema_valid on a#2: boom"}}},
	}}
	q, args, err := trialInsert(7, exp)
	if err != nil {
		t.Fatal(err)
	}
	const cols = 10
	if strings.Count(q, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)") != len(exp.Trials) || strings.Count(q, "?") != len(args) {
		t.Fatalf("query/args mismatch: %s (%d args)", q, len(args))
	}
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"run_id", args[4], uint64(42)},
		{"status pass", args[5], StatusPass},
		{"passed", args[6], true},
		{"scores", args[7], `{"outcome_match":{"pass":true,"value":1}}`},
		{"infra run_id null", args[cols+4], nil},
		{"infra status", args[cols+5], StatusInfraError},
		{"infra scores", args[cols+7], `{}`},
		{"grader error status", args[2*cols+5], StatusGraderError},
		{"grader error not passed", args[2*cols+6], false},
		{"grader error scores", args[2*cols+7],
			`{"schema_valid":{"pass":false,"value":0,"red_line":true,"error":"grader schema_valid on a#2: boom"}}`},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %#v, want %#v", tt.name, tt.got, tt.want)
		}
	}
}

func TestStart_BadGitSHA(t *testing.T) {
	_, err := New(nil).Start(t.Context(), []string{"regression"}, TriggerManual, "abc", time.Unix(0, 0))
	if !errors.Is(err, ErrBadGitSHA) {
		t.Fatalf("err = %v, want ErrBadGitSHA", err)
	}
}
