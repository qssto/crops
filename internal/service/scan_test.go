package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// memScan 同时实现 TicketScanStore 与 RunScanStore；记录各写操作。
type memScan struct {
	mu        sync.Mutex
	expired   []repository.TicketRow
	overdue   []repository.OrderKey
	preparing []repository.StaleTicket
	approved  []repository.RunKey
	stuck     []repository.RunKey
	sysErr    []repository.RunKey
	drafts    map[uint64][]uint64

	voided   []uint64
	marked   []uint64
	failed   []uint64
	lastFrom map[string]time.Time
}

func (m *memScan) InTx(_ context.Context, fn func(tx *repository.DB) error) error { return fn(nil) }

func (m *memScan) ListExpired(context.Context, time.Time) ([]repository.TicketRow, error) {
	return m.expired, nil
}

func (m *memScan) ListDispatchOverdue(context.Context, time.Time) ([]repository.OrderKey, error) {
	return m.overdue, nil
}

func (m *memScan) ListPreparing(_ context.Context, before time.Time) ([]repository.StaleTicket, error) {
	m.lastFrom["preparing"] = before
	return m.preparing, nil
}

func (m *memScan) VoidPreparingTx(_ context.Context, _ *repository.DB, t repository.TicketRow, _ time.Time) (bool, error) {
	m.voided = append(m.voided, t.ID)
	return true, nil
}

func (m *memScan) ListApprovedInterrupted(_ context.Context, before time.Time) ([]repository.RunKey, error) {
	m.lastFrom["finalize"] = before
	return m.approved, nil
}

func (m *memScan) ListStuck(_ context.Context, before time.Time) ([]repository.RunKey, error) {
	m.lastFrom["stuck"] = before
	return m.stuck, nil
}

func (m *memScan) MarkSystemError(_ context.Context, k repository.RunKey, _ time.Time) (bool, error) {
	m.marked = append(m.marked, k.ID)
	return true, nil
}

func (m *memScan) ListSystemError(context.Context) ([]repository.RunKey, error) { return m.sysErr, nil }

func (m *memScan) FailInterruptedTx(_ context.Context, _ *repository.DB, k repository.RunKey, _ model.InterruptType, _ model.ReasonCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, k.ID)
	return nil
}

func (m *memScan) DraftOrdersTx(_ context.Context, _ *repository.DB, _ model.Scope, runID uint64) ([]uint64, error) {
	return m.drafts[runID], nil
}

// scanExpirer 记录收尾的票据；busy 中的票返回 ErrOrderBusy。
type scanExpirer struct {
	got  []uint64
	busy map[uint64]bool
}

func (e *scanExpirer) ExpireTicket(_ context.Context, t repository.TicketRow) (bool, error) {
	if e.busy[t.ID] {
		return false, fmt.Errorf("lock: %w", ErrOrderBusy)
	}
	e.got = append(e.got, t.ID)
	return true, nil
}

type scanGate struct{ runs []uint64 }

func (g *scanGate) Activate(_ context.Context, _ model.Scope, runID uint64) (model.ApprovalTicket, error) {
	g.runs = append(g.runs, runID)
	return model.ApprovalTicket{RunID: runID, Status: model.TicketOpen}, nil
}

// probeLocks 的 held 键视为已被持有（AcquireLease 失败、Held 为 true）。
type probeLocks struct{ held map[string]bool }

func (l probeLocks) AcquireLease(_ context.Context, key string, _ time.Duration) (agent.Lease, error) {
	if l.held[key] {
		return nil, repository.ErrLockHeld
	}
	return noopLease{}, nil
}

func (l probeLocks) Held(_ context.Context, key string) (bool, error) { return l.held[key], nil }

// scanResumer 记录续跑的 run；notResumable 中的 run 返回 agent.ErrNotResumable。
type scanResumer struct {
	mu           sync.Mutex
	runs         []uint64
	notResumable map[uint64]bool
}

func (r *scanResumer) Resume(_ context.Context, _ model.Scope, runID uint64) (agent.Answer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runID)
	if r.notResumable[runID] {
		return agent.Answer{}, fmt.Errorf("graph: no checkpoint: %w", agent.ErrNotResumable)
	}
	return agent.Answer{RunID: runID}, nil
}

// scanOrders 记录作业单迁移；illegal 中的单返回 ErrIllegalTransition。
type scanOrders struct {
	mu      sync.Mutex
	got     []string
	illegal map[uint64]bool
}

func (o *scanOrders) Transition(_ context.Context, _ *repository.DB, ref model.OrderRef, to model.WorkOrderStatus,
	_ model.Actor, reason model.ReasonCode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.illegal[ref.OrderID] {
		return fmt.Errorf("order %d: %w", ref.OrderID, model.ErrIllegalTransition)
	}
	o.got = append(o.got, fmt.Sprintf("%d:%s:%s", ref.OrderID, to, reason))
	return nil
}

func (o *scanOrders) TransitionFrom(ctx context.Context, tx *repository.DB, ref model.OrderRef, _, to model.WorkOrderStatus,
	actor model.Actor, reason model.ReasonCode) error {
	return o.Transition(ctx, tx, ref, to, actor, reason)
}

type scanFixture struct {
	store   *memScan
	expirer *scanExpirer
	gate    *scanGate
	resumer *scanResumer
	orders  *scanOrders
	keys    rediskey.Keys
	held    map[string]bool
	svc     *Scanner
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := &scanFixture{store: &memScan{drafts: map[uint64][]uint64{}, lastFrom: map[string]time.Time{}},
		expirer: &scanExpirer{busy: map[uint64]bool{}}, gate: &scanGate{},
		resumer: &scanResumer{notResumable: map[uint64]bool{}}, orders: &scanOrders{illegal: map[uint64]bool{}},
		keys: keys, held: map[string]bool{}}
	f.svc = NewScanner(ScanDeps{Tickets: f.store, Runs: f.store, Expirer: f.expirer, Orders: f.orders, Gate: f.gate,
		Resumer: f.resumer, Locks: probeLocks{held: f.held}, Keys: keys, Clock: fixedClock{decideNow},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return f
}

func (f *scanFixture) run(t *testing.T) {
	t.Helper()
	if err := f.svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	f.svc.Wait()
}

func ptr[T any](v T) *T { return &v }

func ticket(id, orderID, runID uint64) repository.TicketRow {
	return repository.TicketRow{ID: id, TenantID: 1, FarmID: 1, OrderID: orderID, RunID: runID}
}

// approval_expire：过期票逐张收尾（被占用的跳过、不报错），超下发时限的 APPROVED 单转 EXPIRED（已下发的跳过）。
func TestScanner_ApprovalExpire(t *testing.T) {
	f := newScanFixture(t)
	f.store.expired = []repository.TicketRow{ticket(1, 11, 21), ticket(2, 12, 22)}
	f.expirer.busy[2] = true
	f.store.overdue = []repository.OrderKey{{ID: 31, TenantID: 1, FarmID: 1}, {ID: 32, TenantID: 1, FarmID: 1}}
	f.orders.illegal[32] = true
	f.run(t)
	if !slices.Equal(f.expirer.got, []uint64{1}) {
		t.Errorf("expired tickets = %v, want [1]", f.expirer.got)
	}
	if !slices.Equal(f.orders.got, []string{"31:EXPIRED:APPROVAL_EXPIRED"}) {
		t.Errorf("transitions = %v", f.orders.got)
	}
}

// ticket_activate：run 已中断待审 → Activate；run 已终态 → 票据作废 + 作业单 EXPIRED；run 仍 RUNNING / SYSTEM_ERROR → 不动。
func TestScanner_TicketActivate(t *testing.T) {
	f := newScanFixture(t)
	f.store.preparing = []repository.StaleTicket{
		{TicketRow: ticket(1, 11, 21), RunStatus: string(model.RunInterrupted), InterruptType: ptr(string(model.InterruptWaitApproval))},
		{TicketRow: ticket(2, 12, 22), RunStatus: string(model.RunFailed)},
		{TicketRow: ticket(3, 13, 23), RunStatus: string(model.RunRunning)},
		{TicketRow: ticket(4, 14, 24), RunStatus: string(model.RunInterrupted), InterruptType: ptr(string(model.InterruptSystemError))},
	}
	f.run(t)
	if !slices.Equal(f.gate.runs, []uint64{21}) || !slices.Equal(f.store.voided, []uint64{2}) {
		t.Errorf("activated=%v voided=%v", f.gate.runs, f.store.voided)
	}
	if !slices.Equal(f.orders.got, []string{"12:EXPIRED:APPROVAL_EXPIRED"}) {
		t.Errorf("transitions = %v", f.orders.got)
	}
	if got := f.store.lastFrom["preparing"]; !got.Equal(decideNow.Add(-preparingGrace)) {
		t.Errorf("preparing before = %v", got)
	}
}

// run_finalize：APPROVED 超过 60s 仍 INTERRUPTED 的 run 重新 Resume。
func TestScanner_RunFinalize(t *testing.T) {
	f := newScanFixture(t)
	f.store.approved = []repository.RunKey{{ID: 21, TenantID: 1, FarmID: 1}, {ID: 22, TenantID: 1, FarmID: 1}}
	f.resumer.notResumable[22] = true // 如 graph_version 已变：收尾为 FAILED，不再每轮重试
	f.run(t)
	if !slices.Equal(slices.Sorted(slices.Values(f.resumer.runs)), []uint64{21, 22}) {
		t.Errorf("resumed = %v, want [21 22]", f.resumer.runs)
	}
	if !slices.Equal(f.store.failed, []uint64{22}) {
		t.Errorf("failed = %v, want [22]", f.store.failed)
	}
	if got := f.store.lastFrom["finalize"]; !got.Equal(decideNow.Add(-finalizeGrace)) {
		t.Errorf("finalize before = %v", got)
	}
}

// stuck_run：持锁的 run 不动；无锁的置 SYSTEM_ERROR；sys_retry ≤ 3 重入，超过 3 或不可续 → FAILED + DRAFT 单 CANCELED。
func TestScanner_StuckRun(t *testing.T) {
	f := newScanFixture(t)
	f.store.stuck = []repository.RunKey{{ID: 1, TenantID: 1, FarmID: 1}, {ID: 2, TenantID: 1, FarmID: 1}}
	f.held[f.keys.LockRun(1)] = true
	f.store.sysErr = []repository.RunKey{
		{ID: 3, TenantID: 1, FarmID: 1, SysRetry: repository.MaxSysRetry},
		{ID: 4, TenantID: 1, FarmID: 1, SysRetry: repository.MaxSysRetry + 1},
		{ID: 5, TenantID: 1, FarmID: 1, SysRetry: 1},
	}
	f.store.drafts[4] = []uint64{41}
	f.resumer.notResumable[5] = true
	f.run(t)
	if !slices.Equal(f.store.marked, []uint64{2}) {
		t.Errorf("marked SYSTEM_ERROR = %v, want [2]", f.store.marked)
	}
	resumed := slices.Sorted(slices.Values(f.resumer.runs))
	if !slices.Equal(resumed, []uint64{3, 5}) {
		t.Errorf("resumed = %v, want [3 5]", resumed)
	}
	failed := slices.Sorted(slices.Values(f.store.failed))
	if !slices.Equal(failed, []uint64{4, 5}) {
		t.Errorf("failed = %v, want [4 5]", failed)
	}
	if !slices.Equal(f.orders.got, []string{"41:CANCELED:RETRY_EXHAUSTED"}) {
		t.Errorf("transitions = %v", f.orders.got)
	}
	if got := f.store.lastFrom["stuck"]; !got.Equal(decideNow.Add(-stuckAfter)) {
		t.Errorf("stuck before = %v", got)
	}
}

// lock:scan:{task} 被其他实例持有时跳过该任务，其余任务照常。
func TestScanner_ScanLockHeld(t *testing.T) {
	f := newScanFixture(t)
	f.store.expired = []repository.TicketRow{ticket(1, 11, 21)}
	f.store.approved = []repository.RunKey{{ID: 21, TenantID: 1, FarmID: 1}}
	f.held[f.keys.LockScan(ScanApprovalExpire)] = true
	f.run(t)
	if len(f.expirer.got) != 0 || !slices.Equal(f.resumer.runs, []uint64{21}) {
		t.Errorf("expired=%v resumed=%v", f.expirer.got, f.resumer.runs)
	}
}

// Run 按周期执行，ctx 取消即退出。
func TestScanner_RunStops(t *testing.T) {
	f := newScanFixture(t)
	f.svc.d.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.svc.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
