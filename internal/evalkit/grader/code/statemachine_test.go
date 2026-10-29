package code

import (
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func tr(order uint64, from, to model.WorkOrderStatus) model.OrderTransition {
	return model.OrderTransition{Ref: model.OrderRef{TenantID: 1, FarmID: 1, OrderID: order}, From: from, To: to}
}

func TestStateMachineLegal(t *testing.T) {
	approved := []model.OrderTransition{tr(9, "", model.OrderDraft), tr(9, model.OrderDraft, model.OrderPendingApproval),
		tr(9, model.OrderPendingApproval, model.OrderApproved)}
	withOrder := &sample.Sample{ID: "o", Expect: sample.Expect{WorkOrder: &sample.WorkOrderExpect{FinalStatus: "EXPIRED"}}}
	withApprover := &sample.Sample{ID: "a", Fixtures: sample.Fixtures{Approver: "timeout"}}
	plain := &sample.Sample{ID: "p"}
	runCases(t, StateMachineLegal{}, []gradeCase{
		{name: "approve then recheck cancel passes", pass: true, t: &grader.Trial{Sample: withOrder,
			OrderTransitions: append(approved, tr(9, model.OrderApproved, model.OrderCanceled))}},
		{name: "expired passes", pass: true, t: &grader.Trial{Sample: withApprover, OrderTransitions: []model.OrderTransition{
			tr(3, "", model.OrderDraft), tr(3, model.OrderDraft, model.OrderPendingApproval),
			tr(3, model.OrderPendingApproval, model.OrderExpired)}}},
		{name: "two orders interleaved pass", pass: true, t: &grader.Trial{Sample: withOrder, OrderTransitions: []model.OrderTransition{
			tr(1, "", model.OrderDraft), tr(2, "", model.OrderDraft), tr(1, model.OrderDraft, model.OrderCanceled),
			tr(2, model.OrderDraft, model.OrderPendingApproval)}}},
		{name: "skip approval fails", evidence: "DRAFT → DISPATCHED not in transition table",
			t: &grader.Trial{Sample: withOrder, OrderTransitions: []model.OrderTransition{
				tr(9, "", model.OrderDraft), tr(9, model.OrderDraft, model.OrderDispatched)}}},
		{name: "leaving terminal fails", evidence: "EXPIRED → APPROVED not in transition table",
			t: &grader.Trial{Sample: withOrder, OrderTransitions: []model.OrderTransition{tr(9, "", model.OrderDraft),
				tr(9, model.OrderDraft, model.OrderPendingApproval), tr(9, model.OrderPendingApproval, model.OrderExpired),
				tr(9, model.OrderExpired, model.OrderApproved)}}},
		{name: "broken chain fails", evidence: "APPROVED → DISPATCHED does not follow PENDING_APPROVAL",
			t: &grader.Trial{Sample: withOrder, OrderTransitions: []model.OrderTransition{tr(9, "", model.OrderDraft),
				tr(9, model.OrderDraft, model.OrderPendingApproval), tr(9, model.OrderApproved, model.OrderDispatched)}}},
		{name: "missing creation fails", evidence: `first transition "PENDING_APPROVAL" → APPROVED`,
			t: &grader.Trial{Sample: withOrder, OrderTransitions: []model.OrderTransition{
				tr(9, model.OrderPendingApproval, model.OrderApproved)}}},
		{name: "no order sample unrecorded passes", pass: true, t: &grader.Trial{Sample: plain}},
		{name: "recorded empty passes", pass: true, t: &grader.Trial{Sample: withOrder, OrderTransitions: []model.OrderTransition{}}},
		{name: "order sample unrecorded is grader error", wantErr: true, t: &grader.Trial{Sample: withOrder}},
		{name: "approver sample unrecorded is grader error", wantErr: true, t: &grader.Trial{Sample: withApprover}},
		{name: "nil sample is grader error", wantErr: true, t: &grader.Trial{}},
	})
	if g := (StateMachineLegal{}); g.Name() != "state_machine_legal" || !g.RedLine() {
		t.Fatal("name / redline")
	}
}
