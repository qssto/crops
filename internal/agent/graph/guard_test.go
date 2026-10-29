package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// recordingEvents 记录 Emit 的事件；err 非空时返回该错误。
type recordingEvents struct {
	mu     sync.Mutex
	events []model.RunEvent
	err    error
}

func (r *recordingEvents) Emit(_ context.Context, e model.RunEvent) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	r.events = append(r.events, e)
	return int64(len(r.events)), nil
}

// bareNumberModel 让 explain 裸写数字（应被数值校验拦截）。
var bareNumberModel = scriptedModel{override: map[llm.Task]string{
	llm.TaskExplain: `{"conclusion":"每亩用吡蚜酮10克","reasons":[]}`}}

// 验收 4：校验失败 → REFUSED / VALIDATION_FAILED，并经 Events 发 VALIDATION_FAILED（payload 带 run_id 与校验项）。
func TestAgentAsk_ValidationFailedEmitsEvent(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	tests := []struct {
		name      string
		model     scriptedModel
		wantCheck string
	}{
		{name: "bare number", model: bareNumberModel, wantCheck: "number"},
		{name: "product outside candidates",
			model:     scriptedModel{override: map[llm.Task]string{llm.TaskChoose: `{"product_id":"bifenthrin-25ec"}`}},
			wantCheck: "product"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, ev := newMemStore(), &recordingEvents{}
			clk := sampleClock(t, s)
			a := h.newAgent(t, clk, deps{model: tt.model, tools: fixtureTools(t, s, clk), store: store, events: ev})
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if ans.Outcome != model.OutcomeRefused || ans.Card.RefuseReason != "VALIDATION_FAILED" || store.orderCount() != 0 {
				t.Fatalf("outcome %s/%q orders %d", ans.Outcome, ans.Card.RefuseReason, store.orderCount())
			}
			var got []model.RunEvent
			for _, e := range ev.events {
				if e.Type == model.EventValidationFailed {
					got = append(got, e)
				}
			}
			if len(got) != 1 {
				t.Fatalf("VALIDATION_FAILED events = %d, want 1 (no retry)", len(got))
			}
			var p struct {
				RunID  uint64 `json:"run_id"`
				Check  string `json:"check"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(got[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.RunID != ans.RunID || got[0].RunID != ans.RunID || p.Check != tt.wantCheck || p.Detail == "" ||
				got[0].StepID != "validate" || got[0].Actor != model.ActorAgent {
				t.Fatalf("event %+v payload %s", got[0], got[0].Payload)
			}
		})
	}
}

// 事件写失败不吞：Ask 返回错误。
func TestAgentAsk_ValidationEventErrorNotSwallowed(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	boom := errors.New("event store down")
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: bareNumberModel, tools: fixtureTools(t, s, clk), store: newMemStore(),
		events: &recordingEvents{err: boom}})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Ask(context.Background(), q); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
