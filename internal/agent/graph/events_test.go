package graph_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agri-steward/internal/model"
)

// L7：Ask 发 RUN_STARTED（actor=user）/ KB_LOADED（kb_id@version）/ ADVICE_READY（advice_id）/ RUN_COMPLETED（status）。
func TestAgentAsk_EmitsL7Events(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	store, ev := newMemStore(), &recordingEvents{}
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{tools: fixtureTools(t, s, clk), store: store, events: ev})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[model.EventType][]model.RunEvent{}
	for _, e := range ev.events {
		if e.RunID != ans.RunID || e.FarmID != testScope.FarmID || e.CreatedAt.IsZero() {
			t.Fatalf("event ids = %+v", e)
		}
		byType[e.Type] = append(byType[e.Type], e)
	}
	for _, typ := range []model.EventType{model.EventRunStarted, model.EventKBLoaded, model.EventAdviceReady,
		model.EventRunCompleted} {
		if len(byType[typ]) != 1 {
			t.Fatalf("%s events = %d, want 1", typ, len(byType[typ]))
		}
	}
	decode := func(e model.RunEvent, v any) {
		t.Helper()
		if err := json.Unmarshal(e.Payload, v); err != nil {
			t.Fatalf("%s payload %s: %v", e.Type, e.Payload, err)
		}
	}
	started := byType[model.EventRunStarted][0]
	var sp model.RunStartedPayload
	decode(started, &sp)
	if started.Actor != model.ActorUser || started.ActorID != testScope.UserID || sp.Question == "" || sp.FieldID != q.FieldID {
		t.Fatalf("RUN_STARTED = %+v payload %+v", started, sp)
	}
	kb := byType[model.EventKBLoaded][0]
	var kp model.KBLoadedPayload
	decode(kb, &kp)
	if kb.StepID != "load_knowledge" || len(kp.Entries) == 0 || !strings.Contains(kp.Entries[0], "@") {
		t.Fatalf("KB_LOADED = %+v payload %+v", kb, kp)
	}
	ready := byType[model.EventAdviceReady][0]
	var ap model.AdviceReadyPayload
	decode(ready, &ap)
	if ready.StepID != "persist" || ap.AdviceID == 0 || ap.AdviceID != ans.Card.AdviceID || ap.Outcome != string(ans.Outcome) {
		t.Fatalf("ADVICE_READY = %+v payload %+v", ready, ap)
	}
	var cp model.RunEndPayload
	decode(byType[model.EventRunCompleted][0], &cp)
	if cp.Status != string(model.RunCompleted) {
		t.Fatalf("RUN_COMPLETED payload %+v", cp)
	}
}
