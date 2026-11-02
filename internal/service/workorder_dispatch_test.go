package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

var dispatchNow = time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)

// memDispatch 是作业单 + outbox 的内存模型：同时实现 WorkOrderStore（Transition 用）与 DispatchStore；
// InTx 失败时回滚到事务前快照。守卫语义同 repository 的 SQL。
type memDispatch struct {
	order       repository.DispatchOrder
	result      []byte
	outbox      string
	transitions []model.OrderTransition
	checks      []model.ConstraintCheck
	checkEpoch  int
	superseded  [2]uint64
	failOn      string
}

type memDispatchSnap struct {
	order  repository.DispatchOrder
	result []byte
	outbox string
	trans  int
}

func (m *memDispatch) fail(step string) error {
	if m.failOn == step {
		return fmt.Errorf("injected %s failure", step)
	}
	return nil
}

func (m *memDispatch) InTx(_ context.Context, fn func(tx *repository.DB) error) error {
	before := memDispatchSnap{order: m.order, result: m.result, outbox: m.outbox, trans: len(m.transitions)}
	if err := fn(nil); err != nil {
		m.order, m.result, m.outbox, m.transitions = before.order, before.result, before.outbox, m.transitions[:before.trans]
		return err
	}
	return nil
}

func (m *memDispatch) GetState(context.Context, *repository.DB, model.OrderRef) (model.OrderState, error) {
	return model.OrderState{RunID: m.order.RunID, WorkType: model.WorkType(m.order.WorkType),
		Status: model.WorkOrderStatus(m.order.Status), RowVer: m.order.RowVer}, nil
}

func (m *memDispatch) UpdateStatus(_ context.Context, _ *repository.DB, _ model.OrderRef, from, to model.WorkOrderStatus,
	rowVer int, _ time.Time) error {
	if string(from) != m.order.Status || rowVer != m.order.RowVer {
		return model.ErrConcurrentUpdate
	}
	m.order.Status, m.order.RowVer = string(to), m.order.RowVer+1
	return nil
}

func (m *memDispatch) InsertTransition(_ context.Context, _ *repository.DB, t model.OrderTransition) error {
	m.transitions = append(m.transitions, t)
	return nil
}

func (m *memDispatch) OrderForDispatch(context.Context, *repository.DB, model.OrderRef) (repository.DispatchOrder, error) {
	o := m.order
	o.HasResult = m.result != nil
	return o, m.fail("read")
}

func (m *memDispatch) RecheckSource(context.Context, model.Scope, uint64) (repository.RecheckSource, error) {
	return repository.RecheckSource{UserID: 1002, FieldID: m.order.FieldID, Question: "3号田要不要打药"}, m.fail("source")
}

func (m *memDispatch) SupersedeRun(_ context.Context, _ model.Scope, oldRunID, newRunID uint64, _ time.Time) (bool, error) {
	m.superseded = [2]uint64{oldRunID, newRunID}
	return true, m.fail("supersede")
}

func (m *memDispatch) InsertPreDispatchChecks(_ context.Context, _ model.OrderRef, attempt int, checks []model.ConstraintCheck,
	_ time.Time) error {
	if err := m.fail("checks"); err != nil {
		return err
	}
	m.checks, m.checkEpoch = append(m.checks, checks...), attempt
	return nil
}

func (m *memDispatch) SetDispatchResultTx(_ context.Context, _ *repository.DB, _ model.OrderRef, result []byte, rowVer int,
	_ time.Time) error {
	if err := m.fail("result"); err != nil {
		return err
	}
	if m.order.Status != string(model.OrderDispatched) || m.result != nil || rowVer != m.order.RowVer {
		return model.ErrConcurrentUpdate
	}
	m.result, m.order.RowVer = result, m.order.RowVer+1
	return nil
}

func (m *memDispatch) MarkDoneTx(context.Context, *repository.DB, uint64, time.Time) (bool, error) {
	if err := m.fail("outbox"); err != nil {
		return false, err
	}
	if m.outbox != repository.OutboxPending {
		return false, nil
	}
	m.outbox = repository.OutboxDone
	return true, nil
}

// fakePrecheck 模拟 RecheckService：结论变化时原单 CANCELED 并返回新 run。
type fakePrecheck struct {
	store *memDispatch
	res   PreDispatchResult
	err   error
	calls int
	got   PreDispatchInput
}

func (f *fakePrecheck) PreDispatch(_ context.Context, in PreDispatchInput) (PreDispatchResult, error) {
	f.calls++
	f.got = in
	if f.err == nil && f.res.Changed {
		f.store.order.Status = string(model.OrderCanceled)
	}
	return f.res, f.err
}

type fakeCards struct{}

func (fakeCards) GetByRun(context.Context, model.Scope, uint64) (model.AdviceCard, error) {
	return model.AdviceCard{SelectedProduct: "P1"}, nil
}

type fakeEpochs struct{}

func (fakeEpochs) RecheckEpoch(context.Context, model.Scope, uint64) (int, error) { return 3, nil }

// countingAdapter 包装 MockAdapter 记录调用；err 非空时 DryRun 失败。
type countingAdapter struct {
	adapter.MockAdapter
	dryRuns int
	err     error
}

func (a *countingAdapter) DryRun(ctx context.Context, o model.WorkOrder) (adapter.DryRunResult, error) {
	a.dryRuns++
	if a.err != nil {
		return adapter.DryRunResult{}, a.err
	}
	return a.MockAdapter.DryRun(ctx, o)
}

type dispatchFixture struct {
	store  *memDispatch
	events *fakeEvents
	pre    *fakePrecheck
	dev    *countingAdapter
	flags  *fakeLiveFlags
	audit  *memAudit
	svc    *WorkOrderService
}

// fakeLiveFlags 是 LiveFlagReader 的桩：robot 为 enable_robot_dispatch 的生效值。
type fakeLiveFlags struct {
	robot bool
	err   error
	calls int
}

func (f *fakeLiveFlags) BoolLive(_ context.Context, key model.FlagKey, _ model.Scope) (bool, error) {
	f.calls++
	if key != model.FlagEnableRobotDispatch {
		return false, fmt.Errorf("unexpected flag %s", key)
	}
	return f.robot, f.err
}

// memAudit 记录 AppendTx 写入的审计行；fail 非空时写入失败。
type memAudit struct {
	recs []model.AuditRecord
	fail error
}

func (m *memAudit) AppendTx(_ context.Context, _ *repository.DB, r model.AuditRecord) error {
	if m.fail != nil {
		return m.fail
	}
	m.recs = append(m.recs, r)
	return nil
}

func newDispatchFixture(t *testing.T, status model.WorkOrderStatus, lockHeld bool) *dispatchFixture {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	deadline := dispatchNow.Add(time.Hour)
	store := &memDispatch{outbox: repository.OutboxPending, order: repository.DispatchOrder{ID: 3, TenantID: 1, FarmID: 1,
		FieldID: 8, AdviceID: 4, RunID: 5, WorkType: string(model.WorkSpray), DeviceType: "drone",
		Payload:        json.RawMessage(`{"product":"P1","dose_per_mu":{"value":30,"unit":"ml"},"area_mu":12}`),
		TimeWindowFrom: dispatchNow, TimeWindowTo: dispatchNow.Add(4 * time.Hour), Status: string(status),
		DispatchMode: string(model.DispatchDryRun), DispatchDeadlineAt: &deadline, RowVer: 2}}
	f := &dispatchFixture{store: store, events: &fakeEvents{}, dev: &countingAdapter{}, flags: &fakeLiveFlags{},
		audit: &memAudit{}}
	f.pre = &fakePrecheck{store: store, res: PreDispatchResult{Checks: []model.ConstraintCheck{
		{Phase: model.PhasePreDispatch, RuleID: "wind", Passed: true}}}}
	f.svc = NewWorkOrderService(fixedClock{dispatchNow}, store, f.events)
	if err := f.svc.SetDispatch(DispatchDeps{Adapter: f.dev, Recheck: f.pre, Store: store, Tx: store, Cards: fakeCards{},
		Epochs: fakeEpochs{}, Locks: memLocks{held: lockHeld}, Keys: keys, Flags: f.flags, Audit: f.audit,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	return f
}

func dispatchMsg(payload string) repository.OutboxMessage {
	return repository.OutboxMessage{ID: 70, TenantID: 1, FarmID: 1, Topic: repository.TopicOrderDispatch, AggregateID: 3,
		Payload: json.RawMessage(payload)}
}

const okDispatchPayload = `{"order_id":3,"ticket_id":7,"run_id":5}`

// dispatchedPhases 返回 DISPATCHED 事件的 phase 序列，并校验 dry_run=true / device_touched=false。
func (f *dispatchFixture) dispatchedPhases(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range f.events.events {
		if e.Type != model.EventDispatched {
			continue
		}
		var p model.DispatchedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if !p.DryRun || p.DeviceTouched || p.TicketID != 7 || p.OrderID != 3 || e.Actor != model.ActorSystem {
			t.Errorf("DISPATCHED payload = %+v actor %s", p, e.Actor)
		}
		out = append(out, p.Phase)
	}
	return out
}

// dispatchWant 是一次 Dispatch 之后的期望落库状态。
type dispatchWant struct {
	err       error  // nil 表示应成功
	status    string // 作业单最终状态
	outbox    string
	phases    string // DISPATCHED 事件 phase 序列（逗号分隔）
	dryRuns   int
	prechecks int
	hasResult bool
}

func (f *dispatchFixture) check(t *testing.T, err error, w dispatchWant) {
	t.Helper()
	switch {
	case w.err == nil && err != nil:
		t.Fatalf("Dispatch err = %v, want nil", err)
	case w.err == errAny && err == nil:
		t.Fatal("Dispatch err = nil, want an error")
	case w.err != nil && w.err != errAny && !errors.Is(err, w.err):
		t.Fatalf("Dispatch err = %v, want %v", err, w.err)
	}
	if f.store.order.Status != w.status || f.store.outbox != w.outbox {
		t.Errorf("order=%s outbox=%s, want %s / %s", f.store.order.Status, f.store.outbox, w.status, w.outbox)
	}
	if got := fmt.Sprint(f.dispatchedPhases(t)); got != w.phases {
		t.Errorf("DISPATCHED phases = %s, want %s", got, w.phases)
	}
	if f.dev.dryRuns != w.dryRuns || f.pre.calls != w.prechecks {
		t.Errorf("dryRuns=%d prechecks=%d, want %d / %d", f.dev.dryRuns, f.pre.calls, w.dryRuns, w.prechecks)
	}
	if (f.store.result != nil) != w.hasResult {
		t.Errorf("dispatch_result present = %t, want %t", f.store.result != nil, w.hasResult)
	}
}

// Dispatch 各分支：成功 / 重复投递 / 过期 / 复检变化 / 续跑，以及消息与状态非法、锁占用、复检无结论、写库失败回滚。
func TestDispatch(t *testing.T) {
	past := dispatchNow.Add(-time.Minute)
	tests := []struct {
		name    string
		status  model.WorkOrderStatus
		lock    bool
		payload string
		mutate  func(*dispatchFixture)
		want    dispatchWant
	}{
		{name: "normal dry-run", status: model.OrderApproved, want: dispatchWant{status: "DONE", outbox: "DONE",
			phases: "[INTENT RESULT]", dryRuns: 1, prechecks: 1, hasResult: true}},
		{name: "terminal duplicate delivery", status: model.OrderDone, want: dispatchWant{status: "DONE", outbox: "PENDING",
			phases: "[]"}},
		{name: "canceled duplicate delivery", status: model.OrderCanceled, want: dispatchWant{status: "CANCELED",
			outbox: "PENDING", phases: "[]"}},
		{name: "expired before dispatch", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.store.order.DispatchDeadlineAt = &past },
			want:   dispatchWant{status: "EXPIRED", outbox: "DONE", phases: "[]"}},
		{name: "recheck changed cancels", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.pre.res = PreDispatchResult{Changed: true, NewRunID: 9} },
			want:   dispatchWant{status: "CANCELED", outbox: "PENDING", phases: "[]", prechecks: 1}},
		{name: "dispatched without result resumes", status: model.OrderDispatched, want: dispatchWant{status: "DONE",
			outbox: "DONE", phases: "[RESULT]", dryRuns: 1, hasResult: true}},
		{name: "back to approval dropped", status: model.OrderPendingApproval, want: dispatchWant{status: "PENDING_APPROVAL",
			outbox: "PENDING", phases: "[]"}},
		{name: "bad json", status: model.OrderApproved, payload: `{`, want: dispatchWant{err: ErrBadOutboxPayload,
			status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "order id mismatch", status: model.OrderApproved, payload: `{"order_id":4,"ticket_id":7}`,
			want: dispatchWant{err: ErrBadOutboxPayload, status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "run id mismatch", status: model.OrderApproved, payload: `{"order_id":3,"ticket_id":7,"run_id":6}`,
			want: dispatchWant{err: ErrBadOutboxPayload, status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "draft not dispatchable", status: model.OrderDraft, want: dispatchWant{err: ErrDispatchState,
			status: "DRAFT", outbox: "PENDING", phases: "[]"}},
		{name: "real held when flag off", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.store.order.DispatchMode = string(model.DispatchReal) },
			want:   dispatchWant{status: "APPROVED", outbox: "DONE", phases: "[]"}},
		{name: "real with flag on refused (no real adapter)", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.store.order.DispatchMode, f.flags.robot = string(model.DispatchReal), true },
			want:   dispatchWant{err: ErrDispatchState, status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "real flag read error retries", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) {
				f.store.order.DispatchMode, f.flags.err = string(model.DispatchReal), errors.New("mysql down")
			},
			want: dispatchWant{err: errAny, status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "real held audit fails rolls back", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) {
				f.store.order.DispatchMode, f.audit.fail = string(model.DispatchReal), errors.New("audit down")
			},
			want: dispatchWant{err: errAny, status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "real overdue expires first", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) {
				f.store.order.DispatchMode, f.store.order.DispatchDeadlineAt = string(model.DispatchReal), &past
			},
			want: dispatchWant{status: "EXPIRED", outbox: "DONE", phases: "[]"}},
		{name: "unknown dispatch mode refused", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.store.order.DispatchMode = "SHADOW" },
			want:   dispatchWant{err: ErrDispatchState, status: "APPROVED", outbox: "PENDING", phases: "[]"}},
		{name: "real dispatched without result not resumed", status: model.OrderDispatched,
			mutate: func(f *dispatchFixture) { f.store.order.DispatchMode = string(model.DispatchReal) },
			want:   dispatchWant{err: ErrDispatchState, status: "DISPATCHED", outbox: "PENDING", phases: "[]"}},
		{name: "lock busy", status: model.OrderApproved, lock: true, want: dispatchWant{err: ErrOrderBusy,
			status: "APPROVED", outbox: "PENDING", phases: "[]", prechecks: 1}},
		{name: "recheck inconclusive", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.pre.err = agent.ErrRecheckInconclusive },
			want: dispatchWant{err: agent.ErrRecheckInconclusive, status: "APPROVED", outbox: "PENDING", phases: "[]",
				prechecks: 1}},
		{name: "checks write fails", status: model.OrderApproved, mutate: func(f *dispatchFixture) { f.store.failOn = "checks" },
			want: dispatchWant{err: errAny, status: "APPROVED", outbox: "PENDING", phases: "[]", prechecks: 1}},
		{name: "dry-run fails keeps intent", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.dev.err = errors.New("adapter down") },
			want: dispatchWant{err: errAny, status: "DISPATCHED", outbox: "PENDING", phases: "[INTENT]", dryRuns: 1,
				prechecks: 1}},
		{name: "result write rolls back", status: model.OrderApproved, mutate: func(f *dispatchFixture) { f.store.failOn = "result" },
			want: dispatchWant{err: errAny, status: "DISPATCHED", outbox: "PENDING", phases: "[INTENT]", dryRuns: 1,
				prechecks: 1}},
		{name: "outbox done rolls back result", status: model.OrderApproved,
			mutate: func(f *dispatchFixture) { f.store.failOn = "outbox" },
			want: dispatchWant{err: errAny, status: "DISPATCHED", outbox: "PENDING", phases: "[INTENT]", dryRuns: 1,
				prechecks: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDispatchFixture(t, tt.status, tt.lock)
			if tt.mutate != nil {
				tt.mutate(f)
			}
			payload := tt.payload
			if payload == "" {
				payload = okDispatchPayload
			}
			f.check(t, f.svc.Dispatch(context.Background(), dispatchMsg(payload)), tt.want)
		})
	}
}

// errAny 表示期望任意非 nil 错误（check 中特判）。
var errAny = errors.New("any error")

// REAL 单 + 开关关：Adapter 调用 0 次、不复检、无迁移，写一条 DISPATCH_HELD 审计（system、order:3、DENIED）。
func TestDispatch_RealHeldAudit(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	f.store.order.DispatchMode = string(model.DispatchReal)
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	if f.dev.dryRuns != 0 || f.pre.calls != 0 || len(f.store.transitions) != 0 || f.flags.calls != 1 {
		t.Errorf("dryRuns=%d prechecks=%d transitions=%d flagReads=%d, want 0/0/0/1", f.dev.dryRuns, f.pre.calls,
			len(f.store.transitions), f.flags.calls)
	}
	if len(f.audit.recs) != 1 {
		t.Fatalf("audit = %+v, want one DISPATCH_HELD", f.audit.recs)
	}
	r := f.audit.recs[0]
	var held dispatchHeld
	if err := json.Unmarshal(r.Summary, &held); err != nil {
		t.Fatal(err)
	}
	if r.Action != string(model.AuditDispatchHeld) || r.Target != "order:3" || r.Result != model.AuditResultDenied ||
		r.Actor.Type != model.ActorSystem || r.TenantID != 1 || r.FarmID != 1 || r.RunID == nil || *r.RunID != 5 ||
		held != (dispatchHeld{OrderID: 3, TicketID: 7, DispatchMode: model.DispatchReal, FlagKey: model.FlagEnableRobotDispatch}) {
		t.Errorf("audit = %+v summary %s", r, r.Summary)
	}
	// 重投：outbox 已 DONE，单仍 APPROVED → 再读一次开关、再写一条审计，仍不调 Adapter
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	if f.dev.dryRuns != 0 {
		t.Errorf("dryRuns after redelivery = %d, want 0", f.dev.dryRuns)
	}
}

// DRY_RUN 单不读 enable_robot_dispatch（开关只管 REAL）。
func TestDispatch_DryRunIgnoresFlag(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	if f.flags.calls != 0 || len(f.audit.recs) != 0 || f.dev.dryRuns != 1 {
		t.Errorf("flagReads=%d audits=%d dryRuns=%d, want 0/0/1", f.flags.calls, len(f.audit.recs), f.dev.dryRuns)
	}
}

// 正常 dry-run 的落库细节：迁移链、dispatch_result JSON、PRE_DISPATCH 校验与复检入参。
func TestDispatch_DryRunDetails(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	var chain []string
	for _, tr := range f.store.transitions {
		chain = append(chain, string(tr.From)+">"+string(tr.To))
		if tr.Actor.Type != model.ActorSystem {
			t.Errorf("transition actor = %+v, want system", tr.Actor)
		}
	}
	if got := fmt.Sprint(chain); got != "[APPROVED>DISPATCHED DISPATCHED>DONE]" {
		t.Errorf("transitions = %s", got)
	}
	var res model.DryRunDispatchResult
	if err := json.Unmarshal(f.store.result, &res); err != nil {
		t.Fatal(err)
	}
	var cmds []json.RawMessage
	if err := json.Unmarshal(res.Commands, &cmds); err != nil {
		t.Fatalf("commands %s: %v", res.Commands, err)
	}
	var params dispatchParams
	if err := json.Unmarshal(res.Params, &params); err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.DeviceTouched || res.Adapter != adapter.MockName || len(cmds) != 3 || !res.SimulatedAt.Equal(dispatchNow) ||
		params.WorkType != model.WorkSpray || params.DeviceType != "drone" || !params.TimeWindow.From.Equal(dispatchNow) {
		t.Errorf("dispatch_result = %s", f.store.result)
	}
	if len(f.store.checks) != 1 || f.store.checkEpoch != 3 {
		t.Errorf("pre-dispatch checks = %+v epoch %d", f.store.checks, f.store.checkEpoch)
	}
	q := f.pre.got.Question
	if q.Text != "3号田要不要打药" || q.Scope.UserID != 1002 || q.FieldID != 8 || q.RunID != 5 || f.pre.got.Epoch != 3 ||
		f.pre.got.Card.SelectedProduct != "P1" {
		t.Errorf("PreDispatch input = %+v", f.pre.got)
	}
}

// 过期：同事务 APPROVED → EXPIRED(APPROVAL_EXPIRED)，不复检不下发。
func TestDispatch_ExpiredReason(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	past := dispatchNow
	f.store.order.DispatchDeadlineAt = &past // 恰到截止时刻即过期
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	if len(f.store.transitions) != 1 || f.store.transitions[0].ReasonCode != model.ReasonApprovalExpired {
		t.Errorf("transitions = %+v", f.store.transitions)
	}
}

// 复检结论变化：旧 run 记 superseded_by_run_id；SupersedeRun 失败返回错误（原单已取消，重投会按终态跳过）。
func TestDispatch_Supersede(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	f.pre.res = PreDispatchResult{Changed: true, NewRunID: 9}
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	if f.store.superseded != [2]uint64{5, 9} {
		t.Errorf("superseded = %v, want [5 9]", f.store.superseded)
	}
	failing := newDispatchFixture(t, model.OrderApproved, false)
	failing.pre.res, failing.store.failOn = PreDispatchResult{Changed: true, NewRunID: 9}, "supersede"
	if err := failing.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err == nil {
		t.Error("supersede failure swallowed")
	}
	if err := failing.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Errorf("redelivery after cancel err = %v, want nil (terminal)", err)
	}
}

// DryRun 失败后重投：从 ⑥ 续跑，不再写 INTENT、不再复检，最终 DONE。
func TestDispatch_RetryAfterDryRunFailure(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	f.dev.err = errors.New("adapter down")
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err == nil {
		t.Fatal("want dry-run error")
	}
	f.dev.err = nil
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	f.check(t, nil, dispatchWant{status: "DONE", outbox: "DONE", phases: "[INTENT RESULT]", dryRuns: 2, prechecks: 1,
		hasResult: true})
}

// 未装配下发器 / 依赖缺失。
func TestDispatch_NotConfigured(t *testing.T) {
	svc := NewWorkOrderService(fixedClock{dispatchNow}, &memDispatch{}, &fakeEvents{})
	if err := svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); !errors.Is(err, ErrDispatchNotConfigured) {
		t.Errorf("Dispatch err = %v, want ErrDispatchNotConfigured", err)
	}
	store := &memDispatch{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, d := range map[string]DispatchDeps{
		"no adapter": {Store: store, Tx: store, Locks: memLocks{}, Flags: &fakeLiveFlags{}, Audit: &memAudit{}, Logger: log},
		"no flags":   {Adapter: adapter.MockAdapter{}, Store: store, Tx: store, Locks: memLocks{}, Audit: &memAudit{}, Logger: log},
		"no audit":   {Adapter: adapter.MockAdapter{}, Store: store, Tx: store, Locks: memLocks{}, Flags: &fakeLiveFlags{}, Logger: log},
		"recheck needs cards": {Adapter: adapter.MockAdapter{}, Recheck: &fakePrecheck{}, Store: store, Tx: store, Locks: memLocks{},
			Flags: &fakeLiveFlags{}, Audit: &memAudit{}, Logger: log},
	} {
		if err := svc.SetDispatch(d); !errors.Is(err, ErrDispatchNotConfigured) {
			t.Errorf("%s: SetDispatch err = %v", name, err)
		}
	}
}
