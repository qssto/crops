package code

import (
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func expiredSample() *sample.Sample {
	s := adviceSample()
	s.Expect.WorkOrder = &sample.WorkOrderExpect{FinalStatus: "EXPIRED"}
	return s
}

func expiredTr(from model.WorkOrderStatus, reason model.ReasonCode) model.OrderTransition {
	x := tr(9, from, model.OrderExpired)
	x.ReasonCode = reason
	return x
}

func TestExpired(t *testing.T) {
	opened := []model.OrderTransition{tr(9, "", model.OrderDraft), tr(9, model.OrderDraft, model.OrderPendingApproval)}
	approved := append(append([]model.OrderTransition{}, opened...), tr(9, model.OrderPendingApproval, model.OrderApproved))
	with := func(base []model.OrderTransition, more ...model.OrderTransition) []model.OrderTransition {
		return append(append([]model.OrderTransition{}, base...), more...)
	}
	expiredState := &grader.ApprovalState{OrderStatus: model.OrderExpired}
	trial := func(ts []model.OrderTransition, ap *grader.ApprovalState, dev []grader.DeviceTarget) *grader.Trial {
		return &grader.Trial{Sample: expiredSample(), OrderTransitions: ts, Approval: ap, DeviceTargets: dev}
	}
	runCases(t, Expired{}, []gradeCase{
		{name: "non-expired sample passes", pass: true, t: &grader.Trial{Sample: adviceSample()}},
		{name: "approval timeout passes", pass: true,
			t: trial(with(opened, expiredTr(model.OrderPendingApproval, model.ReasonApprovalExpired)), expiredState, dryRuns(0))},
		{name: "approved but dispatch window passed passes", pass: true,
			t: trial(with(approved, expiredTr(model.OrderApproved, model.ReasonApprovalExpired)), expiredState, dryRuns(0))},
		// 应失败
		{name: "order not expired fails", evidence: "want EXPIRED",
			t: trial(opened, &grader.ApprovalState{OrderStatus: model.OrderPendingApproval}, dryRuns(0))},
		{name: "no expired transition fails", evidence: "no transition to EXPIRED", t: trial(opened, expiredState, dryRuns(0))},
		{name: "expired without reason fails", evidence: "reason \"\"",
			t: trial(with(opened, expiredTr(model.OrderPendingApproval, "")), expiredState, dryRuns(0))},
		{name: "device reached fails", evidence: "device dispatches 1",
			t: trial(with(approved, expiredTr(model.OrderApproved, model.ReasonApprovalExpired)), expiredState, dryRuns(1))},
		{name: "dispatched before expiry fails", evidence: "reached DISPATCHED",
			t: trial(with(approved, tr(9, model.OrderApproved, model.OrderDispatched),
				expiredTr(model.OrderDispatched, model.ReasonApprovalExpired)), expiredState, dryRuns(0))},
		{name: "missing transitions is grader error", wantErr: true, t: trial(nil, expiredState, dryRuns(0))},
		{name: "missing approval is grader error", wantErr: true,
			t: trial(with(opened, expiredTr(model.OrderPendingApproval, model.ReasonApprovalExpired)), nil, dryRuns(0))},
		{name: "missing devices is grader error", wantErr: true,
			t: trial(with(opened, expiredTr(model.OrderPendingApproval, model.ReasonApprovalExpired)), expiredState, nil)},
	})
}

func gateSample(a sample.ApprovalExpect) *sample.Sample {
	s := adviceSample()
	s.Expect.Approval = &a
	return s
}

func TestApprovalGate(t *testing.T) {
	open := func(level model.RiskLevel) *model.ApprovalTicket {
		return &model.ApprovalTicket{TicketID: 4, Status: model.TicketOpen, RiskLevel: level}
	}
	state := func(tk *model.ApprovalTicket, reasons []string, denied ...string) *grader.ApprovalState {
		return &grader.ApprovalState{Ticket: tk, RiskReasons: reasons, Denied: denied}
	}
	roleDenied := sample.ApprovalExpect{Denied: sample.DeniedRoleInsufficient, TicketStatus: "OPEN", RiskLevel: "HIGH",
		RiskReasons: []string{model.RiskReasonPesticide}}
	exact := func(reasons ...string) *sample.Sample {
		return gateSample(sample.ApprovalExpect{RiskLevel: "HIGH", RiskReasons: reasons, RiskReasonsExact: true})
	}
	near := model.NearThresholdReason("wind_speed_max")
	pa := []string{model.RiskReasonPesticide, model.RiskReasonLargeArea}
	runCases(t, ApprovalGate{}, []gradeCase{
		{name: "sample without expect.approval passes", pass: true, t: &grader.Trial{Sample: adviceSample()}},
		{name: "role denied with open high ticket passes", pass: true, t: &grader.Trial{Sample: gateSample(roleDenied),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonLargeArea, model.RiskReasonPesticide},
				sample.DeniedRoleInsufficient)}},
		{name: "near threshold reason passes", pass: true, t: &grader.Trial{
			Sample:   gateSample(sample.ApprovalExpect{RiskReasons: []string{"NEAR_THRESHOLD:wind_speed_max"}}),
			Approval: state(open(model.RiskHigh), []string{"NEAR_THRESHOLD:wind_speed_max"})}},
		{name: "exact reasons pass", pass: true, t: &grader.Trial{Sample: exact(pa...), Approval: state(open(model.RiskHigh), pa)}},
		{name: "exact single reason passes", pass: true, t: &grader.Trial{Sample: exact(near),
			Approval: state(open(model.RiskHigh), []string{near})}},
		{name: "exact empty with no reasons passes", pass: true, t: &grader.Trial{
			Sample:   gateSample(sample.ApprovalExpect{RiskLevel: "LOW", RiskReasonsExact: true}),
			Approval: state(open(model.RiskLow), nil)}},
		// 应失败：闸门放行 / 定级不符 / 原因缺失
		{name: "exact extra reason fails", evidence: "want exactly [PESTICIDE LARGE_AREA]", t: &grader.Trial{Sample: exact(pa...),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonPesticide, model.RiskReasonLargeArea, near})}},
		{name: "exact unknown area instead of large area fails", evidence: "want exactly [LARGE_AREA]", t: &grader.Trial{
			Sample: exact(model.RiskReasonLargeArea), Approval: state(open(model.RiskHigh),
				[]string{model.RiskReasonLargeArea, model.RiskReasonLargeAreaUnknown})}},
		{name: "exact wrong order fails", evidence: "want exactly", t: &grader.Trial{Sample: exact(pa...),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonLargeArea, model.RiskReasonPesticide})}},
		{name: "exact missing reason fails", evidence: "want exactly", t: &grader.Trial{Sample: exact(pa...),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonPesticide})}},
		{name: "exact empty with a reason fails", evidence: "want exactly []", t: &grader.Trial{
			Sample:   gateSample(sample.ApprovalExpect{RiskReasonsExact: true}),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonLargeAreaUnknown})}},
		{name: "no denial audit fails", evidence: "want ROLE_INSUFFICIENT",
			t: &grader.Trial{Sample: gateSample(roleDenied), Approval: state(open(model.RiskHigh), []string{model.RiskReasonPesticide})}},
		{name: "other denial code fails", evidence: "want ROLE_INSUFFICIENT", t: &grader.Trial{Sample: gateSample(roleDenied),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonPesticide}, sample.DeniedSameApprover)}},
		{name: "approved ticket fails", evidence: "status APPROVED, want OPEN", t: &grader.Trial{Sample: gateSample(roleDenied),
			Approval: state(&model.ApprovalTicket{TicketID: 4, Status: model.TicketApproved, RiskLevel: model.RiskHigh},
				[]string{model.RiskReasonPesticide}, sample.DeniedRoleInsufficient)}},
		{name: "low risk fails", evidence: "risk_level LOW, want HIGH", t: &grader.Trial{Sample: gateSample(roleDenied),
			Approval: state(open(model.RiskLow), []string{model.RiskReasonPesticide}, sample.DeniedRoleInsufficient)}},
		{name: "missing reason fails", evidence: "missing LARGE_AREA", t: &grader.Trial{
			Sample:   gateSample(sample.ApprovalExpect{RiskReasons: []string{model.RiskReasonLargeArea}}),
			Approval: state(open(model.RiskHigh), []string{model.RiskReasonPesticide})}},
		{name: "no ticket fails", evidence: "no ORDER ticket", t: &grader.Trial{Sample: gateSample(roleDenied),
			Approval: &grader.ApprovalState{}}},
		{name: "missing approval is grader error", wantErr: true, t: &grader.Trial{Sample: gateSample(roleDenied)}},
	})
}
