package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// memDecisions 是 DecisionStore 的内存 fake：InTx 失败时回滚到事务前快照（模拟单事务）；守卫语义同 DecisionRepo 的 SQL。
type memDecisions struct {
	mu        sync.Mutex
	ticket    repository.TicketRow
	runStatus model.RunStatus
	runReason model.ReasonCode
	decisions []repository.DecisionRow
	outbox    int
	memories  int
	deadline  *time.Time
	audits    []string
	failOn    string // 指定一步返回错误，验证回滚
	// 待审批列表
	pending    []repository.PendingRow
	orders     map[uint64]model.WorkOrder
	orderCalls [][]uint64
}

func (m *memDecisions) snapshot() memDecisions {
	return memDecisions{ticket: m.ticket, runStatus: m.runStatus, runReason: m.runReason,
		decisions: append([]repository.DecisionRow(nil), m.decisions...), outbox: m.outbox, memories: m.memories, deadline: m.deadline}
}

func (m *memDecisions) InTx(_ context.Context, fn func(tx *repository.DB) error) error {
	before := m.snapshot()
	if err := fn(nil); err != nil {
		m.ticket, m.runStatus, m.runReason, m.decisions = before.ticket, before.runStatus, before.runReason, before.decisions
		m.outbox, m.memories, m.deadline = before.outbox, before.memories, before.deadline
		return err
	}
	return nil
}

func (m *memDecisions) fail(step string) error {
	if m.failOn == step {
		return fmt.Errorf("injected %s failure", step)
	}
	return nil
}

func (m *memDecisions) TicketTx(_ context.Context, _ *repository.DB, scope model.Scope, id uint64) (repository.TicketRow, error) {
	if id != m.ticket.ID || scope.FarmID != m.ticket.FarmID {
		return repository.TicketRow{}, model.ErrNotFound
	}
	return m.ticket, nil
}

func (m *memDecisions) DecisionByRequest(_ context.Context, rid string) (repository.DecisionRow, bool, error) {
	for _, d := range m.decisions {
		if d.RequestID == rid {
			return d, true, nil
		}
	}
	return repository.DecisionRow{}, false, nil
}

func (m *memDecisions) InsertDecisionTx(_ context.Context, _ *repository.DB, d repository.DecisionRow) (uint64, error) {
	d.ID = uint64(len(m.decisions) + 1)
	m.decisions = append(m.decisions, d)
	return d.ID, nil
}

func (m *memDecisions) guard(t repository.TicketRow, now time.Time) bool {
	c := m.ticket
	return c.Status == string(model.TicketOpen) && c.RowVer == t.RowVer && c.ExpiresAt != nil && c.ExpiresAt.After(now)
}

func (m *memDecisions) DecideTicketTx(_ context.Context, _ *repository.DB, t repository.TicketRow, to model.TicketStatus, op uint64, now time.Time) error {
	if !m.guard(t, now) || m.ticket.SnapshotDigest != t.SnapshotDigest {
		return model.ErrConcurrentUpdate
	}
	m.ticket.Status, m.ticket.OperatorID, m.ticket.DecidedAt = string(to), &op, &now
	m.ticket.RowVer++
	return nil
}

func (m *memDecisions) SetFirstApproverTx(_ context.Context, _ *repository.DB, t repository.TicketRow, uid uint64, now time.Time) error {
	if !m.guard(t, now) || m.ticket.FirstApproverID != nil {
		return model.ErrConcurrentUpdate
	}
	m.ticket.FirstApproverID = &uid
	m.ticket.RowVer++
	return nil
}

func (m *memDecisions) ExpireTicketTx(_ context.Context, _ *repository.DB, t repository.TicketRow, _ time.Time) (bool, error) {
	if m.ticket.Status != string(model.TicketOpen) || m.ticket.RowVer != t.RowVer {
		return false, nil
	}
	m.ticket.Status = string(model.TicketExpired)
	m.ticket.RowVer++
	return true, nil
}

func (m *memDecisions) SetDispatchDeadlineTx(_ context.Context, _ *repository.DB, _ model.OrderRef, d, _ time.Time) error {
	m.deadline = &d
	return m.fail("deadline")
}

func (m *memDecisions) InsertDispatchOutboxTx(context.Context, *repository.DB, repository.TicketRow, []byte, time.Time) error {
	m.outbox++
	return m.fail("outbox")
}

func (m *memDecisions) CloseRunTx(_ context.Context, _ *repository.DB, _ model.Scope, _ uint64, r model.ReasonCode, _ time.Time) error {
	if m.runStatus != model.RunInterrupted {
		return model.ErrConcurrentUpdate
	}
	m.runStatus, m.runReason = model.RunCompleted, r
	return nil
}

func (m *memDecisions) InsertRejectedMemoryTx(context.Context, *repository.DB, repository.TicketRow, string, time.Time) error {
	m.memories++
	return nil
}

func (m *memDecisions) AuditDenied(_ context.Context, _ model.Scope, _ repository.TicketRow, code string) error {
	m.audits = append(m.audits, code)
	return nil
}

func (m *memDecisions) ListPending(context.Context, model.Scope) ([]repository.PendingRow, error) {
	return append([]repository.PendingRow(nil), m.pending...), nil
}

func (m *memDecisions) OrdersWithChecks(_ context.Context, _ model.Scope, ids []uint64) (map[uint64]model.WorkOrder, error) {
	m.orderCalls = append(m.orderCalls, ids)
	out := map[uint64]model.WorkOrder{}
	for _, id := range ids {
		if o, ok := m.orders[id]; ok {
			out[id] = o
		}
	}
	return out, m.fail("orders")
}

// memTransitions 记录作业单迁移（Transition 的状态机校验由 WorkOrderService 单测覆盖）。
type memTransitions struct{ to []model.WorkOrderStatus }

func (o *memTransitions) Transition(_ context.Context, _ *repository.DB, _ model.OrderRef, to model.WorkOrderStatus, _ model.Actor, _ model.ReasonCode) error {
	o.to = append(o.to, to)
	return nil
}

func (o *memTransitions) TransitionFrom(ctx context.Context, tx *repository.DB, ref model.OrderRef, _, to model.WorkOrderStatus,
	actor model.Actor, reason model.ReasonCode) error {
	return o.Transition(ctx, tx, ref, to, actor, reason)
}

// memIdem 是 IdemStore 的内存实现（不模拟过期）。
type memIdem struct {
	mu sync.Mutex
	m  map[string]string
}

func (i *memIdem) SetNX(_ context.Context, k, v string, _ time.Duration) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.m[k]; ok {
		return false, nil
	}
	i.m[k] = v
	return true, nil
}

func (i *memIdem) Get(_ context.Context, k string) (string, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	v, ok := i.m[k]
	return v, ok, nil
}

func (i *memIdem) Replace(_ context.Context, k, expect, v string, _ time.Duration) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.m[k] != expect {
		return false, nil
	}
	i.m[k] = v
	return true, nil
}

func (i *memIdem) DeleteIf(_ context.Context, k, expect string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.m[k] != expect {
		return false, nil
	}
	delete(i.m, k)
	return true, nil
}

type noopLease struct{}

func (noopLease) Renew(context.Context) error   { return nil }
func (noopLease) Release(context.Context) error { return nil }

type memLocks struct{ held bool }

func (l memLocks) AcquireLease(context.Context, string, time.Duration) (agent.Lease, error) {
	if l.held {
		return nil, repository.ErrLockHeld
	}
	return noopLease{}, nil
}

type countingResumer struct {
	mu   sync.Mutex
	runs []uint64
}

func (r *countingResumer) Resume(_ context.Context, _ model.Scope, runID uint64) (agent.Answer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runID)
	return agent.Answer{RunID: runID}, nil
}

var decideNow = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

const (
	initiator  = 1001
	approverA  = 1002
	approverB  = 1003
	testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type decideFixture struct {
	store   *memDecisions
	orders  *memTransitions
	events  *fakeEvents
	idem    *memIdem
	resumer *countingResumer
	svc     *DecisionService
	keys    rediskey.Keys
}

// newDecideFixture 建一张 OPEN 票（HIGH 或 LOW），run INTERRUPTED(WAIT_APPROVAL)。
func newDecideFixture(t *testing.T, risk model.RiskLevel, expiresIn time.Duration, lockHeld bool) *decideFixture {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	exp := decideNow.Add(expiresIn)
	confirm := 0
	if risk == model.RiskHigh {
		confirm = 1
	}
	f := &decideFixture{
		store: &memDecisions{runStatus: model.RunInterrupted, ticket: repository.TicketRow{ID: 7, TenantID: 1, FarmID: 1,
			RunID: 5, OrderID: 3, TicketSeq: 1, RiskLevel: string(risk), RequiredRole: `["agronomist"]`,
			ConfirmRequired: confirm, InitiatorID: initiator, SnapshotDigest: testDigest,
			Status: string(model.TicketOpen), ExpiresAt: &exp}},
		orders: &memTransitions{}, events: &fakeEvents{}, idem: &memIdem{m: map[string]string{}},
		resumer: &countingResumer{}, keys: keys,
	}
	f.svc = NewDecisionService(DecisionDeps{Store: f.store, Orders: f.orders, Events: f.events, Idem: f.idem,
		Locks: memLocks{held: lockHeld}, Keys: keys, Resumer: f.resumer, Clock: fixedClock{decideNow},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return f
}

func (f *decideFixture) decide(user uint64, rid, decision string) (DecideResult, error) {
	scope := model.Scope{TenantID: 1, FarmID: 1, UserID: user, Roles: []model.Role{model.RoleAgronomist}}
	res, err := f.svc.Decide(context.Background(), scope, DecideInput{TicketID: 7, RequestID: rid, Decision: decision,
		SnapshotDigest: testDigest})
	f.svc.Wait()
	return res, err
}

// HIGH 票：第一审批人只记录、同一人再次通过 SAME_APPROVER、第二人 CONFIRM 生效（outbox + deadline + Resume）；发起人自审 ROLE_INSUFFICIENT。
func TestDecide_HighTwoPerson(t *testing.T) {
	f := newDecideFixture(t, model.RiskHigh, time.Hour, false)
	if _, err := f.decide(initiator, "r0", DecisionApprove); !errors.Is(err, ErrRoleInsufficient) {
		t.Fatalf("initiator self-approve err = %v, want ROLE_INSUFFICIENT", err)
	}
	res, err := f.decide(approverA, "r1", DecisionApprove)
	if err != nil || !res.AwaitingConfirm || res.Status != model.TicketOpen {
		t.Fatalf("first approve = %+v, %v; want awaiting_confirm OPEN", res, err)
	}
	if _, err := f.decide(approverA, "r2", DecisionApprove); !errors.Is(err, ErrSameApprover) {
		t.Fatalf("same approver err = %v, want SAME_APPROVER", err)
	}
	res, err = f.decide(approverB, "r3", DecisionApprove)
	if err != nil || res.Status != model.TicketApproved {
		t.Fatalf("confirm = %+v, %v; want APPROVED", res, err)
	}
	if f.store.outbox != 1 || f.store.deadline == nil || len(f.orders.to) != 1 || f.orders.to[0] != model.OrderApproved {
		t.Errorf("outbox=%d deadline=%v transitions=%v", f.store.outbox, f.store.deadline, f.orders.to)
	}
	if got := f.store.decisions[len(f.store.decisions)-1].Decision; got != decisionConfirm {
		t.Errorf("second decision = %s, want CONFIRM", got)
	}
	if len(f.resumer.runs) != 1 || f.resumer.runs[0] != 5 {
		t.Errorf("resumed runs = %v, want [5]", f.resumer.runs)
	}
	if strings.Join(f.store.audits, ",") != "ROLE_INSUFFICIENT,SAME_APPROVER" {
		t.Errorf("audits = %v", f.store.audits)
	}
	// 同一 request_id 重放返回首次结果，不再执行
	again, err := f.decide(approverB, "r3", DecisionApprove)
	if err != nil || again.DecisionID != res.DecisionID || f.store.outbox != 1 {
		t.Errorf("replay = %+v, %v; outbox=%d", again, err, f.store.outbox)
	}
	// 同一 request_id 换审批人或决策取值：REQUEST_ID_CONFLICT，不静默回放
	if _, err := f.decide(approverA, "r3", DecisionApprove); !errors.Is(err, ErrRequestConflict) {
		t.Errorf("reuse request id by other approver: err = %v, want REQUEST_ID_CONFLICT", err)
	}
	if _, err := f.decide(approverB, "r3", DecisionReject); !errors.Is(err, ErrRequestConflict) {
		t.Errorf("reuse request id with other decision: err = %v, want REQUEST_ID_CONFLICT", err)
	}
}

// 驳回：单事务票据 REJECTED、作业单 REJECTED、run COMPLETED(APPROVAL_REJECTED)、episodic REJECTED、APPROVAL_DECIDED；不 Resume。
func TestDecide_Reject(t *testing.T) {
	f := newDecideFixture(t, model.RiskLow, time.Hour, false)
	res, err := f.decide(approverA, "r1", DecisionReject)
	if err != nil || res.Status != model.TicketRejected {
		t.Fatalf("reject = %+v, %v", res, err)
	}
	if f.store.runStatus != model.RunCompleted || f.store.runReason != model.ReasonApprovalRejected || f.store.memories != 1 {
		t.Errorf("run=%s/%s memories=%d", f.store.runStatus, f.store.runReason, f.store.memories)
	}
	if len(f.events.events) != 1 || f.events.events[0].Type != model.EventApprovalDecided || len(f.resumer.runs) != 0 {
		t.Errorf("events=%v resumed=%v", f.events.events, f.resumer.runs)
	}
}

// 拒绝与失败路径：错误码、审计、事务回滚与 Redis 占位删除。
func TestDecide_Refusals(t *testing.T) {
	tests := []struct {
		name      string
		expiresIn time.Duration
		mutate    func(*decideFixture)
		want      error
		wantAudit string
		wantKey   bool // 失败后 idem 键是否仍在
	}{
		{name: "expired at decision instant", expiresIn: 0, want: ErrTicketExpired, wantAudit: "TICKET_EXPIRED"},
		{name: "closed", expiresIn: time.Hour, mutate: func(f *decideFixture) { f.store.ticket.Status = string(model.TicketApproved) },
			want: ErrTicketClosed, wantAudit: "TICKET_CLOSED"},
		{name: "stale digest", expiresIn: time.Hour, mutate: func(f *decideFixture) { f.store.ticket.SnapshotDigest = strings.Repeat("b", 64) },
			want: ErrSnapshotStale, wantAudit: "SNAPSHOT_STALE"},
		{name: "in flight", expiresIn: time.Hour, mutate: func(f *decideFixture) { f.idem.m[f.keys.IdemApprove("r1")] = "PENDING:x" },
			want: ErrRequestInFlight, wantKey: true},
		{name: "outbox failure rolls back", expiresIn: time.Hour, mutate: func(f *decideFixture) { f.store.failOn = "outbox" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDecideFixture(t, model.RiskLow, tt.expiresIn, false)
			if tt.mutate != nil {
				tt.mutate(f)
			}
			_, err := f.decide(approverA, "r1", DecisionApprove)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if got := strings.Join(f.store.audits, ","); got != tt.wantAudit {
				t.Errorf("audits = %q, want %q", got, tt.wantAudit)
			}
			if _, ok := f.idem.m[f.keys.IdemApprove("r1")]; ok != tt.wantKey {
				t.Errorf("idem key present = %v, want %v", ok, tt.wantKey)
			}
			if tt.name == "outbox failure rolls back" && (f.store.ticket.Status != string(model.TicketOpen) ||
				len(f.store.decisions) != 0 || f.store.outbox != 0 || len(f.resumer.runs) != 0) {
				t.Errorf("not rolled back: ticket=%s decisions=%d outbox=%d", f.store.ticket.Status, len(f.store.decisions), f.store.outbox)
			}
		})
	}
}

// 到期瞬间审批：同事务票据与作业单 EXPIRED、run COMPLETED(APPROVAL_EXPIRED)，三者一致。
func TestDecide_ExpiredClosesAll(t *testing.T) {
	f := newDecideFixture(t, model.RiskLow, 0, false)
	if _, err := f.decide(approverA, "r1", DecisionApprove); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("err = %v", err)
	}
	if f.store.ticket.Status != string(model.TicketExpired) || len(f.orders.to) != 1 || f.orders.to[0] != model.OrderExpired ||
		f.store.runStatus != model.RunCompleted || f.store.runReason != model.ReasonApprovalExpired {
		t.Errorf("ticket=%s orders=%v run=%s/%s", f.store.ticket.Status, f.orders.to, f.store.runStatus, f.store.runReason)
	}
	// 扫描再次收尾：票据已非 OPEN，跳过
	if ok, err := f.svc.ExpireTicket(context.Background(), f.store.ticket); err != nil || ok {
		t.Errorf("second expire = %v, %v; want skipped", ok, err)
	}
}

func TestDecide_BadInputAndBusy(t *testing.T) {
	f := newDecideFixture(t, model.RiskLow, time.Hour, false)
	scope := model.Scope{TenantID: 1, FarmID: 1, UserID: approverA}
	for _, in := range []DecideInput{
		{RequestID: "r", Decision: DecisionApprove, SnapshotDigest: testDigest},
		{TicketID: 7, Decision: DecisionApprove, SnapshotDigest: testDigest},
		{TicketID: 7, RequestID: "r", Decision: "CONFIRM", SnapshotDigest: testDigest},
		{TicketID: 7, RequestID: "r", Decision: DecisionApprove, SnapshotDigest: "short"},
	} {
		if _, err := f.svc.Decide(context.Background(), scope, in); !errors.Is(err, ErrBadInput) {
			t.Errorf("Decide(%+v) err = %v, want ErrBadInput", in, err)
		}
	}
	busy := newDecideFixture(t, model.RiskLow, time.Hour, true)
	if _, err := busy.decide(approverA, "r1", DecisionApprove); !errors.Is(err, ErrOrderBusy) {
		t.Errorf("busy err = %v, want ErrOrderBusy", err)
	}
	if _, ok := busy.idem.m[busy.keys.IdemApprove("r1")]; ok {
		t.Error("idem key kept after lock failure")
	}
}

// 待审批列表：一次批量读作业单（无 N+1），HIGH 票第一人通过后 awaiting_confirm，LOW 票恒 false；读单失败整体报错。
func TestPending_Enrich(t *testing.T) {
	first := uint64(approverA)
	exp := decideNow.Add(time.Hour)
	rows := []repository.PendingRow{
		{ID: 1, OrderID: 10, RiskLevel: string(model.RiskHigh), ConfirmRequired: 1, FirstApproverID: &first, ExpiresAt: &exp},
		{ID: 2, OrderID: 11, RiskLevel: string(model.RiskHigh), ConfirmRequired: 1},
		{ID: 3, OrderID: 12, RiskLevel: string(model.RiskLow), FirstApproverID: &first},
	}
	orders := map[uint64]model.WorkOrder{
		10: {OrderID: 10, ConstraintChecks: []model.ConstraintCheck{{Phase: model.PhasePreApproval, RuleID: "wind", Passed: true}}},
		11: {OrderID: 11}, 12: {OrderID: 12},
	}
	tests := []struct {
		name     string
		failOn   string
		wantErr  bool
		awaiting []bool
	}{
		{name: "enriched", awaiting: []bool{true, false, false}},
		{name: "orders read fails", failOn: "orders", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDecideFixture(t, model.RiskLow, time.Hour, false)
			f.store.pending, f.store.orders, f.store.failOn = rows, orders, tt.failOn
			got, err := f.svc.Pending(context.Background(), model.Scope{TenantID: 1, FarmID: 1})
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.store.orderCalls) != 1 || len(f.store.orderCalls[0]) != len(rows) {
				t.Fatalf("order reads = %v, want one batch of %d", f.store.orderCalls, len(rows))
			}
			for i, r := range got {
				if r.AwaitingConfirm != tt.awaiting[i] {
					t.Errorf("ticket %d awaiting_confirm = %t, want %t", r.ID, r.AwaitingConfirm, tt.awaiting[i])
				}
				if r.WorkOrder == nil || r.WorkOrder.OrderID != r.OrderID {
					t.Fatalf("ticket %d work_order = %+v", r.ID, r.WorkOrder)
				}
			}
			if w := got[0].WorkOrder; len(w.ConstraintChecks) != 1 || w.ApprovalExpiresAt == nil || !w.ApprovalExpiresAt.Equal(exp) {
				t.Errorf("ticket 1 work_order = %+v", w)
			}
		})
	}
}
