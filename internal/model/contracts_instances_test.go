package model_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"agri-steward/internal/model"
)

func ptr[T any](v T) *T { return &v }

var (
	t0      = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	digest  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	kbCite  = model.Citation{TenantID: 1, FarmID: 1, Type: model.CitationKB, KBID: "rice/pest/planthopper", KBVersion: "v1"}
	measure = model.Citation{TenantID: 1, FarmID: 1, Type: model.CitationMeasured, ClaimPath: "reasons[0]",
		DataSource: "mcp-weather", CollectedAt: &t0, Value: json.RawMessage(`2.1`), ToolCallID: ptr(uint64(7))}
)

// validInstances 是每个契约的合法样例，必须通过 schema 校验。
func validInstances() map[string]any {
	return map[string]any{
		"advice_card.schema.json": model.AdviceCard{
			AdviceID: 1, RunID: 2, TenantID: 1, FarmID: 1, FieldID: 3, Crop: "rice", PhenologyStage: "分蘖末期",
			Category: model.CategoryProtect, AdviceType: model.AdviceTypeOperation, Outcome: model.OutcomeAdvice,
			Conclusion: "建议近期施药", Reasons: []string{"虫口密度达标"}, ProductCandidates: []string{"吡蚜酮"},
			SelectedProduct: "吡蚜酮", Numbers: []model.Number{{Field: "dose", Value: 20, Unit: "g/亩"}},
			Citations: []model.Citation{kbCite, measure}, Confidence: 0.8,
			SuggestedWindow: &model.TimeWindow{From: t0, To: t0.Add(48 * time.Hour)}, NextAction: model.NextCreateOrder,
		},
		"work_order.schema.json": model.WorkOrder{
			OrderID: 1, AdviceID: 1, RunID: 2, TenantID: 1, FarmID: 1, FieldID: 3, WorkType: model.WorkSpray,
			DeviceType: "DRONE", Payload: json.RawMessage(`{"product":"吡蚜酮"}`),
			TimeWindow: model.TimeWindow{From: t0, To: t0.Add(time.Hour)},
			ConstraintChecks: []model.ConstraintCheck{{Phase: model.PhasePreApproval, Attempt: 1, RuleID: "wind",
				Passed: true, Observed: json.RawMessage(`{"value":2.1}`), Threshold: json.RawMessage(`{"value":3}`)}},
			Status: model.OrderApproved, DispatchMode: model.DispatchDryRun, Approver: ptr(uint64(1002)),
			ApprovalExpiresAt: &t0,
		},
		"citation.schema.json": measure,
		"run_state.schema.json": model.RunState{
			RunID: 2, TenantID: 1, FarmID: 1, CreatedAt: t0,
			Steps:            []model.RunStep{{StepID: "classify", Node: "classify", Status: model.StepCompleted, StartedAt: t0, EndedAt: &t0}},
			CompletedStepIDs: []string{"classify"},
			PendingApproval:  &model.PendingApproval{TicketID: 5, OrderID: 1},
			ContextSnapshot:  json.RawMessage(`{"field_id":3}`),
		},
		"approval_ticket.schema.json": model.ApprovalTicket{
			TicketID: 5, RunID: 2, TenantID: 1, FarmID: 1, OrderID: 1, TicketKind: model.TicketKindOrder, TicketSeq: 1,
			RiskLevel: model.RiskHigh, RequiredRole: []model.Role{model.RoleAgronomist}, ConfirmRequired: true,
			InitiatorID: 1001, Snapshot: model.TicketSnapshot{Digest: digest, RawObjectID: 9},
			Status: model.TicketOpen, ExpiresAt: &t0,
		},
	}
}

// toMap 把 Go 实例序列化后解回 map，便于构造应失败的变体。
func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestContractSchemas_Instances(t *testing.T) {
	set := loadSchemas(t)
	valid := validInstances()
	tests := []struct {
		name    string
		file    string
		mutate  func(m map[string]any)
		wantErr bool
	}{
		{name: "advice ok", file: "advice_card.schema.json"},
		{name: "order ok", file: "work_order.schema.json"},
		{name: "citation ok", file: "citation.schema.json"},
		{name: "run state ok", file: "run_state.schema.json"},
		{name: "ticket ok", file: "approval_ticket.schema.json"},
		{name: "refused advice without citations ok", file: "advice_card.schema.json", mutate: func(m map[string]any) {
			m["outcome"], m["refuse_reason"], m["citations"] = "REFUSED", "KB_MISS", []any{}
			delete(m, "advice_type")
		}},
		{name: "advice missing tenant_id", file: "advice_card.schema.json", wantErr: true,
			mutate: func(m map[string]any) { delete(m, "tenant_id") }},
		{name: "advice ADVICE without citation", file: "advice_card.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["citations"] = []any{} }},
		{name: "advice REFUSED without reason", file: "advice_card.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["outcome"] = "REFUSED" }},
		{name: "advice unknown property", file: "advice_card.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["action"] = "spray" }},
		{name: "advice confidence > 1", file: "advice_card.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["confidence"] = 1.5 }},
		{name: "advice nested kb citation without version", file: "advice_card.schema.json", wantErr: true,
			mutate: func(m map[string]any) { delete(m["citations"].([]any)[0].(map[string]any), "kb_version") }},
		{name: "measured citation without tool_call_id", file: "citation.schema.json", wantErr: true,
			mutate: func(m map[string]any) { delete(m, "tool_call_id") }},
		{name: "citation bad collected_at", file: "citation.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["collected_at"] = "yesterday" }},
		{name: "order unknown status", file: "work_order.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["status"] = "PAUSED" }},
		{name: "order missing farm_id", file: "work_order.schema.json", wantErr: true,
			mutate: func(m map[string]any) { delete(m, "farm_id") }},
		{name: "order payload not object", file: "work_order.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["payload"] = "x" }},
		{name: "run state duplicate completed step", file: "run_state.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["completed_step_ids"] = []any{"a", "a"} }},
		{name: "run state bad step status", file: "run_state.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["steps"].([]any)[0].(map[string]any)["status"] = "DONE" }},
		{name: "ticket OPEN without expires_at", file: "approval_ticket.schema.json", wantErr: true,
			mutate: func(m map[string]any) { delete(m, "expires_at") }},
		{name: "ticket APPROVED without operator", file: "approval_ticket.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["status"] = "APPROVED"; m["decided_at"] = t0.Format(time.RFC3339) }},
		{name: "ticket bad digest", file: "approval_ticket.schema.json", wantErr: true,
			mutate: func(m map[string]any) { m["snapshot"].(map[string]any)["digest"] = "abc" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := toMap(t, valid[tt.file])
			if tt.mutate != nil {
				tt.mutate(m)
			}
			err := set.compile(t, tt.file).Validate(m)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestContractSchemas_Enums 比对 schema 枚举与 Go 常量。
func TestContractSchemas_Enums(t *testing.T) {
	set := loadSchemas(t)
	tests := []struct {
		file, prop string
		want       []string
	}{
		{"work_order.schema.json", "status", strs(model.WorkOrderStatuses())},
		{"work_order.schema.json", "dispatch_mode", strs([]model.DispatchMode{model.DispatchDryRun, model.DispatchReal})},
		{"work_order.schema.json", "work_type", strs([]model.WorkType{model.WorkSpray, model.WorkFertilize, model.WorkIrrigate})},
		{"advice_card.schema.json", "outcome", strs([]model.Outcome{model.OutcomeAdvice, model.OutcomeRefused})},
		{"advice_card.schema.json", "category", strs([]model.Category{model.CategoryProtect, model.CategoryFertilize,
			model.CategoryIrrigate, model.CategoryDisaster})},
		{"advice_card.schema.json", "advice_type", strs([]model.AdviceType{model.AdviceTypeOperation, model.AdviceTypeObservation})},
		{"advice_card.schema.json", "next_action", strs([]model.NextAction{model.NextCreateOrder, model.NextObserve, model.NextNone})},
		{"citation.schema.json", "type", strs([]model.CitationType{model.CitationKB, model.CitationMeasured})},
		{"approval_ticket.schema.json", "status", strs([]model.TicketStatus{model.TicketPreparing, model.TicketOpen,
			model.TicketApproved, model.TicketRejected, model.TicketExpired, model.TicketSuperseded})},
		{"approval_ticket.schema.json", "risk_level", strs([]model.RiskLevel{model.RiskLow, model.RiskHigh})},
		{"approval_ticket.schema.json", "ticket_kind", strs([]model.TicketKind{model.TicketKindOrder, model.TicketKindPlan})},
	}
	for _, tt := range tests {
		t.Run(tt.file+"#"+tt.prop, func(t *testing.T) {
			prop := set.byFile[tt.file]["properties"].(map[string]any)[tt.prop].(map[string]any)
			var got []string
			for _, v := range prop["enum"].([]any) {
				got = append(got, v.(string))
			}
			sort.Strings(got)
			sort.Strings(tt.want)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("schema enum %v != Go constants %v", got, tt.want)
			}
		})
	}
}

func strs[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}
