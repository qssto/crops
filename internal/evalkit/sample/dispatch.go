package sample

import (
	"fmt"

	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/model"
)

// RuleDispatch 校验 P1-L4-A 下发样本：fixtures.dispatch 与 expect.dispatch 的结构。
const RuleDispatch = "dispatch_shape"

// maxRedeliver 是 fixtures.dispatch.redeliver 上限（重复投递演练几次即可，防止样本写错成大数拖慢评测）。
const maxRedeliver = 3

// checkDispatch：fixtures.dispatch 须配 approver（下发只发生在审批通过之后）且不与 stop_at_approval 同用；
// reapprove 取值同 approver；redeliver 在 [0, maxRedeliver]。expect.dispatch 须配 fixtures.dispatch 与 outcome=ADVICE，
// device_dispatches ≥ 0；断言退回时 work_order.final_status 须为 PENDING_APPROVAL（未声明 reapprove）。
func checkDispatch(s *Sample) []string {
	var msgs []string
	f, e := s.Fixtures.Dispatch, s.Expect.Dispatch
	if f != nil {
		if s.Fixtures.Approver == "" {
			msgs = append(msgs, "fixtures.dispatch requires fixtures.approver")
		}
		if s.Expect.Trajectory.StopAtApproval {
			msgs = append(msgs, "fixtures.dispatch conflicts with stop_at_approval")
		}
		if !validApprover(f.Reapprove) {
			msgs = append(msgs, fmt.Sprintf("fixtures.dispatch.reapprove %q is not allowed", f.Reapprove))
		}
		if f.Redeliver < 0 || f.Redeliver > maxRedeliver {
			msgs = append(msgs, fmt.Sprintf("fixtures.dispatch.redeliver %d must be 0..%d", f.Redeliver, maxRedeliver))
		}
	}
	if e == nil {
		return msgs
	}
	if f == nil {
		msgs = append(msgs, "expect.dispatch requires fixtures.dispatch")
	}
	if s.Expect.Outcome != model.OutcomeAdvice {
		msgs = append(msgs, "expect.dispatch requires outcome=ADVICE")
	}
	if e.DeviceDispatches != nil && *e.DeviceDispatches < 0 {
		msgs = append(msgs, fmt.Sprintf("expect.dispatch.device_dispatches %d must be >= 0", *e.DeviceDispatches))
	}
	returnedFinal := f != nil && f.Reapprove == ""
	if e.RecheckReturned && returnedFinal &&
		(s.Expect.WorkOrder == nil || s.Expect.WorkOrder.FinalStatus != string(model.OrderPendingApproval)) {
		msgs = append(msgs, "expect.dispatch.recheck_returned without reapprove requires work_order.final_status=PENDING_APPROVAL")
	}
	if len(e.RecheckFailedRules) > 0 && !e.RecheckReturned {
		msgs = append(msgs, "expect.dispatch.recheck_failed_rules requires recheck_returned=true")
	}
	for i, id := range e.RecheckFailedRules {
		if id == "" || len(id) > constraint.RuleIDMax {
			msgs = append(msgs, fmt.Sprintf("expect.dispatch.recheck_failed_rules[%d] %q must be 1..%d bytes", i, id, constraint.RuleIDMax))
		}
	}
	return msgs
}
