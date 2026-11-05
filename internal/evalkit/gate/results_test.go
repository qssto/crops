package gate

import (
	"encoding/json"
	"slices"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

func TestRunResults(t *testing.T) {
	applied := &service.RunDetail{Timeline: []repository.EventRow{{Type: model.EventCompactionApplied, Payload: json.RawMessage(`{}`)}}}
	other := &service.RunDetail{Timeline: []repository.EventRow{{Type: model.EventCompactionFailed, Payload: json.RawMessage(`{}`)}}}
	trials := []evalkit.Trial{
		{SampleID: "a", Detail: applied},
		{SampleID: "a", TrialIndex: 1, Detail: other, UnexpectedCalls: []string{"get_weather"}, RunID: 7},
		{SampleID: "h", Suite: "holdout", Holdout: true, UnexpectedCalls: []string{"secret_tool"}},
		{SampleID: "b"},
	}
	n, calls := RunResults(trials)
	if n != 1 {
		t.Fatalf("compactions = %d, want 1", n)
	}
	if len(calls) != 2 || calls[0].SampleID != "a" || calls[0].RunID != 7 || !slices.Equal(calls[0].Tools, []string{"get_weather"}) {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[1].SampleID != "h" || len(calls[1].Tools) != 0 {
		t.Fatalf("holdout call must carry ID only: %+v", calls[1])
	}
	if n, calls := RunResults(nil); n != 0 || calls != nil {
		t.Fatalf("empty = %d %v", n, calls)
	}
}
