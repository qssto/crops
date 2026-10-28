package model_test

import (
	"testing"

	"agri-steward/internal/model"
)

// legalTransitions 是母文档 §4.6.2-11 迁移表的独立抄写，与 model.WorkOrderTransitions 互相校对。
var legalTransitions = map[[2]model.WorkOrderStatus]bool{
	{model.OrderDraft, model.OrderPendingApproval}:    true,
	{model.OrderDraft, model.OrderCanceled}:           true,
	{model.OrderPendingApproval, model.OrderApproved}: true,
	{model.OrderPendingApproval, model.OrderRejected}: true,
	{model.OrderPendingApproval, model.OrderExpired}:  true,
	{model.OrderPendingApproval, model.OrderCanceled}: true,
	{model.OrderApproved, model.OrderDispatched}:      true,
	{model.OrderApproved, model.OrderPendingApproval}: true,
	{model.OrderApproved, model.OrderExpired}:         true,
	{model.OrderApproved, model.OrderCanceled}:        true,
	{model.OrderDispatched, model.OrderExecuting}:     true,
	{model.OrderDispatched, model.OrderDone}:          true,
	{model.OrderDispatched, model.OrderPartiallyDone}: true,
	{model.OrderDispatched, model.OrderFailed}:        true,
	{model.OrderDispatched, model.OrderCanceled}:      true,
	{model.OrderExecuting, model.OrderDone}:           true,
	{model.OrderExecuting, model.OrderPartiallyDone}:  true,
	{model.OrderExecuting, model.OrderFailed}:         true,
	{model.OrderExecuting, model.OrderCanceled}:       true,
}

// TestCanTransition_AllPairs 覆盖全部 from × to（含未知状态），不在表中的一律非法。
func TestCanTransition_AllPairs(t *testing.T) {
	states := append(model.WorkOrderStatuses(), "UNKNOWN", "")
	if got := len(model.WorkOrderStatuses()); got != 11 {
		t.Fatalf("status count = %d, want 11", got)
	}
	legalSeen := 0
	for _, from := range states {
		for _, to := range states {
			want := legalTransitions[[2]model.WorkOrderStatus{from, to}]
			if want {
				legalSeen++
			}
			if got := model.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	if legalSeen != len(legalTransitions) {
		t.Fatalf("legal pairs seen = %d, want %d", legalSeen, len(legalTransitions))
	}
}

func TestWorkOrderStatus_IsTerminal(t *testing.T) {
	terminal := map[model.WorkOrderStatus]bool{
		model.OrderDone: true, model.OrderPartiallyDone: true, model.OrderRejected: true,
		model.OrderExpired: true, model.OrderFailed: true, model.OrderCanceled: true,
	}
	for _, s := range model.WorkOrderStatuses() {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}
	if model.WorkOrderStatus("UNKNOWN").IsTerminal() {
		t.Error("unknown status must not be terminal")
	}
}

func TestEventType_Valid(t *testing.T) {
	tests := []struct {
		in   model.EventType
		want bool
	}{
		{model.EventOrderTransition, true},
		{model.EventRunFailed, true},
		{"order_transition", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := tt.in.Valid(); got != tt.want {
			t.Errorf("%q.Valid() = %v, want %v", tt.in, got, tt.want)
		}
	}
}
