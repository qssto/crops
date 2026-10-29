package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// fakeTx 直接以 nil 句柄执行 fn（fake 仓储不看句柄），记录调用次数与 fn 的返回。
type fakeTx struct {
	calls int
	err   error
}

func (f *fakeTx) InTx(_ context.Context, fn func(*repository.DB) error) error {
	f.calls++
	f.err = fn(nil)
	return f.err
}

// fakeTickets 是内存票据仓储：existing 为事务外读到的票，dupInTx 模拟事务内命中 uk_order_seq，
// raceWinner 是命中后重读到的库中票；byRunAfter 是 ActivateTx 之后（并发激活已提交）重读到的票。
type fakeTickets struct {
	existing   *model.ApprovalTicket
	raceWinner *model.ApprovalTicket
	byRun      *model.ApprovalTicket
	byRunAfter *model.ApprovalTicket
	readErr    error
	dupInTx    bool
	insertErr  error
	activated  int64
	activeErr  error
	open       []repository.PendingTicket

	checks       []model.ConstraintCheck
	checkAttempt int
	inserted     []model.ApprovalTicket
	expires      time.Time
	activations  int
}

func (f *fakeTickets) TicketByOrderSeq(context.Context, *repository.DB, model.OrderRef, int) (model.ApprovalTicket, bool, error) {
	if len(f.inserted) > 0 && f.raceWinner != nil {
		return *f.raceWinner, true, f.readErr
	}
	if f.existing == nil {
		return model.ApprovalTicket{}, false, f.readErr
	}
	return *f.existing, true, f.readErr
}

func (f *fakeTickets) TicketByRun(context.Context, *repository.DB, model.Scope, uint64) (model.ApprovalTicket, bool, error) {
	if f.activations > 0 && f.byRunAfter != nil {
		return *f.byRunAfter, true, f.readErr
	}
	if f.byRun == nil {
		return model.ApprovalTicket{}, false, f.readErr
	}
	return *f.byRun, true, f.readErr
}

func (f *fakeTickets) InsertChecksTx(_ context.Context, _ *repository.DB, _ model.OrderRef, attempt int, cs []model.ConstraintCheck, _ time.Time) error {
	f.checkAttempt = attempt
	f.checks = append(f.checks, cs...)
	return nil
}

func (f *fakeTickets) InsertTicketTx(_ context.Context, _ *repository.DB, t model.ApprovalTicket, _ time.Time) (uint64, bool, error) {
	if f.insertErr != nil {
		return 0, false, f.insertErr
	}
	f.inserted = append(f.inserted, t)
	return 77, !f.dupInTx, nil
}

func (f *fakeTickets) ActivateTx(_ context.Context, _ *repository.DB, _ model.Scope, _ uint64, expiresAt, _ time.Time) (int64, error) {
	f.expires = expiresAt
	f.activations++
	return f.activated, f.activeErr
}

func (f *fakeTickets) ListOpen(context.Context, *repository.DB, model.Scope) ([]repository.PendingTicket, error) {
	return f.open, f.readErr
}

type fakeRaw struct {
	puts []model.RawPut
	err  error
}

func (f *fakeRaw) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	if f.err != nil {
		return model.RawObject{}, f.err
	}
	f.puts = append(f.puts, p)
	return model.RawObject{ID: 501}, nil
}

type fakeRisk struct {
	err error
	ttl time.Duration
}

func (f fakeRisk) For(model.WorkType) (policy.Risk, error) {
	return policy.Risk{Level: model.RiskHigh, Roles: []model.Role{model.RoleAgronomist}, ConfirmRequired: true}, f.err
}

func (f fakeRisk) TTL(model.RiskLevel) (time.Duration, error) { return f.ttl, f.err }

type approvalFixture struct {
	svc     *ApprovalService
	tx      *fakeTx
	tickets *fakeTickets
	orders  *fakeOrders
	events  *fakeEvents
	raw     *fakeRaw
}

var approvalNow = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

func newApprovalFixture(t *testing.T, tickets *fakeTickets, orderStatus model.WorkOrderStatus, risk fakeRisk) approvalFixture {
	t.Helper()
	f := approvalFixture{tx: &fakeTx{}, tickets: tickets, events: &fakeEvents{}, raw: &fakeRaw{},
		orders: &fakeOrders{state: model.OrderState{Status: orderStatus, RowVer: 3, RunID: 5, WorkType: model.WorkSpray}}}
	clk := fixedClock{approvalNow}
	svc, err := NewApprovalService(ApprovalDeps{Clock: clk, DB: &repository.DB{}, Tx: f.tx, Tickets: tickets,
		Orders: f.orders, WorkOrders: NewWorkOrderService(clk, f.orders, f.events), Events: f.events, Raw: f.raw, Risk: risk})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

var approvalScope = model.Scope{TenantID: 1, FarmID: 2, UserID: 1002}

func openRequest() agent.ApprovalRequest {
	return agent.ApprovalRequest{InitiatorID: 1002,
		Order: model.WorkOrder{OrderID: 9, AdviceID: 4, RunID: 5, TenantID: 1, FarmID: 2, WorkType: model.WorkSpray,
			Status: model.OrderDraft, ConstraintChecks: []model.ConstraintCheck{{Phase: model.PhasePreApproval, RuleID: "kb#t1", Passed: true}}},
		Card: model.AdviceCard{AdviceID: 4, RunID: 5, TenantID: 1, FarmID: 2}}
}

func eventTypes(es []model.RunEvent) []model.EventType {
	out := make([]model.EventType, len(es))
	for i, e := range es {
		out[i] = e.Type
	}
	return out
}

// 新票：快照 Put + digest，单事务写约束校验（attempt=1）、PREPARING 票、DRAFT→PENDING_APPROVAL、APPROVAL_OPEN。
func TestApprovalOpen_NewTicket(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
	got, err := f.svc.Open(context.Background(), approvalScope, openRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got.TicketID != 77 || got.Status != model.TicketPreparing || got.TicketSeq != 1 || got.RiskLevel != model.RiskHigh ||
		!got.ConfirmRequired || len(got.RequiredRole) != 1 || got.InitiatorID != 1002 || got.TicketKind != model.TicketKindOrder {
		t.Errorf("ticket %+v", got)
	}
	if len(f.raw.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(f.raw.puts))
	}
	p := f.raw.puts[0]
	sum := sha256.Sum256(p.Body)
	digest := hex.EncodeToString(sum[:])
	if p.Kind != model.RawSnapshot || p.Name != "ticket-9-1-"+digest[:16] || p.RunID != 5 || !p.At.Equal(approvalNow) ||
		got.Snapshot.Digest != digest || got.Snapshot.RawObjectID != 501 {
		t.Errorf("snapshot put %+v digest %s", p, got.Snapshot.Digest)
	}
	var snap approvalSnapshot
	if err := json.Unmarshal(p.Body, &snap); err != nil || snap.Order.OrderID != 9 || snap.Card.AdviceID != 4 || snap.RiskLevel != model.RiskHigh {
		t.Errorf("snapshot body %s (%v)", p.Body, err)
	}
	if f.tx.calls != 1 || f.tickets.checkAttempt != 1 || len(f.tickets.checks) != 1 || len(f.tickets.inserted) != 1 {
		t.Errorf("tx=%d attempt=%d checks=%d inserted=%d", f.tx.calls, f.tickets.checkAttempt, len(f.tickets.checks), len(f.tickets.inserted))
	}
	if !f.orders.updated || len(f.orders.transitions) != 1 || f.orders.transitions[0].To != model.OrderPendingApproval {
		t.Errorf("transition updated=%v %+v", f.orders.updated, f.orders.transitions)
	}
	types := eventTypes(f.events.events)
	if len(types) != 2 || types[0] != model.EventOrderTransition || types[1] != model.EventApprovalOpen {
		t.Fatalf("events %v", types)
	}
	var op openPayload
	if err := json.Unmarshal(f.events.events[1].Payload, &op); err != nil || op.TicketID != 77 || op.OrderID != 9 || op.TicketSeq != 1 {
		t.Errorf("APPROVAL_OPEN payload %s (%v)", f.events.events[1].Payload, err)
	}
}

// 已有票（整图重跑）：直接返回，不重写快照、不开事务。
func TestApprovalOpen_ExistingTicketShortCircuits(t *testing.T) {
	prev := model.ApprovalTicket{TicketID: 33, Status: model.TicketPreparing}
	f := newApprovalFixture(t, &fakeTickets{existing: &prev}, model.OrderPendingApproval, fakeRisk{})
	got, err := f.svc.Open(context.Background(), approvalScope, openRequest())
	if err != nil || got.TicketID != 33 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(f.raw.puts) != 0 || f.tx.calls != 0 || len(f.events.events) != 0 {
		t.Errorf("writes on rerun: puts=%d tx=%d events=%d", len(f.raw.puts), f.tx.calls, len(f.events.events))
	}
}

// 事务内命中 uk_order_seq（并发开票先提交）：不再迁移、不再发事件，返回库中先提交者的票（快照 digest 以它为准）。
func TestApprovalOpen_DuplicateInTxReturnsStoredTicket(t *testing.T) {
	winner := model.ApprovalTicket{TicketID: 77, Status: model.TicketPreparing, RiskLevel: model.RiskHigh,
		Snapshot: model.TicketSnapshot{Digest: "winner-digest", RawObjectID: 400}}
	f := newApprovalFixture(t, &fakeTickets{dupInTx: true, raceWinner: &winner}, model.OrderDraft, fakeRisk{})
	got, err := f.svc.Open(context.Background(), approvalScope, openRequest())
	if err != nil || got.TicketID != 77 || got.Snapshot != winner.Snapshot {
		t.Fatalf("got %+v, %v; want stored ticket %+v", got, err, winner)
	}
	if f.orders.updated || len(f.events.events) != 0 {
		t.Errorf("duplicate wrote transition=%v events=%v", f.orders.updated, eventTypes(f.events.events))
	}
}

// 事务内命中 uk_order_seq 但读不回票：报错而不是返回本地拼的票。
func TestApprovalOpen_DuplicateInTxWithoutStoredTicketFails(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{dupInTx: true}, model.OrderDraft, fakeRisk{})
	if _, err := f.svc.Open(context.Background(), approvalScope, openRequest()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// 内容不同的快照落到不同对象（并发开票不互相覆盖）；内容相同则同名（重跑写同一对象）。
func TestApprovalOpen_SnapshotNameFollowsContent(t *testing.T) {
	names := func(req agent.ApprovalRequest) string {
		f := newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
		if _, err := f.svc.Open(context.Background(), approvalScope, req); err != nil {
			t.Fatal(err)
		}
		return f.raw.puts[0].Name
	}
	other := openRequest()
	other.Card.Conclusion = "different advice text"
	a, b, c := names(openRequest()), names(openRequest()), names(other)
	if a != b || a == c {
		t.Errorf("names same=%q/%q different=%q", a, b, c)
	}
}

// 作业单已是 PENDING_APPROVAL：跳过迁移，只发 APPROVAL_OPEN。
func TestApprovalOpen_AlreadyPendingSkipsTransition(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderPendingApproval, fakeRisk{})
	if _, err := f.svc.Open(context.Background(), approvalScope, openRequest()); err != nil {
		t.Fatal(err)
	}
	if types := eventTypes(f.events.events); f.orders.updated || len(types) != 1 || types[0] != model.EventApprovalOpen {
		t.Errorf("updated=%v events=%v", f.orders.updated, types)
	}
}

func TestApprovalOpen_Errors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name     string
		mutate   func(*agent.ApprovalRequest)
		tickets  fakeTickets
		status   model.WorkOrderStatus
		risk     fakeRisk
		rawErr   error
		orderErr error
		want     error
		wantPuts int
		wantTx   int
	}{
		{name: "scope mismatch", mutate: func(r *agent.ApprovalRequest) { r.Order.FarmID = 99 }, want: ErrBadApproval},
		{name: "zero order id", mutate: func(r *agent.ApprovalRequest) { r.Order.OrderID = 0 }, want: ErrBadApproval},
		{name: "zero initiator", mutate: func(r *agent.ApprovalRequest) { r.InitiatorID = 0 }, want: ErrBadApproval},
		{name: "card of another order", mutate: func(r *agent.ApprovalRequest) { r.Card.AdviceID = 8 }, want: ErrBadApproval},
		// 定级与归属以库中作业单为准：请求里的 run_id / work_type 与库不符直接拒绝，不写快照。
		{name: "run of another order", mutate: func(r *agent.ApprovalRequest) { r.Order.RunID, r.Card.RunID = 6, 6 }, want: ErrBadApproval},
		{name: "work type differs from db", mutate: func(r *agent.ApprovalRequest) { r.Order.WorkType = model.WorkIrrigate }, want: ErrBadApproval},
		{name: "order read error", orderErr: boom, want: boom},
		{name: "read error", tickets: fakeTickets{readErr: boom}, want: boom},
		{name: "no risk", risk: fakeRisk{err: policy.ErrNoRisk}, want: policy.ErrNoRisk},
		{name: "put error", rawErr: boom, want: boom},
		{name: "insert error", tickets: fakeTickets{insertErr: boom}, want: boom, wantPuts: 1, wantTx: 1},
		{name: "illegal transition", status: model.OrderExecuting, want: model.ErrIllegalTransition, wantPuts: 1, wantTx: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == "" {
				status = model.OrderDraft
			}
			tk := tt.tickets
			f := newApprovalFixture(t, &tk, status, tt.risk)
			f.raw.err = tt.rawErr
			f.orders.getErr = tt.orderErr
			req := openRequest()
			if tt.mutate != nil {
				tt.mutate(&req)
			}
			_, err := f.svc.Open(context.Background(), approvalScope, req)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if len(f.raw.puts) != tt.wantPuts || f.tx.calls != tt.wantTx {
				t.Errorf("puts=%d tx=%d, want %d/%d", len(f.raw.puts), f.tx.calls, tt.wantPuts, tt.wantTx)
			}
			if f.tx.calls > 0 && f.tx.err == nil {
				t.Errorf("tx committed despite error")
			}
		})
	}
}

// PREPARING → OPEN：expires_at = now + approval_ttl，同事务发 APPROVAL_PENDING。
func TestApprovalActivate_Preparing(t *testing.T) {
	tk := model.ApprovalTicket{TicketID: 77, RunID: 5, TenantID: 1, FarmID: 2, OrderID: 9, RiskLevel: model.RiskHigh, Status: model.TicketPreparing}
	f := newApprovalFixture(t, &fakeTickets{byRun: &tk, activated: 1}, model.OrderPendingApproval, fakeRisk{ttl: 2 * time.Hour})
	got, err := f.svc.Activate(context.Background(), approvalScope, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := approvalNow.Add(2 * time.Hour)
	if got.Status != model.TicketOpen || got.ExpiresAt == nil || !got.ExpiresAt.Equal(want) || !f.tickets.expires.Equal(want) {
		t.Errorf("ticket %+v, store expires %v", got, f.tickets.expires)
	}
	if types := eventTypes(f.events.events); len(types) != 1 || types[0] != model.EventApprovalPending {
		t.Fatalf("events %v", types)
	}
	var pp pendingPayload
	if err := json.Unmarshal(f.events.events[0].Payload, &pp); err != nil || pp.TicketID != 77 || !pp.ExpiresAt.Equal(want) {
		t.Errorf("APPROVAL_PENDING payload %s (%v)", f.events.events[0].Payload, err)
	}
}

func TestApprovalActivate_Cases(t *testing.T) {
	ticket := func(st model.TicketStatus) *model.ApprovalTicket {
		return &model.ApprovalTicket{TicketID: 77, RunID: 5, Status: st, RiskLevel: model.RiskHigh}
	}
	tests := []struct {
		name    string
		runID   uint64
		tickets fakeTickets
		want    error
		wantTx  int
	}{
		{name: "already open is idempotent", runID: 5, tickets: fakeTickets{byRun: ticket(model.TicketOpen)}},
		{name: "zero run id", runID: 0, want: ErrBadApproval},
		{name: "no ticket", runID: 5, want: agent.ErrNotActivatable},
		{name: "decided ticket", runID: 5, tickets: fakeTickets{byRun: ticket(model.TicketApproved)}, want: agent.ErrNotActivatable},
		{name: "run not interrupted", runID: 5, tickets: fakeTickets{byRun: ticket(model.TicketPreparing)}, want: agent.ErrNotActivatable, wantTx: 1},
		// UPDATE 命中 0 行后重读：并发 Activate 已置 OPEN 则幂等返回，票被并发决策则不可激活。
		{name: "concurrent activate wins", runID: 5, wantTx: 1,
			tickets: fakeTickets{byRun: ticket(model.TicketPreparing), byRunAfter: ticket(model.TicketOpen)}},
		{name: "decided concurrently", runID: 5, want: agent.ErrNotActivatable, wantTx: 1,
			tickets: fakeTickets{byRun: ticket(model.TicketPreparing), byRunAfter: ticket(model.TicketExpired)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk := tt.tickets
			f := newApprovalFixture(t, &tk, model.OrderPendingApproval, fakeRisk{ttl: time.Hour})
			got, err := f.svc.Activate(context.Background(), approvalScope, tt.runID)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && got.Status != model.TicketOpen {
				t.Errorf("status %s, want OPEN", got.Status)
			}
			if f.tx.calls != tt.wantTx || len(f.events.events) != 0 {
				t.Errorf("tx=%d events=%v, want tx %d and no events", f.tx.calls, eventTypes(f.events.events), tt.wantTx)
			}
		})
	}
}

func TestApprovalListPending(t *testing.T) {
	rows := []repository.PendingTicket{{TicketID: 1, OrderID: 9, RiskLevel: model.RiskHigh}}
	f := newApprovalFixture(t, &fakeTickets{open: rows}, model.OrderDraft, fakeRisk{})
	got, err := f.svc.ListPending(context.Background(), approvalScope)
	if err != nil || len(got) != 1 || got[0].TicketID != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	f.tickets.readErr = errors.New("boom")
	if _, err := f.svc.ListPending(context.Background(), approvalScope); err == nil {
		t.Error("list error swallowed")
	}
}

func TestNewApprovalService_MissingDeps(t *testing.T) {
	if _, err := NewApprovalService(ApprovalDeps{}); err == nil {
		t.Fatal("want error for missing deps")
	}
}
