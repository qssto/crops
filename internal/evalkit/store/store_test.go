package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

func TestStatus(t *testing.T) {
	tests := []struct {
		name      string
		trial     evalkit.Trial
		want      string
		wantMatch bool
	}{
		{name: "pass", trial: evalkit.Trial{Expected: model.OutcomeRefused, Outcome: model.OutcomeRefused}, want: StatusPass, wantMatch: true},
		{name: "fail", trial: evalkit.Trial{Expected: model.OutcomeAdvice, Outcome: model.OutcomeRefused}, want: StatusFail},
		{name: "infra", trial: evalkit.Trial{Expected: model.OutcomeRefused, Outcome: model.OutcomeRefused, Error: "boom"}, want: StatusInfraError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, match := Status(tt.trial)
			if got != tt.want || match != tt.wantMatch {
				t.Fatalf("Status = %s,%v want %s,%v", got, match, tt.want, tt.wantMatch)
			}
		})
	}
}

func TestTrialInsert(t *testing.T) {
	exp := &evalkit.Experiment{FinishedAt: time.Unix(0, 0).UTC(), Trials: []evalkit.Trial{
		{SampleID: "a", Suite: "regression", Expected: model.OutcomeRefused, Outcome: model.OutcomeRefused},
		{SampleID: "a", Suite: "regression", TrialIndex: 1, Error: "x"},
	}}
	q, args, err := trialInsert(7, exp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(q, "(?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)") != 2 || strings.Count(q, "?") != len(args) {
		t.Fatalf("query/args mismatch: %s (%d args)", q, len(args))
	}
	if args[4] != StatusPass || args[13] != StatusInfraError || args[6] != `{"outcome_match":true}` {
		t.Fatalf("args = %v", args)
	}
}

func TestStart_BadGitSHA(t *testing.T) {
	_, err := New(nil).Start(t.Context(), []string{"regression"}, TriggerManual, "abc", time.Unix(0, 0))
	if !errors.Is(err, ErrBadGitSHA) {
		t.Fatalf("err = %v, want ErrBadGitSHA", err)
	}
}
