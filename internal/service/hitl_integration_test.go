//go:build integration

package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// 验收 6 的故障用例：真实 MySQL / Redis 上驱动 DecisionService 与 Scanner。

var hitlDigest = fmt.Sprintf("%064d", 7)

// hitlEnv 是一个槽位上装配好的审批决策与扫描。
type hitlEnv struct {
	slot    *env.Slot
	svc     *service.DecisionService
	scan    *service.Scanner
	locker  *repository.Locker
	resumer *itResumer
	gate    *itGate
}

func newHITLEnv(t *testing.T, e *itest.Env, resumeErr error) *hitlEnv {
	t.Helper()
	slot := e.Fresh(t, t0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := repository.NewDecisionRepo(slot.DB, slot.Clock)
	orders := service.NewWorkOrderService(slot.Clock, repository.WorkOrderRepo{}, repository.EventRepo{})
	h := &hitlEnv{slot: slot, locker: repository.NewLocker(e.Redis), resumer: &itResumer{err: resumeErr}, gate: &itGate{}}
	h.svc = service.NewDecisionService(service.DecisionDeps{Store: store, Orders: orders, Events: repository.EventRepo{},
		Idem: repository.NewRedisIdem(e.Redis), Locks: h.locker, Keys: slot.Keys, Resumer: h.resumer,
		Clock: slot.Clock, Logger: logger})
	h.scan = service.NewScanner(service.ScanDeps{Tickets: store, Runs: repository.NewRunRepo(slot.DB, slot.Clock),
		Expirer: h.svc, Orders: orders, Gate: h.gate, Resumer: h.resumer, Locks: h.locker, Keys: slot.Keys,
		Clock: slot.Clock, Logger: logger})
	return h
}

// waitApproval 造一个中断等待审批的 run、一张 PENDING_APPROVAL 作业单与一张票（OPEN 票 expires_at = t0+1h）。
func (h *hitlEnv) waitApproval(t *testing.T, risk model.RiskLevel, ticketStatus model.TicketStatus) (runID, orderID, ticketID uint64) {
	t.Helper()
	db := h.slot.DB
	runID = itest.InsertRun(t, db, t0)
	h.exec(t, `UPDATE agent_run SET status='INTERRUPTED', interrupt_type='WAIT_APPROVAL' WHERE id=?`, runID)
	orderID = itest.InsertOrder(t, db, runID, string(model.OrderPendingApproval), t0)
	confirm := 0
	if risk == model.RiskHigh {
		confirm = 1
	}
	var expires *time.Time
	if ticketStatus == model.TicketOpen {
		at := t0.Add(time.Hour)
		expires = &at
	}
	h.exec(t, `INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role,
  confirm_required, initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
  VALUES (?, ?, ?, ?, 1, ?, '["agronomist"]', ?, 1001, ?, 1, ?, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, runID, orderID, string(risk), confirm, hitlDigest, string(ticketStatus), expires, t0, t0)
	ticketID = uint64(itest.Count(t, db, `SELECT id FROM approval_ticket WHERE run_id=?`, runID))
	return runID, orderID, ticketID
}

func (h *hitlEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := h.slot.DB.WithContext(t.Context()).Exec(q, args...).Error; err != nil {
		t.Fatal(err)
	}
}

func (h *hitlEnv) decide(t *testing.T, ticketID, user uint64, rid, decision string) (service.DecideResult, error) {
	t.Helper()
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: user, Roles: []model.Role{model.RoleAgronomist}}
	res, err := h.svc.Decide(t.Context(), scope, service.DecideInput{TicketID: ticketID, RequestID: rid,
		Decision: decision, SnapshotDigest: hitlDigest})
	h.svc.Wait()
	return res, err
}

func (h *hitlEnv) scanOnce(t *testing.T) {
	t.Helper()
	if err := h.scan.RunOnce(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	h.scan.Wait()
}

// want 断言一条 COUNT(*) 查询结果为 1。
func (h *hitlEnv) want(t *testing.T, what, q string, args ...any) {
	t.Helper()
	if n := itest.Count(t, h.slot.DB, q, args...); n != 1 {
		t.Errorf("%s: count = %d, want 1", what, n)
	}
}

// itResumer 记录 Resume 调用；err 非空时每次失败（模拟续跑失败）。
type itResumer struct {
	mu    sync.Mutex
	calls []uint64
	err   error
}

func (r *itResumer) Resume(_ context.Context, _ model.Scope, runID uint64) (agent.Answer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runID)
	return agent.Answer{RunID: runID}, r.err
}

func (r *itResumer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// itGate 记录 Activate 调用（L5 ApprovalService 的替身）。
type itGate struct {
	mu   sync.Mutex
	runs []uint64
}

func (g *itGate) Activate(_ context.Context, _ model.Scope, runID uint64) (model.ApprovalTicket, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runs = append(g.runs, runID)
	return model.ApprovalTicket{}, nil
}

// 到期瞬间审批：expires_at 当刻即视为过期，同事务收尾（票据 / 作业单 EXPIRED、run COMPLETED(APPROVAL_EXPIRED)）并写 DENIED 审计；
// 早 1ms 仍可生效。
func TestDecide_ExpiryInstant_Integration(t *testing.T) {
	e := itest.Setup(t, "svchitl")
	tests := []struct {
		name    string
		at      time.Duration // 相对 t0
		wantErr error
		ticket  model.TicketStatus
		order   model.WorkOrderStatus
	}{
		{name: "1ms before expiry", at: time.Hour - time.Millisecond, ticket: model.TicketApproved, order: model.OrderApproved},
		{name: "at expiry instant", at: time.Hour, wantErr: service.ErrTicketExpired, ticket: model.TicketExpired,
			order: model.OrderExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHITLEnv(t, e, nil)
			runID, orderID, ticketID := h.waitApproval(t, model.RiskLow, model.TicketOpen)
			h.slot.Clock.Set(t0.Add(tt.at))
			if _, err := h.decide(t, ticketID, 1002, "it-expiry", service.DecisionApprove); !errors.Is(err, tt.wantErr) {
				t.Fatalf("decide err = %v, want %v", err, tt.wantErr)
			}
			h.want(t, "ticket status", `SELECT COUNT(*) FROM approval_ticket WHERE id=? AND status=?`, ticketID, string(tt.ticket))
			h.want(t, "order status", `SELECT COUNT(*) FROM work_order WHERE id=? AND status=?`, orderID, string(tt.order))
			if tt.wantErr == nil {
				h.want(t, "outbox", `SELECT COUNT(*) FROM outbox WHERE aggregate_id=?`, orderID)
				return
			}
			h.want(t, "run closed", `SELECT COUNT(*) FROM agent_run WHERE id=? AND status='COMPLETED' AND reason_code='APPROVAL_EXPIRED'`, runID)
			h.want(t, "denied audit", `SELECT COUNT(*) FROM audit_log WHERE run_id=? AND action='APPROVAL' AND result='DENIED'`, runID)
			if n := itest.Count(t, h.slot.DB, `SELECT COUNT(*) FROM approval_decision WHERE ticket_id=?`, ticketID); n != 0 {
				t.Errorf("approval_decision rows = %d, want 0", n)
			}
		})
	}
}

// HIGH 票：发起人自审 ROLE_INSUFFICIENT；同一人两次通过 SAME_APPROVER；第二人 CONFIRM 才生效（outbox + deadline + Resume）。
func TestDecide_HighTicketTwoPerson_Integration(t *testing.T) {
	e := itest.Setup(t, "svchitl")
	h := newHITLEnv(t, e, nil)
	runID, orderID, ticketID := h.waitApproval(t, model.RiskHigh, model.TicketOpen)
	h.slot.Clock.Advance(time.Minute)

	if _, err := h.decide(t, ticketID, 1001, "it-self", service.DecisionApprove); !errors.Is(err, service.ErrRoleInsufficient) {
		t.Fatalf("initiator self-approval: %v", err)
	}
	if res, err := h.decide(t, ticketID, 1002, "it-first", service.DecisionApprove); err != nil || !res.AwaitingConfirm {
		t.Fatalf("first approver = %+v, %v; want awaiting confirm", res, err)
	}
	if _, err := h.decide(t, ticketID, 1002, "it-again", service.DecisionApprove); !errors.Is(err, service.ErrSameApprover) {
		t.Fatalf("same approver twice: %v", err)
	}
	if res, err := h.decide(t, ticketID, 1002, "it-first", service.DecisionApprove); err != nil || !res.AwaitingConfirm {
		t.Fatalf("replay first request = %+v, %v; want same result", res, err)
	}
	if h.resumer.count() != 0 {
		t.Fatal("resume before confirm")
	}
	if res, err := h.decide(t, ticketID, 1004, "it-confirm", service.DecisionApprove); err != nil || res.Status != model.TicketApproved {
		t.Fatalf("confirm = %+v, %v", res, err)
	}
	h.want(t, "ticket approved by confirmer", `SELECT COUNT(*) FROM approval_ticket WHERE id=? AND status='APPROVED'
 AND first_approver_id=1002 AND operator_id=1004`, ticketID)
	h.want(t, "decision rows", `SELECT COUNT(*) FROM approval_decision WHERE ticket_id=? AND decision='CONFIRM' AND operator_id=1004`, ticketID)
	h.want(t, "dispatch deadline", `SELECT COUNT(*) FROM work_order WHERE id=? AND status='APPROVED' AND dispatch_deadline_at=?`,
		orderID, h.slot.Clock.Now().Add(service.DefaultDispatchWindow))
	if n := itest.Count(t, h.slot.DB, `SELECT COUNT(*) FROM audit_log WHERE run_id=? AND result='DENIED'`, runID); n != 2 {
		t.Errorf("denied audits = %d, want 2 (self + same approver)", n)
	}
	if h.resumer.count() != 1 {
		t.Errorf("resume calls = %d, want 1", h.resumer.count())
	}
}

// Resume 失败：决策已生效、run 仍 INTERRUPTED(WAIT_APPROVAL)；60s 内扫描不重试，超过后 run_finalize 重新触发 Resume。
func TestResumeFailure_RunFinalize_Integration(t *testing.T) {
	e := itest.Setup(t, "svchitl")
	h := newHITLEnv(t, e, errors.New("resume boom"))
	runID, _, ticketID := h.waitApproval(t, model.RiskLow, model.TicketOpen)
	h.slot.Clock.Advance(time.Minute)
	if res, err := h.decide(t, ticketID, 1002, "it-resume", service.DecisionApprove); err != nil || res.Status != model.TicketApproved {
		t.Fatalf("approve = %+v, %v", res, err)
	}
	h.want(t, "run still interrupted", `SELECT COUNT(*) FROM agent_run WHERE id=? AND status='INTERRUPTED' AND interrupt_type='WAIT_APPROVAL'`, runID)
	h.slot.Clock.Advance(30 * time.Second)
	h.scanOnce(t)
	if h.resumer.count() != 1 {
		t.Fatalf("resume calls within grace = %d, want 1", h.resumer.count())
	}
	h.slot.Clock.Advance(31 * time.Second)
	h.scanOnce(t)
	if h.resumer.count() != 2 {
		t.Fatalf("resume calls after grace = %d, want 2", h.resumer.count())
	}
}

// 票据卡在 PREPARING：30s 内不动；之后 run 已中断则补 Activate，run 已终态则作废票据并把作业单转 EXPIRED。
func TestTicketActivate_StuckPreparing_Integration(t *testing.T) {
	e := itest.Setup(t, "svchitl")
	tests := []struct {
		name         string
		runStatus    string // 空表示保持 INTERRUPTED(WAIT_APPROVAL)
		wantActivate int
		wantTicket   model.TicketStatus
		wantOrder    model.WorkOrderStatus
	}{
		{name: "run interrupted is activated", wantActivate: 1, wantTicket: model.TicketPreparing,
			wantOrder: model.OrderPendingApproval},
		{name: "run failed voids ticket", runStatus: "FAILED", wantTicket: model.TicketExpired, wantOrder: model.OrderExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHITLEnv(t, e, nil)
			runID, orderID, ticketID := h.waitApproval(t, model.RiskLow, model.TicketPreparing)
			if tt.runStatus != "" {
				h.exec(t, `UPDATE agent_run SET status=?, interrupt_type=NULL WHERE id=?`, tt.runStatus, runID)
			}
			h.slot.Clock.Advance(20 * time.Second)
			h.scanOnce(t)
			if len(h.gate.runs) != 0 || itest.Count(t, h.slot.DB, `SELECT COUNT(*) FROM approval_ticket WHERE id=? AND status='PREPARING'`, ticketID) != 1 {
				t.Fatal("PREPARING ticket touched within grace")
			}
			h.slot.Clock.Advance(11 * time.Second)
			h.scanOnce(t)
			if len(h.gate.runs) != tt.wantActivate {
				t.Errorf("activate calls = %v, want %d", h.gate.runs, tt.wantActivate)
			}
			h.want(t, "ticket status", `SELECT COUNT(*) FROM approval_ticket WHERE id=? AND status=?`, ticketID, string(tt.wantTicket))
			h.want(t, "order status", `SELECT COUNT(*) FROM work_order WHERE id=? AND status=?`, orderID, string(tt.wantOrder))
		})
	}
}

// 心跳丢失：RUNNING 超过 90s 未续期且 lock:run 不存在 → INTERRUPTED(SYSTEM_ERROR)、sys_retry+1 → Resume；
// 不可续（无检查点）则 FAILED(RETRY_EXHAUSTED) 并取消 DRAFT 作业单；lock:run 仍被持有时不动；重试超过 3 次直接失败。
func TestStuckRun_HeartbeatLost_Integration(t *testing.T) {
	e := itest.Setup(t, "svchitl")
	tests := []struct {
		name       string
		lockHeld   bool
		sysRetry   int
		wantStatus model.RunStatus
		wantResume int
	}{
		{name: "lock still held", lockHeld: true, wantStatus: model.RunRunning},
		{name: "unresumable fails run", wantStatus: model.RunFailed, wantResume: 1},
		{name: "retry exhausted", sysRetry: 3, wantStatus: model.RunFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHITLEnv(t, e, agent.ErrNotResumable)
			runID := itest.InsertRun(t, h.slot.DB, t0)
			orderID := itest.InsertOrder(t, h.slot.DB, runID, string(model.OrderDraft), t0)
			h.exec(t, `UPDATE agent_run SET sys_retry=? WHERE id=?`, tt.sysRetry, runID)
			if tt.lockHeld {
				lk, err := h.locker.Acquire(t.Context(), h.slot.Keys.LockRun(runID), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := lk.Release(context.WithoutCancel(t.Context())); err != nil {
						t.Errorf("release run lock: %v", err)
					}
				})
			}
			h.slot.Clock.Advance(91 * time.Second)
			h.scanOnce(t)
			h.want(t, "run status", `SELECT COUNT(*) FROM agent_run WHERE id=? AND status=?`, runID, string(tt.wantStatus))
			if h.resumer.count() != tt.wantResume {
				t.Errorf("resume calls = %d, want %d", h.resumer.count(), tt.wantResume)
			}
			if tt.wantStatus != model.RunFailed {
				return
			}
			h.want(t, "retry counted", `SELECT COUNT(*) FROM agent_run WHERE id=? AND reason_code='RETRY_EXHAUSTED' AND sys_retry=?`,
				runID, tt.sysRetry+1)
			h.want(t, "draft canceled", `SELECT COUNT(*) FROM work_order WHERE id=? AND status='CANCELED'`, orderID)
		})
	}
}
