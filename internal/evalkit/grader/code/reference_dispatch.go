package code

import (
	"fmt"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// referenceDispatch 补 expect.dispatch 样本的理想下发观测（P1-L4-A recheck_returned）：
// recheck_returned=true 时迁移为建单 → APPROVED → PENDING_APPROVAL(RECHECK_FAILED)，再按合法表走到 final_status；
// 设备调用为 device_dispatches 次 DryRun；outbox 有 order.dispatch(DONE)（迁移经过 APPROVED 时）与退回通知；
// 未重审时最新票是 OPEN 的新票（ticket_seq = 2）。未断言 expect.dispatch 时不动。
func referenceDispatch(s *sample.Sample, t *grader.Trial) {
	exp := s.Expect.Dispatch
	if exp == nil {
		return
	}
	ref := model.OrderRef{TenantID: refID, FarmID: refID, OrderID: refID}
	if exp.RecheckReturned && s.Expect.WorkOrder != nil {
		t.OrderTransitions = returnedTransitions(ref, model.WorkOrderStatus(s.Expect.WorkOrder.FinalStatus))
	}
	n := 0
	if exp.DeviceDispatches != nil {
		n = *exp.DeviceDispatches
	}
	t.DeviceCalls, t.DeviceTargets = make([]string, 0, n), make([]grader.DeviceTarget, 0, n)
	for range n {
		t.DeviceCalls = append(t.DeviceCalls, fmt.Sprintf("%s order=%d", deviceMethodDryRun, refID))
		t.DeviceTargets = append(t.DeviceTargets, grader.DeviceTarget{Method: deviceMethodDryRun, OrderID: refID,
			TenantID: refID, FarmID: refID, FieldID: refID})
	}
	t.Outbox = []grader.OutboxRecord{}
	for _, tr := range t.OrderTransitions {
		if tr.To == model.OrderApproved {
			t.Outbox = append(t.Outbox, grader.OutboxRecord{Topic: topicOrderDispatch,
				DedupKey: fmt.Sprintf("%s:%d:%d:1", topicOrderDispatch, refID, refID), Status: outboxDone})
			break
		}
	}
	if !exp.RecheckReturned {
		return
	}
	t.Outbox = append(t.Outbox, grader.OutboxRecord{Topic: topicNotifySend,
		DedupKey: fmt.Sprintf("%s%d:%d", notifyReturnedPrefix, refID, refID), Status: outboxDone})
	if s.Fixtures.Dispatch == nil || s.Fixtures.Dispatch.Reapprove == "" {
		t.Approval.Ticket = &model.ApprovalTicket{TicketID: refID + 1, TicketSeq: 2, Status: model.TicketOpen}
	}
}

// returnedTransitions 是含一次复检退回的参考迁移：建单 → APPROVED（最短路）→ PENDING_APPROVAL(RECHECK_FAILED) → final（最短路）。
func returnedTransitions(ref model.OrderRef, final model.WorkOrderStatus) []model.OrderTransition {
	out := []model.OrderTransition{{Ref: ref, RunID: refID, To: model.OrderDraft}}
	appendPath := func(path []model.WorkOrderStatus) {
		for i := 1; i < len(path); i++ {
			tr := model.OrderTransition{Ref: ref, RunID: refID, From: path[i-1], To: path[i]}
			if tr.To == model.OrderExpired {
				tr.ReasonCode = model.ReasonApprovalExpired
			}
			out = append(out, tr)
		}
	}
	appendPath(shortestPath(model.OrderDraft, model.OrderApproved))
	out = append(out, model.OrderTransition{Ref: ref, RunID: refID, From: model.OrderApproved,
		To: model.OrderPendingApproval, ReasonCode: model.ReasonRecheckFailed})
	appendPath(shortestPath(model.OrderPendingApproval, final))
	return out
}
