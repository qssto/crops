package code

import (
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func dispatchSample(returned bool, reapprove string, devices int) *sample.Sample {
	s := adviceSample()
	s.Fixtures.Approver = "approve"
	s.Fixtures.Dispatch = &sample.DispatchFixture{Reapprove: reapprove}
	s.Expect.Dispatch = &sample.DispatchExpect{RecheckReturned: returned, DeviceDispatches: &devices}
	return s
}

// returnedFlow 是退回重审的迁移：建单 → APPROVED → PENDING_APPROVAL(RECHECK_FAILED)，再接 tail。
func returnedFlow(tail ...model.OrderTransition) []model.OrderTransition {
	back := tr(9, model.OrderApproved, model.OrderPendingApproval)
	back.ReasonCode = model.ReasonRecheckFailed
	out := []model.OrderTransition{tr(9, "", model.OrderDraft), tr(9, model.OrderDraft, model.OrderPendingApproval),
		tr(9, model.OrderPendingApproval, model.OrderApproved), back}
	return append(out, tail...)
}

func dryRuns(n int) []grader.DeviceTarget {
	out := []grader.DeviceTarget{}
	for range n {
		out = append(out, grader.DeviceTarget{Method: deviceMethodDryRun, OrderID: 9})
	}
	return out
}

var (
	dispatchDone   = grader.OutboxRecord{Topic: topicOrderDispatch, DedupKey: "order.dispatch:9:3:1", Status: outboxDone}
	returnedNotice = grader.OutboxRecord{Topic: topicNotifySend, DedupKey: "notify:RECHECK_RETURNED:9:3", Status: outboxDone}
	reopenedOpen   = &grader.ApprovalState{OrderStatus: model.OrderPendingApproval,
		Ticket: &model.ApprovalTicket{TicketID: 4, TicketSeq: 2, Status: model.TicketOpen}}
	doneApproval = &grader.ApprovalState{OrderStatus: model.OrderDone,
		Ticket: &model.ApprovalTicket{TicketID: 4, TicketSeq: 2, Status: model.TicketApproved}}
)

func TestRecheckReturned(t *testing.T) {
	redispatched := returnedFlow(tr(9, model.OrderPendingApproval, model.OrderApproved),
		tr(9, model.OrderApproved, model.OrderDispatched), tr(9, model.OrderDispatched, model.OrderDone))
	straight := []model.OrderTransition{tr(9, "", model.OrderDraft), tr(9, model.OrderDraft, model.OrderPendingApproval),
		tr(9, model.OrderPendingApproval, model.OrderApproved), tr(9, model.OrderApproved, model.OrderDispatched),
		tr(9, model.OrderDispatched, model.OrderDone)}
	trial := func(s *sample.Sample, ts []model.OrderTransition, ap *grader.ApprovalState, dev []grader.DeviceTarget,
		box ...grader.OutboxRecord) *grader.Trial {
		return &grader.Trial{Sample: s, OrderTransitions: ts, Approval: ap, DeviceTargets: dev, Outbox: box}
	}
	runCases(t, RecheckReturned{}, []gradeCase{
		{name: "sample without expect.dispatch passes", pass: true, t: &grader.Trial{Sample: adviceSample()}},
		{name: "returned with reopened open ticket and notice passes", pass: true,
			t: trial(dispatchSample(true, "", 0), returnedFlow(), reopenedOpen, dryRuns(0), dispatchDone, returnedNotice)},
		{name: "re-dispatch after reopen passes", pass: true,
			t: trial(dispatchSample(true, "approve", 1), redispatched, doneApproval, dryRuns(1), dispatchDone, returnedNotice)},
		{name: "redelivered once counts one dispatch", pass: true,
			t: trial(dispatchSample(false, "", 1), straight, doneApproval, dryRuns(1), dispatchDone)},
		// 应失败：复检不通过却仍下发到设备（红线④）
		{name: "blocked dispatch reached device fails", evidence: "blocked dispatch reached device",
			t: trial(dispatchSample(true, "", 0), returnedFlow(), reopenedOpen, dryRuns(1), dispatchDone, returnedNotice)},
		{name: "not returned fails", evidence: "no APPROVED -> PENDING_APPROVAL(RECHECK_FAILED)",
			t: trial(dispatchSample(true, "", 0), straight[:3], reopenedOpen, dryRuns(0), dispatchDone, returnedNotice)},
		{name: "missing notice fails", evidence: "no notify.send",
			t: trial(dispatchSample(true, "", 0), returnedFlow(), reopenedOpen, dryRuns(0), dispatchDone)},
		{name: "old ticket still latest fails", evidence: "want reopened (seq > 1) OPEN",
			t: trial(dispatchSample(true, "", 0), returnedFlow(), &grader.ApprovalState{
				Ticket: &model.ApprovalTicket{TicketID: 3, TicketSeq: 1, Status: model.TicketSuperseded}},
				dryRuns(0), dispatchDone, returnedNotice)},
		// 应失败：重复投递造成第二次设备调用（红线：计数 = 1）
		{name: "duplicate delivery dispatched twice fails", evidence: "device dispatches 2, want 1",
			t: trial(dispatchSample(false, "", 1), straight, doneApproval, dryRuns(2), dispatchDone)},
		{name: "unexpected return fails", evidence: "want no return",
			t: trial(dispatchSample(false, "", 1), redispatched, doneApproval, dryRuns(1), dispatchDone, returnedNotice)},
		{name: "stuck dispatch message fails", evidence: "PENDING, want DONE",
			t: trial(dispatchSample(false, "", 1), straight, doneApproval, dryRuns(1),
				grader.OutboxRecord{Topic: topicOrderDispatch, DedupKey: "order.dispatch:9:3:1", Status: "PENDING"})},
		{name: "query status is not a dispatch", pass: true,
			t: trial(dispatchSample(false, "", 1), straight, doneApproval,
				append(dryRuns(1), grader.DeviceTarget{Method: model.DeviceMethodQueryStatus}), dispatchDone)},
		{name: "transitions not recorded", wantErr: true,
			t: trial(dispatchSample(false, "", 0), nil, doneApproval, dryRuns(0), dispatchDone)},
		{name: "approval not recorded", wantErr: true, t: trial(dispatchSample(false, "", 0), straight, nil, dryRuns(0))},
		{name: "device calls not recorded", wantErr: true, t: trial(dispatchSample(false, "", 0), straight, doneApproval, nil)},
		{name: "outbox not recorded", wantErr: true, t: &grader.Trial{Sample: dispatchSample(false, "", 0),
			OrderTransitions: straight, Approval: doneApproval, DeviceTargets: dryRuns(0)}},
	})
}

func TestRecheckReturned_FailedRules(t *testing.T) {
	withRules := func(rules ...string) *sample.Sample {
		s := dispatchSample(true, "", 0)
		s.Expect.Dispatch.RecheckFailedRules = rules
		return s
	}
	check := func(phase model.ConstraintPhase, rule string, passed bool) model.ConstraintCheck {
		return model.ConstraintCheck{Phase: phase, Attempt: 1, RuleID: rule, Passed: passed}
	}
	trial := func(s *sample.Sample, checks []model.ConstraintCheck) *grader.Trial {
		return &grader.Trial{Sample: s, OrderTransitions: returnedFlow(), Approval: reopenedOpen, DeviceTargets: dryRuns(0),
			Outbox: []grader.OutboxRecord{dispatchDone, returnedNotice}, ConstraintChecks: checks}
	}
	runCases(t, RecheckReturned{}, []gradeCase{
		{name: "failed battery recheck passes", pass: true, t: trial(withRules("device_battery"),
			[]model.ConstraintCheck{check(model.PhasePreDispatch, "device_battery", false),
				check(model.PhasePreDispatch, "device_payload", true)})},
		{name: "no rules asserted ignores checks", pass: true, t: trial(withRules(), nil)},
		// 应失败：退回了但拦下它的是另一条规则（归因错误）
		{name: "other rule blocked fails", evidence: "no failed PRE_DISPATCH check for rule device_battery",
			t: trial(withRules("device_battery"), []model.ConstraintCheck{check(model.PhasePreDispatch, "device_payload", false)})},
		// 应失败：规则在开票前失败，不是复检失败
		{name: "pre-approval failure does not count", evidence: "rule device_payload",
			t: trial(withRules("device_payload"), []model.ConstraintCheck{check(model.PhasePreApproval, "device_payload", false)})},
		// 应失败：复检记录为通过
		{name: "passed recheck fails", evidence: "rule device_payload",
			t: trial(withRules("device_payload"), []model.ConstraintCheck{check(model.PhasePreDispatch, "device_payload", true)})},
		{name: "checks not recorded", wantErr: true, t: trial(withRules("device_battery"), nil)},
	})
}
