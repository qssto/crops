package repository_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func TestProjectAudit(t *testing.T) {
	at := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	raw := uint64(55)
	base := model.RunEvent{TenantID: 1, FarmID: 1, RunID: 9, Actor: model.ActorAgent, CreatedAt: at}
	tests := []struct {
		name       string
		typ        model.EventType
		actor      model.ActorType
		actorID    uint64
		payload    string
		raw        *uint64
		wantOK     bool
		wantAction model.AuditAction
		wantTarget string
		wantResult string
		wantErr    error
	}{
		{name: "ask", typ: model.EventRunStarted, actor: model.ActorUser, actorID: 7,
			payload: `{"field_id":3,"question":"要打药吗"}`, wantOK: true,
			wantAction: model.AuditAsk, wantTarget: "field:3", wantResult: model.AuditResultOK},
		{name: "tool ok", typ: model.EventToolCallEnd, payload: `{"tool":"weather.forecast","status":"OK"}`, wantOK: true,
			wantAction: model.AuditToolCall, wantTarget: "tool:weather.forecast", wantResult: model.AuditResultOK},
		{name: "tool timeout", typ: model.EventToolCallEnd, payload: `{"tool":"soil.moisture","status":"TIMEOUT"}`,
			wantOK: true, wantAction: model.AuditToolCall, wantTarget: "tool:soil.moisture", wantResult: model.AuditResultFailed},
		{name: "tool rejected", typ: model.EventToolCallEnd, payload: `{"tool":"x.y","status":"REJECTED"}`, wantOK: true,
			wantAction: model.AuditToolCall, wantTarget: "tool:x.y", wantResult: model.AuditResultDenied},
		{name: "kb", typ: model.EventKBLoaded, payload: `{"kb_ref":"kb-v1","entries":["a@1"]}`, wantOK: true,
			wantAction: model.AuditKBLoad, wantTarget: "kb:kb-v1", wantResult: model.AuditResultOK},
		{name: "llm ok with raw", typ: model.EventLLMCallEnd, raw: &raw,
			payload: `{"task":"classify","status":"OK","prompt_tokens":1,"completion_tokens":2,"latency_ms":3}`, wantOK: true,
			wantAction: model.AuditLLMCall, wantTarget: "llm:classify", wantResult: model.AuditResultOK},
		{name: "llm error", typ: model.EventLLMCallEnd, payload: `{"task":"explain","status":"ERROR"}`, wantOK: true,
			wantAction: model.AuditLLMCall, wantTarget: "llm:explain", wantResult: model.AuditResultFailed},
		{name: "validation failed", typ: model.EventValidationFailed, payload: `{"check":"numbers","detail":"x"}`,
			wantOK: true, wantAction: model.AuditValidate, wantTarget: "check:numbers", wantResult: model.AuditResultFailed},
		{name: "advice ready", typ: model.EventAdviceReady, payload: `{"advice_id":11,"outcome":"OPERATION"}`, wantOK: true,
			wantAction: model.AuditValidate, wantTarget: "advice:11", wantResult: model.AuditResultOK},
		{name: "approval open", typ: model.EventApprovalOpen, payload: `{"ticket_id":7,"order_id":12}`, wantOK: true,
			wantAction: model.AuditApproval, wantTarget: "ticket:7", wantResult: model.AuditResultOK},
		{name: "approval pending", typ: model.EventApprovalPending, payload: `{"ticket_id":7}`, wantOK: true,
			wantAction: model.AuditApproval, wantTarget: "ticket:7", wantResult: model.AuditResultOK},
		{name: "approval decided", typ: model.EventApprovalDecided, actor: model.ActorUser, actorID: 5,
			payload: `{"ticket_id":7,"order_id":12}`, wantOK: true,
			wantAction: model.AuditApproval, wantTarget: "ticket:7", wantResult: model.AuditResultOK},
		{name: "order transition", typ: model.EventOrderTransition, payload: `{"order_id":12,"from":"DRAFT","to":"APPROVED"}`,
			wantOK: true, wantAction: model.AuditOrderTransition, wantTarget: "order:12", wantResult: model.AuditResultOK},
		{name: "dispatched", typ: model.EventDispatched, payload: `{"order_id":12}`, wantOK: true,
			wantAction: model.AuditDispatch, wantTarget: "order:12", wantResult: model.AuditResultOK},
		{name: "step not projected", typ: model.EventStepStarted, payload: `{"node":"classify"}`},
		{name: "run completed not projected", typ: model.EventRunCompleted, payload: `{"status":"COMPLETED"}`},
		{name: "non-audited bad payload ignored", typ: model.EventStepEnded, payload: `not json`},
		{name: "bad payload fails", typ: model.EventToolCallEnd, payload: `not json`, wantErr: repository.ErrBadAuditPayload},
		{name: "empty payload fails", typ: model.EventOrderTransition, payload: ``, wantErr: repository.ErrBadAuditPayload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := base
			e.Type, e.Payload, e.RawObjectID = tt.typ, json.RawMessage(tt.payload), tt.raw
			if tt.actor != "" {
				e.Actor, e.ActorID = tt.actor, tt.actorID
			}
			got, ok, err := repository.ProjectAudit(e)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("projected = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Action != string(tt.wantAction) || got.Target != tt.wantTarget || got.Result != tt.wantResult {
				t.Fatalf("got action=%s target=%s result=%s, want %s %s %s",
					got.Action, got.Target, got.Result, tt.wantAction, tt.wantTarget, tt.wantResult)
			}
			if got.Actor != (model.Actor{Type: e.Actor, ID: e.ActorID}) || got.RunID == nil || *got.RunID != e.RunID {
				t.Fatalf("actor / run_id = %+v %v", got.Actor, got.RunID)
			}
			if got.PayloadDigest != repository.Digest(e.Payload) || string(got.Summary) != tt.payload {
				t.Fatalf("digest / summary mismatch: %s %s", got.PayloadDigest, got.Summary)
			}
			if got.RawObjectID != tt.raw || !got.CreatedAt.Equal(at) {
				t.Fatalf("raw / created_at = %v %v", got.RawObjectID, got.CreatedAt)
			}
		})
	}
}

func TestProjectAudit_ClipsTargetAndSummary(t *testing.T) {
	big := `{"tool":"` + strings.Repeat("工", 80) + `","status":"OK","pad":"` + strings.Repeat("x", 9000) + `"}`
	got, ok, err := repository.ProjectAudit(model.RunEvent{TenantID: 1, FarmID: 1, RunID: 1,
		Type: model.EventToolCallEnd, Actor: model.ActorAgent, Payload: json.RawMessage(big)})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if n := len([]rune(got.Target)); n != 64 {
		t.Fatalf("target runes = %d, want 64", n)
	}
	if !json.Valid(got.Summary) || len(got.Summary) > 8<<10 || !strings.Contains(string(got.Summary), "truncated") {
		t.Fatalf("summary = %s", got.Summary)
	}
	if got.PayloadDigest != repository.Digest([]byte(big)) {
		t.Fatal("digest must cover the full payload")
	}
}
