package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/model"
)

// P1 L3：开票前约束拦截。拦截时同事务写 constraint_check(PRE_APPROVAL, attempt=1) + CONSTRAINT_BLOCKED
// （P1-L3 审查 4：事件与校验行同事务）：不写快照、不建票、不迁移。
func TestApprovalOpen_ConstraintBlocked(t *testing.T) {
	f := newApprovalFixtureWith(t, &fakeTickets{}, model.OrderDraft, fakeRisk{},
		&fakeReviewer{blocked: []string{constraint.RuleRegistered, constraint.RuleWind}})
	_, err := f.svc.Open(context.Background(), approvalScope, openRequest())
	if !errors.Is(err, agent.ErrConstraintBlocked) {
		t.Fatalf("err = %v, want ErrConstraintBlocked", err)
	}
	var blocked *agent.ConstraintBlockedError
	if !errors.As(err, &blocked) || blocked.OrderID != 9 ||
		!slices.Equal(blocked.RuleIDs, []string{constraint.RuleRegistered, constraint.RuleWind}) {
		t.Fatalf("blocked = %+v", blocked)
	}
	if f.tx.calls != 1 || f.tickets.checkAttempt != firstTicketSeq || len(f.tickets.checks) != 1 {
		t.Errorf("tx=%d attempt=%d checks=%d", f.tx.calls, f.tickets.checkAttempt, len(f.tickets.checks))
	}
	if len(f.tickets.inserted) != 0 || f.orders.updated || len(f.orders.transitions) != 0 || len(f.raw.puts) != 0 {
		t.Errorf("blocked open wrote ticket=%d transition=%v puts=%d", len(f.tickets.inserted), f.orders.updated, len(f.raw.puts))
	}
	if got := eventTypes(f.events.events); !slices.Equal(got, []model.EventType{model.EventConstraintBlocked}) {
		t.Fatalf("events = %v, want [CONSTRAINT_BLOCKED]", got)
	}
	e := f.events.events[0]
	var p model.ConstraintBlockedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if e.TenantID != 1 || e.FarmID != 2 || e.RunID != 5 || e.Actor != model.ActorAgent || !e.CreatedAt.Equal(approvalNow) ||
		p.OrderID != 9 || !slices.Equal(p.RuleIDs, blocked.RuleIDs) {
		t.Errorf("event %+v payload %+v", e, p)
	}
}

// P1-L3 审查 2：拦截后修正再开票，同一 attempt 的校验行被覆盖为通过行并建票；拦截留下的 CONSTRAINT_BLOCKED 仍可追溯。
func TestApprovalOpen_BlockedThenPasses(t *testing.T) {
	rv := &fakeReviewer{blocked: []string{constraint.RuleWind}}
	f := newApprovalFixtureWith(t, &fakeTickets{}, model.OrderDraft, fakeRisk{}, rv)
	req := openRequest()
	req.Order.ConstraintChecks = []model.ConstraintCheck{{Phase: model.PhasePreApproval, RuleID: constraint.RuleWind}}
	if _, err := f.svc.Open(context.Background(), approvalScope, req); !errors.Is(err, agent.ErrConstraintBlocked) {
		t.Fatalf("first open err = %v", err)
	}
	rv.blocked = nil
	req.Order.ConstraintChecks = []model.ConstraintCheck{{Phase: model.PhasePreApproval, RuleID: constraint.RuleWind, Passed: true}}
	got, err := f.svc.Open(context.Background(), approvalScope, req)
	if err != nil || got.TicketID != 77 {
		t.Fatalf("second open = %+v, %v", got, err)
	}
	if f.tickets.upserts != 2 || f.tickets.checkAttempt != firstTicketSeq || len(f.tickets.checks) != 1 || !f.tickets.checks[0].Passed {
		t.Errorf("upserts=%d attempt=%d checks=%+v", f.tickets.upserts, f.tickets.checkAttempt, f.tickets.checks)
	}
	want := []model.EventType{model.EventConstraintBlocked, model.EventOrderTransition, model.EventApprovalOpen}
	if got := eventTypes(f.events.events); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// P1-L3 审查 2：拦截事务内带锁读到并发开出的票，回滚本次写入（不发 CONSTRAINT_BLOCKED），返回库中的票。
func TestApprovalOpen_BlockedLosesToConcurrentTicket(t *testing.T) {
	winner := model.ApprovalTicket{TicketID: 41, Status: model.TicketPreparing}
	f := newApprovalFixtureWith(t, &fakeTickets{lockedInTx: &winner}, model.OrderDraft, fakeRisk{},
		&fakeReviewer{blocked: []string{constraint.RuleWind}})
	got, err := f.svc.Open(context.Background(), approvalScope, openRequest())
	if err != nil || got.TicketID != 41 {
		t.Fatalf("open = %+v, %v", got, err)
	}
	if !errors.Is(f.tx.err, errTicketRaced) || len(f.events.events) != 0 || len(f.tickets.inserted) != 0 {
		t.Errorf("tx err=%v events=%v inserted=%d", f.tx.err, eventTypes(f.events.events), len(f.tickets.inserted))
	}
}

// 复核按库中 work_type / device_type / dispatch_mode（请求 dispatch_mode 为空时以库中 DRY_RUN 复核）；
// 已有票（整图重跑）不再复核，幂等不变。
func TestApprovalOpen_ReviewUsesStoredOrderAndSkipsExisting(t *testing.T) {
	rv := &fakeReviewer{}
	f := newApprovalFixtureWith(t, &fakeTickets{}, model.OrderDraft, fakeRisk{}, rv)
	if _, err := f.svc.Open(context.Background(), approvalScope, openRequest()); err != nil {
		t.Fatal(err)
	}
	if len(rv.seen) != 1 || rv.seen[0].WorkType != model.WorkSpray || rv.seen[0].OrderID != 9 ||
		rv.seen[0].DeviceType != "knapsack" || rv.seen[0].DispatchMode != model.DispatchDryRun {
		t.Fatalf("reviewed %+v", rv.seen)
	}
	prev := model.ApprovalTicket{TicketID: 33, Status: model.TicketPreparing}
	rv2 := &fakeReviewer{blocked: []string{constraint.RuleWind}}
	f2 := newApprovalFixtureWith(t, &fakeTickets{existing: &prev}, model.OrderPendingApproval, fakeRisk{}, rv2)
	if got, err := f2.svc.Open(context.Background(), approvalScope, openRequest()); err != nil || got.TicketID != 33 {
		t.Fatalf("existing ticket: %+v, %v", got, err)
	}
	if len(rv2.seen) != 0 {
		t.Errorf("existing ticket reviewed again")
	}
}

// 真实 Reviewer（内置注册表 + 仓库 policy/regulation_map.yaml）：空校验行、缺必检项、未映射法规条目都拦截。
func TestApprovalOpen_RealReviewerBlocks(t *testing.T) {
	b, err := os.ReadFile("../../policy/regulation_map.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg := constraint.Default()
	regs, err := constraint.ParseRegulationMap(b, reg)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*model.WorkOrder, *model.OrderState) // 请求与库中作业单同改（二者须一致）
		want   []string                                  // 至少包含
	}{
		{name: "empty checks", mutate: func(o *model.WorkOrder, _ *model.OrderState) { o.ConstraintChecks = nil },
			want: []string{constraint.RuleRegistered, constraint.RuleWind, constraint.RulePHI}},
		{name: "unregistered rows ignored", mutate: func(*model.WorkOrder, *model.OrderState) {},
			want: []string{constraint.RuleRegistered, constraint.RuleWind}},
		{name: "uav real hits unmapped regulations", mutate: func(o *model.WorkOrder, st *model.OrderState) {
			o.DeviceType, o.DispatchMode = constraint.DeviceUAV, model.DispatchReal
			st.DeviceType, st.DispatchMode = constraint.DeviceUAV, model.DispatchReal
		}, want: []string{constraint.UnmappedPrefix + "uav-flight-plan", constraint.RuleNoFly}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newApprovalFixtureWith(t, &fakeTickets{}, model.OrderDraft, fakeRisk{}, constraint.NewReviewer(reg, regs))
			req := openRequest()
			tt.mutate(&req.Order, &f.orders.state)
			_, err := f.svc.Open(context.Background(), approvalScope, req)
			var blocked *agent.ConstraintBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("err = %v, want ConstraintBlockedError", err)
			}
			for _, id := range tt.want {
				if !slices.Contains(blocked.RuleIDs, id) {
					t.Errorf("rule_ids %v missing %s", blocked.RuleIDs, id)
				}
			}
			if len(f.tickets.checks) == 0 || f.tickets.checkAttempt != firstTicketSeq || len(f.tickets.inserted) != 0 {
				t.Errorf("checks=%d attempt=%d inserted=%d", len(f.tickets.checks), f.tickets.checkAttempt, len(f.tickets.inserted))
			}
			for _, c := range f.tickets.checks {
				if c.Phase != model.PhasePreApproval || len(c.RuleID) > 48 {
					t.Errorf("row %+v", c)
				}
			}
		})
	}
}
