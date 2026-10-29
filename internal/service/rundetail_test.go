package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

type fakeDetailLoader struct {
	rows repository.RunDetailRows
	err  error
}

func (f fakeDetailLoader) Load(context.Context, model.Scope, uint64) (repository.RunDetailRows, error) {
	return f.rows, f.err
}

var detailT0 = time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)

func u64(v uint64) *uint64 { return &v }

func ev(seq int, step string, typ model.EventType, payload string) repository.EventRow {
	return repository.EventRow{Seq: seq, StepID: step, Type: typ, Actor: "agent", Payload: json.RawMessage(payload),
		CreatedAt: detailT0.Add(time.Duration(seq) * time.Second)}
}

// fullChainRows 是走完「提问 → 工具 → 知识 → 建议 → 审批 → 下发」的一个 run。
func fullChainRows() repository.RunDetailRows {
	toolEnd := ev(3, "enrich", model.EventToolCallEnd, `{"tool":"weather.forecast","status":"OK"}`)
	toolEnd.ToolCallID = u64(11)
	orphanTool := ev(4, "enrich", model.EventToolCallEnd, `{"tool":"pest.risk","status":"OK"}`)
	orphanTool.ToolCallID = u64(999) // 无对应 tool_call 行：只进 step 的 event_seqs
	return repository.RunDetailRows{
		Run:  model.RunSummary{RunID: 7, Status: model.RunCompleted},
		Card: &model.AdviceCard{AdviceID: 5, Citations: []model.Citation{{Type: "kb", KBID: "rice-pest"}}},
		Steps: []repository.StepRow{
			{StepID: "intake", Node: "intake", Status: "COMPLETED"},
			{StepID: "enrich", Node: "enrich", Status: "COMPLETED"},
		},
		Events: []repository.EventRow{
			ev(1, "", model.EventRunStarted, `{"question":"稻飞虱要不要打药","field_id":3}`),
			ev(2, "enrich", model.EventStepStarted, `{"node":"enrich"}`),
			toolEnd, orphanTool,
			ev(5, "load_kb", model.EventKBLoaded, `{"entries":["rice-pest@v1"]}`),
			ev(6, "advise", model.EventLLMCallEnd, `{"task":"advise","status":"OK","prompt_tokens":100,"completion_tokens":20,"latency_ms":800}`),
			ev(7, "advise", model.EventLLMCallEnd, `{"task":"advise","status":"OK","prompt_tokens":50,"completion_tokens":5,"latency_ms":300}`),
			ev(8, "", model.EventDispatched, `{"order_id":21}`),
		},
		ToolCalls: []repository.ToolCallRow{
			{ID: 11, StepID: "enrich", ToolName: "weather.forecast", Status: "OK"},
			{ID: 12, StepID: "enrich", ToolName: "soil.moisture", Status: "TIMEOUT"},
		},
		Tickets:   []model.ApprovalTicket{{TicketID: 31, OrderID: 21}, {TicketID: 32, OrderID: 21}},
		Decisions: []repository.DecisionDetailRow{{ID: 1, TicketID: 31, Decision: "APPROVE"}, {ID: 2, TicketID: 31, Decision: "CONFIRM"}},
		Orders:    []repository.OrderRow{{ID: 21, Status: "DISPATCHED", DispatchMode: "DRY_RUN"}},
		Transitions: []model.OrderTransition{
			{Ref: model.OrderRef{OrderID: 21}, From: model.OrderDraft, To: model.OrderPendingApproval},
			{Ref: model.OrderRef{OrderID: 21}, From: model.OrderApproved, To: model.OrderDispatched},
		},
		Audits: []repository.AuditRow{
			{ID: 1, Action: "ASK"},
			{ID: 2, Action: "LLM_CALL", RawObjectID: u64(91)},
			{ID: 3, Action: "LLM_CALL", RawObjectID: u64(92)},
		},
	}
}

func stepIDs(d RunDetail) []string {
	out := make([]string, 0, len(d.Steps))
	for _, s := range d.Steps {
		out = append(out, s.StepID)
	}
	return out
}

func TestRunDetailService_Get(t *testing.T) {
	scope := model.Scope{TenantID: 1, FarmID: 1, UserID: 1001}
	tests := []struct {
		name    string
		runID   uint64
		loader  fakeDetailLoader
		wantErr error
		check   func(t *testing.T, d RunDetail)
	}{
		{name: "run 0 rejected", runID: 0, wantErr: ErrBadInput},
		{name: "run not found", runID: 7, loader: fakeDetailLoader{err: model.ErrNotFound}, wantErr: model.ErrNotFound},
		{name: "only agent_run: every section empty, no error", runID: 7,
			loader: fakeDetailLoader{rows: repository.RunDetailRows{Run: model.RunSummary{RunID: 7, Status: model.RunRunning}}},
			check: func(t *testing.T, d RunDetail) {
				if d.Question != nil || d.Card != nil || len(d.Warnings) != 0 {
					t.Fatalf("question=%v card=%v warnings=%v", d.Question, d.Card, d.Warnings)
				}
				b, err := json.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]json.RawMessage
				if err := json.Unmarshal(b, &m); err != nil {
					t.Fatal(err)
				}
				// 空表编码为 []，前端无需判 null
				for _, k := range []string{"steps", "approvals", "timeline", "audit"} {
					if string(m[k]) != "[]" {
						t.Errorf("%s = %s, want []", k, m[k])
					}
				}
				if string(m["evidence"]) != `{"kb_loaded":[],"citations":[]}` {
					t.Errorf("evidence = %s", m["evidence"])
				}
			}},
		{name: "full chain grouped by step", runID: 7, loader: fakeDetailLoader{rows: fullChainRows()},
			check: func(t *testing.T, d RunDetail) {
				// run_step 顺序在前，事件里新出现的 step 按首次出现追加；step_id 为空的事件归 run 级（""）
				want := []string{"intake", "enrich", "", "load_kb", "advise"}
				if got := stepIDs(d); len(got) != len(want) {
					t.Fatalf("steps = %q, want %q", got, want)
				} else {
					for i := range want {
						if got[i] != want[i] {
							t.Fatalf("steps = %q, want %q", got, want)
						}
					}
				}
				if d.Question == nil || d.Question.Question != "稻飞虱要不要打药" || d.Question.FieldID != 3 {
					t.Fatalf("question = %+v", d.Question)
				}
				enrich := d.Steps[1]
				if enrich.Status != "COMPLETED" || len(enrich.ToolCalls) != 2 {
					t.Fatalf("enrich = %+v", enrich)
				}
				if c := enrich.ToolCalls[0]; c.EventSeq == nil || *c.EventSeq != 3 || len(c.Event) == 0 {
					t.Errorf("tool 11 not linked to TOOL_CALL_END: %+v", c)
				}
				if c := enrich.ToolCalls[1]; c.EventSeq != nil {
					t.Errorf("tool 12 has no end event, got seq %v", *c.EventSeq)
				}
				if got := enrich.EventSeqs; len(got) != 2 || got[0] != 2 || got[1] != 4 {
					t.Errorf("enrich event_seqs = %v, want [2 4]", got)
				}
				advise := d.Steps[4]
				if advise.Node != "advise" || advise.Status != "" || len(advise.LLMCalls) != 2 {
					t.Fatalf("advise = %+v", advise)
				}
				if l := advise.LLMCalls[0]; l.PromptTokens != 100 || l.RawObjectID == nil || *l.RawObjectID != 91 {
					t.Errorf("llm[0] = %+v", l)
				}
				if l := advise.LLMCalls[1]; l.RawObjectID == nil || *l.RawObjectID != 92 {
					t.Errorf("llm[1] raw = %v, want 92", l.RawObjectID)
				}
				if len(d.Evidence.KBLoaded) != 1 || d.Evidence.KBLoaded[0].Entries[0] != "rice-pest@v1" ||
					len(d.Evidence.Citations) != 1 {
					t.Errorf("evidence = %+v", d.Evidence)
				}
				if len(d.Approvals) != 2 || len(d.Approvals[0].Decisions) != 2 || len(d.Approvals[1].Decisions) != 0 {
					t.Errorf("approvals = %+v", d.Approvals)
				}
				if len(d.Execution.Orders) != 1 || len(d.Execution.Orders[0].Transitions) != 2 ||
					d.Execution.Orders[0].Transitions[1].To != model.OrderDispatched || len(d.Execution.Dispatches) != 1 {
					t.Errorf("execution = %+v", d.Execution)
				}
				if len(d.Timeline) != 8 || len(d.Audit) != 3 {
					t.Errorf("timeline %d audit %d", len(d.Timeline), len(d.Audit))
				}
			}},
		{name: "llm audit count mismatch: raw ids not linked", runID: 7,
			loader: fakeDetailLoader{rows: func() repository.RunDetailRows {
				r := fullChainRows()
				r.Audits = r.Audits[:2] // 只投影了 1 条 LLM_CALL
				return r
			}()},
			check: func(t *testing.T, d RunDetail) {
				for _, l := range d.Steps[4].LLMCalls {
					if l.RawObjectID != nil {
						t.Errorf("raw linked despite mismatch: %+v", l)
					}
				}
			}},
		{name: "bad payload becomes warning", runID: 7,
			loader: fakeDetailLoader{rows: repository.RunDetailRows{Run: model.RunSummary{RunID: 7},
				Events: []repository.EventRow{ev(1, "", model.EventRunStarted, `not-json`),
					ev(2, "advise", model.EventLLMCallEnd, `[1]`)}}},
			check: func(t *testing.T, d RunDetail) {
				if d.Question != nil || len(d.Warnings) != 2 || len(d.Timeline) != 2 {
					t.Fatalf("question=%v warnings=%v", d.Question, d.Warnings)
				}
				if s := d.Steps[1]; len(s.LLMCalls) != 0 || len(s.EventSeqs) != 1 {
					t.Errorf("advise = %+v", s)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewRunDetailService(tt.loader).Get(context.Background(), scope, tt.runID)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, d)
		})
	}
}
