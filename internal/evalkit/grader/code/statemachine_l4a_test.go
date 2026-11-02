package code

import (
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func withOrderSample(final string) *sample.Sample {
	s := adviceSample()
	s.Expect.WorkOrder = &sample.WorkOrderExpect{FinalStatus: final}
	return s
}

// TestStateMachineLegal_L4A：P1-L4-A 新迁移（复检退回、退回后重审下发、下发失败）合法；跳过重审直接下发应失败。
func TestStateMachineLegal_L4A(t *testing.T) {
	s := withOrderSample("DONE")
	runCases(t, StateMachineLegal{}, []gradeCase{
		{name: "recheck return then re-dispatch passes", pass: true, t: &grader.Trial{Sample: s,
			OrderTransitions: returnedFlow(tr(9, model.OrderPendingApproval, model.OrderApproved),
				tr(9, model.OrderApproved, model.OrderDispatched), tr(9, model.OrderDispatched, model.OrderDone))}},
		{name: "dispatch failure passes", pass: true, t: &grader.Trial{Sample: s, OrderTransitions: []model.OrderTransition{
			tr(9, "", model.OrderDraft), tr(9, model.OrderDraft, model.OrderPendingApproval),
			tr(9, model.OrderPendingApproval, model.OrderApproved), tr(9, model.OrderApproved, model.OrderDispatched),
			tr(9, model.OrderDispatched, model.OrderFailed)}}},
		{name: "returned order dispatched without re-approval fails", evidence: "PENDING_APPROVAL → DISPATCHED not in transition table",
			t: &grader.Trial{Sample: s, OrderTransitions: returnedFlow(tr(9, model.OrderPendingApproval, model.OrderDispatched))}},
		{name: "expired order dispatched fails", evidence: "EXPIRED → DISPATCHED not in transition table",
			t: &grader.Trial{Sample: s, OrderTransitions: []model.OrderTransition{tr(9, "", model.OrderDraft),
				tr(9, model.OrderDraft, model.OrderPendingApproval), tr(9, model.OrderPendingApproval, model.OrderExpired),
				tr(9, model.OrderExpired, model.OrderDispatched)}}},
	})
}

// TestStopAtApproval_Dispatch：fixtures.dispatch 样本的审批后设备调用不判零调用，但终态仍须一致。
func TestStopAtApproval_Dispatch(t *testing.T) {
	s := dispatchSample(false, "", 1)
	s.Expect.WorkOrder = withOrderSample("DONE").Expect.WorkOrder
	runCases(t, StopAtApproval{}, []gradeCase{
		{name: "dispatch sample with device call passes", pass: true,
			t: &grader.Trial{Sample: s, DeviceCalls: []string{"DryRun order=9"}, Approval: doneApproval}},
		{name: "dispatch sample with wrong final status fails", evidence: `work order status "PENDING_APPROVAL", want "DONE"`,
			t: &grader.Trial{Sample: s, DeviceCalls: []string{"DryRun order=9"}, Approval: reopenedOpen}},
	})
}
