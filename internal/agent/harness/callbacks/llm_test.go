package callbacks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agri-steward/internal/agent/harness/callbacks"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

func TestRecorder_LLMCallLinksRawAndStep(t *testing.T) {
	tests := []struct {
		name    string
		rawErr  error
		wantRaw bool
	}{
		{name: "raw stored", wantRaw: true},
		{name: "raw put failure still emits", rawErr: errors.New("minio down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, raw := &sink{}, &rawStore{err: tt.rawErr}
			rec, err := callbacks.New(callbacks.Config{Events: ev, Raw: raw, Clock: &tickClock{}})
			if err != nil {
				t.Fatal(err)
			}
			invoke(t, rec, build(t, rec, passNode), true)
			var calls []model.RunEvent
			for _, e := range ev.got {
				if e.Type == model.EventLLMCallEnd {
					calls = append(calls, e)
				}
			}
			if len(calls) != 1 {
				t.Fatalf("LLM_CALL_END events = %d, want 1", len(calls))
			}
			c := calls[0]
			if c.StepID != "b" {
				t.Fatalf("llm step_id = %q, want b (enclosing node)", c.StepID)
			}
			var p model.LLMCallPayload
			if err := json.Unmarshal(c.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Task != string(llm.TaskClassify) || p.Status != model.LLMStatusOK || p.PromptTokens != 12 ||
				p.CompletionTokens != 3 || p.LatencyMS != 40 || p.Error != "" {
				t.Fatalf("payload = %+v", p)
			}
			if tt.wantRaw != (c.RawObjectID != nil) {
				t.Fatalf("raw_object_id = %v, want linked=%v", c.RawObjectID, tt.wantRaw)
			}
			if !tt.wantRaw {
				return
			}
			put := raw.puts[0]
			if len(raw.puts) != 1 || put.Kind != model.RawLLMIO || put.RunID != runID || *c.RawObjectID != 101 {
				t.Fatalf("raw puts = %+v id = %d", raw.puts, *c.RawObjectID)
			}
			var body struct {
				StepID   string        `json:"step_id"`
				Messages []llm.Message `json:"messages"`
				Attempts []llm.Attempt `json:"attempts"`
			}
			if err := json.Unmarshal(put.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.StepID != "b" || len(body.Messages) != 1 || len(body.Attempts) != 1 {
				t.Fatalf("raw body = %s", put.Body)
			}
		})
	}
}

func TestRecorder_LLMErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "rate limited", err: errors.Join(llm.ErrUnavailable, llm.ErrRateLimited), want: "RATE_LIMITED"},
		{name: "bad reply", err: errors.Join(llm.ErrUnavailable, llm.ErrBadReply), want: "BAD_REPLY"},
		{name: "timeout", err: context.DeadlineExceeded, want: "TIMEOUT"},
		{name: "unavailable", err: llm.ErrUnavailable, want: "LLM_UNAVAILABLE"},
		{name: "other", err: errors.New("x"), want: "LLM_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := &sink{}
			rec, err := callbacks.New(callbacks.Config{Events: ev, Clock: &tickClock{}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := callbacks.WithRun(context.Background(), callbacks.Run{Scope: scope, RunID: runID})
			rec.ObserveLLM(ctx, llm.Call{Task: llm.TaskExplain, Err: tt.err})
			if len(ev.got) != 1 {
				t.Fatalf("events = %d", len(ev.got))
			}
			var p model.LLMCallPayload
			if err := json.Unmarshal(ev.got[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Status != model.LLMStatusError || p.Error != tt.want || ev.got[0].RawObjectID != nil {
				t.Fatalf("payload = %+v raw = %v", p, ev.got[0].RawObjectID)
			}
		})
	}
}

func TestNew_RequiresDeps(t *testing.T) {
	if _, err := callbacks.New(callbacks.Config{}); err == nil {
		t.Fatal("want error without events / clock")
	}
}
