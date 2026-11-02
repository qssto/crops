package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

var (
	opsNow    = time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	opsSecret = []byte("test-receipt-secret")
)

// 固定数据：租户 1 / 农场 10 的作业单 100（REAL，DISPATCHED）与尝试 200（drone / J-1，ACCEPTED）；
// 农场 11 的作业单 101 与尝试 201（J-2）用来测跨农场。
const (
	opsTenant = 1
	opsFarm   = 10
	opsOrder  = 100
	opsAtt    = 200
	opsTicket = 300
)

type opsFixture struct {
	w       *receiptWorld
	dev     *scriptDevices
	idem    *memIdem
	raw     *fakeRaw
	audit   *memDenialStore
	auditTx *memAudit
	denier  *memDenier
	hist    *MemFieldHistory
	log     *bytes.Buffer
	locked  bool
	history FieldHistoryWriter
	// P1-L4 合并统一：locks / tx 非空时替换 memLocks / receiptWorld（丢锁用例）；maxAttempts 为零取 2（config 缺省）。
	locks       KeyLocker
	tx          TxRunner
	maxAttempts int
}

func newOpsFixture() *opsFixture {
	w := newReceiptWorld()
	w.orders[opsOrder] = repository.DispatchOrder{ID: opsOrder, TenantID: opsTenant, FarmID: opsFarm, FieldID: 7, RunID: 5,
		WorkType: string(model.WorkSpray), DeviceType: "drone", Status: string(model.OrderDispatched), DispatchMode: string(model.DispatchReal)}
	w.attempts[opsAtt] = model.DispatchAttempt{ID: opsAtt, TenantID: opsTenant, FarmID: opsFarm, OrderID: opsOrder, TicketID: opsTicket,
		Attempt: 1, DeviceType: "drone", IdemKey: "idem-1", DeviceJobID: "J-1", Mode: model.DispatchReal,
		Status: model.AttemptAccepted, ReceiptDueAt: opsNow.Add(30 * time.Minute), RowVer: 1}
	w.orders[101] = repository.DispatchOrder{ID: 101, TenantID: opsTenant, FarmID: 11, FieldID: 8, RunID: 6,
		WorkType: string(model.WorkSpray), DeviceType: "drone", Status: string(model.OrderDispatched), DispatchMode: string(model.DispatchReal)}
	w.attempts[201] = model.DispatchAttempt{ID: 201, TenantID: opsTenant, FarmID: 11, OrderID: 101, TicketID: 301, Attempt: 1,
		DeviceType: "drone", DeviceJobID: "J-2", Status: model.AttemptAccepted, ReceiptDueAt: opsNow.Add(30 * time.Minute), RowVer: 1}
	return &opsFixture{w: w, dev: &scriptDevices{}, idem: &memIdem{m: map[string]string{}}, raw: &fakeRaw{},
		audit: &memDenialStore{}, auditTx: &memAudit{}, denier: &memDenier{}, hist: NewMemFieldHistory(), log: &bytes.Buffer{}}
}

func (f *opsFixture) svc(t *testing.T) *OrderOpsService {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	var hist FieldHistoryWriter = f.hist
	if f.history != nil {
		hist = f.history
	}
	var locks KeyLocker = memLocks{held: f.locked}
	if f.locks != nil {
		locks = f.locks
	}
	var tx TxRunner = f.w
	if f.tx != nil {
		tx = f.tx
	}
	maxAttempts := f.maxAttempts
	if maxAttempts == 0 {
		maxAttempts = 2
	}
	s, err := NewOrderOpsService(OrderOpsDeps{Store: f.w, Tickets: f.w, Lookup: f.w, Tx: tx, Orders: f.w, Devices: f.dev, Idem: f.idem,
		Locks: locks, Keys: keys, Raw: f.raw, Audit: f.audit, AuditTx: f.auditTx, Denials: f.denier,
		Tenants: memFarmTenants{opsFarm: {opsTenant}, 12: {1, 2}}, History: hist, Secret: opsSecret, MaxAttempts: maxAttempts,
		Clock: fixedClock{opsNow}, Logger: slog.New(slog.NewTextHandler(f.log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// setAttempt 改尝试 200 的状态。
func (f *opsFixture) setAttempt(st model.AttemptStatus, kind model.FailureKind, attempt int) {
	a := f.w.attempts[opsAtt]
	a.Status, a.FailureKind, a.Attempt = st, kind, attempt
	f.w.attempts[opsAtt] = a
}

func (f *opsFixture) setOrder(st model.WorkOrderStatus, mode model.DispatchMode) {
	o := f.w.orders[opsOrder]
	o.Status, o.DispatchMode = string(st), string(mode)
	f.w.orders[opsOrder] = o
}

func area(v float64) *float64 { return &v }

func opsRef() model.OrderRef {
	return model.OrderRef{TenantID: opsTenant, FarmID: opsFarm, OrderID: opsOrder}
}

func receiptIn(id string, r model.DeviceReceipt) ReceiptInput {
	if r.DeviceTime.IsZero() {
		r.DeviceTime = opsNow.Add(-time.Minute)
	}
	return ReceiptInput{Ref: opsRef(), AttemptID: opsAtt, ReceiptID: id, Receipt: r, Source: model.SourceWebhook}
}

func TestProcess_Outcomes(t *testing.T) {
	tests := []struct {
		name        string
		order       model.WorkOrderStatus
		mode        model.DispatchMode
		att         model.AttemptStatus
		kind        model.FailureKind
		attempt     int
		r           model.DeviceReceipt
		wantOutcome model.ReceiptOutcome
		wantOrder   model.WorkOrderStatus
		wantAtt     model.AttemptStatus
		wantRetry   int // order.dispatch 重试消息数
		wantHistory int // field.history.append 消息数
		wantArea    *float64
		wantAlert   string // 日志里应出现的告警
	}{
		{name: "EXECUTING advances DISPATCHED", order: model.OrderDispatched, att: model.AttemptAccepted,
			r: model.DeviceReceipt{Status: model.AttemptExecuting, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderExecuting, wantAtt: model.AttemptExecuting},
		{name: "DONE before EXECUTING jumps to DONE with writeback", order: model.OrderDispatched, att: model.AttemptAccepted,
			r: model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(12)}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone, wantHistory: 1, wantArea: area(12)},
		{name: "DONE without area alerts", order: model.OrderExecuting, att: model.AttemptExecuting,
			r: model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone, wantHistory: 1, wantAlert: alertDoneAreaMissing},
		{name: "DRY_RUN DONE has no writeback", order: model.OrderDispatched, mode: model.DispatchDryRun, att: model.AttemptAccepted,
			r: model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(12)}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone},
		{name: "STALE: ACCEPTED after EXECUTING", order: model.OrderExecuting, att: model.AttemptExecuting,
			r: model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeStale,
			wantOrder: model.OrderExecuting, wantAtt: model.AttemptExecuting},
		{name: "STALE: duplicate EXECUTING", order: model.OrderExecuting, att: model.AttemptExecuting,
			r: model.DeviceReceipt{Status: model.AttemptExecuting, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeStale,
			wantOrder: model.OrderExecuting, wantAtt: model.AttemptExecuting},
		{name: "STALE: same final on final attempt", order: model.OrderDispatched, att: model.AttemptFailed, kind: model.FailureNotExecuted,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureNotExecuted}, wantOutcome: model.OutcomeStale,
			wantOrder: model.OrderDispatched, wantAtt: model.AttemptFailed},
		{name: "LATE: EXECUTING after DONE", order: model.OrderDone, att: model.AttemptDone,
			r: model.DeviceReceipt{Status: model.AttemptExecuting, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeLate,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone},
		{name: "LATE: duplicate DONE", order: model.OrderDone, att: model.AttemptDone,
			r: model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeLate,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone},
		{name: "CONFLICT: FAILED after DONE", order: model.OrderDone, att: model.AttemptDone,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureInExecution}, wantOutcome: model.OutcomeConflict,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone, wantAlert: alertReceiptConflict},
		{name: "CONFLICT: DONE on NOT_EXECUTED attempt", order: model.OrderDispatched, att: model.AttemptFailed, kind: model.FailureNotExecuted,
			r: model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeConflict,
			wantOrder: model.OrderDispatched, wantAtt: model.AttemptFailed, wantAlert: alertReceiptConflict},
		{name: "NOT_EXECUTED retries next attempt", order: model.OrderDispatched, att: model.AttemptAccepted, attempt: 1,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureNotExecuted}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderDispatched, wantAtt: model.AttemptFailed, wantRetry: 1},
		{name: "NOT_EXECUTED on last attempt fails order", order: model.OrderDispatched, att: model.AttemptAccepted, attempt: 2,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureNotExecuted}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderFailed, wantAtt: model.AttemptFailed, wantAlert: alertRetryExhausted},
		{name: "IN_EXECUTION with area is PARTIALLY_DONE", order: model.OrderExecuting, att: model.AttemptExecuting,
			r:           model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureInExecution, DoneAreaMu: area(3)},
			wantOutcome: model.OutcomeApplied, wantOrder: model.OrderPartiallyDone, wantAtt: model.AttemptFailed, wantHistory: 1,
			wantArea: area(3), wantAlert: alertInExecution},
		{name: "IN_EXECUTION without area is FAILED", order: model.OrderExecuting, att: model.AttemptExecuting,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureInExecution}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderFailed, wantAtt: model.AttemptFailed, wantAlert: alertInExecution},
		{name: "UNKNOWN goes to reconcile, no retry", order: model.OrderDispatched, att: model.AttemptAccepted,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureUnknown}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderDispatched, wantAtt: model.AttemptFailed, wantAlert: alertNeedsReconcile},
		{name: "UNKNOWN while EXECUTING goes to reconcile", order: model.OrderExecuting, att: model.AttemptExecuting,
			r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureUnknown}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderExecuting, wantAtt: model.AttemptFailed, wantAlert: alertNeedsReconcile},
		{name: "STALE: UNKNOWN on attempt already pending reconcile", order: model.OrderDispatched, att: model.AttemptNoReceipt,
			kind: model.FailureUnknown, r: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureUnknown},
			wantOutcome: model.OutcomeStale, wantOrder: model.OrderDispatched, wantAtt: model.AttemptNoReceipt},
		{name: "late DONE after NO_RECEIPT advances", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			r: model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(12)}, wantOutcome: model.OutcomeApplied,
			wantOrder: model.OrderDone, wantAtt: model.AttemptDone, wantHistory: 1, wantArea: area(12)},
		{name: "late ACCEPTED after NO_RECEIPT is STALE", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			r: model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "J-1"}, wantOutcome: model.OutcomeStale,
			wantOrder: model.OrderDispatched, wantAtt: model.AttemptNoReceipt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			mode := tt.mode
			if mode == "" {
				mode = model.DispatchReal
			}
			attempt := tt.attempt
			if attempt == 0 {
				attempt = 1
			}
			f.setOrder(tt.order, mode)
			f.setAttempt(tt.att, tt.kind, attempt)
			res, err := f.svc(t).Process(t.Context(), receiptIn("rc-1", tt.r))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tt.wantOutcome || res.Replayed {
				t.Fatalf("outcome = %s replayed %v, want %s", res.Outcome, res.Replayed, tt.wantOutcome)
			}
			if got := model.WorkOrderStatus(f.w.order(opsOrder).Status); got != tt.wantOrder || res.OrderStatus != tt.wantOrder {
				t.Fatalf("order = %s (result %s), want %s", got, res.OrderStatus, tt.wantOrder)
			}
			if got := f.w.attempt(opsAtt).Status; got != tt.wantAtt {
				t.Fatalf("attempt = %s, want %s", got, tt.wantAtt)
			}
			if n := len(f.w.receipts); n != 1 {
				t.Fatalf("device_receipt rows = %d, want 1", n)
			}
			for _, r := range f.w.receipts {
				if r.Effect != tt.wantOutcome || r.Source != model.SourceWebhook || r.Attempt != attempt {
					t.Fatalf("receipt row = %+v", r)
				}
			}
			retries := f.w.topics(repository.TopicOrderDispatch)
			if len(retries) != tt.wantRetry {
				t.Fatalf("retry outbox = %d, want %d", len(retries), tt.wantRetry)
			}
			if tt.wantRetry > 0 {
				var m retryMessage
				if err := json.Unmarshal(retries[0].Payload, &m); err != nil || m.Attempt != 2 || m.OrderID != opsOrder ||
					m.TicketID != opsTicket || m.RunID != 5 || retries[0].Dedup != "order.dispatch:100:300:2" {
					t.Fatalf("retry = %+v / %s (%v)", m, retries[0].Dedup, err)
				}
			}
			if n := len(f.w.topics(repository.TopicFieldHistoryAppend)); n != tt.wantHistory || len(f.w.memories) != tt.wantHistory {
				t.Fatalf("field.history = %d, memories = %d, want %d", n, len(f.w.memories), tt.wantHistory)
			}
			if got, ok := f.w.doneArea[opsOrder]; (tt.wantArea == nil) == ok || (ok && got != *tt.wantArea) {
				t.Fatalf("done_area = %v (%v), want %v", got, ok, tt.wantArea)
			}
			if tt.wantAlert != "" && !strings.Contains(f.log.String(), "alert="+tt.wantAlert) {
				t.Fatalf("missing alert %s in log:\n%s", tt.wantAlert, f.log)
			}
			if tt.wantAlert == "" && strings.Contains(f.log.String(), "alert=") {
				t.Fatalf("unexpected alert:\n%s", f.log)
			}
			for _, tr := range f.w.transitions {
				if tr.Actor.Type != model.ActorDevice {
					t.Fatalf("transition actor = %+v", tr.Actor)
				}
			}
		})
	}
}

func TestProcess_NonFinalExtendsDue(t *testing.T) {
	f := newOpsFixture()
	if _, err := f.svc(t).Process(t.Context(), receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptExecuting, DeviceJobID: "J-1"})); err != nil {
		t.Fatal(err)
	}
	if got := f.w.attempt(opsAtt).ReceiptDueAt; !got.Equal(opsNow.Add(DefaultReceiptWindow)) {
		t.Fatalf("receipt_due_at = %v", got)
	}
}

// 回执重放（§6.5 验收第 2 条计数断言）：同一 receipt_id 投三次 → device_receipt 1 行、迁移 1 次、field.history 1 条，
// 第二次起 Replayed；idem:receipt 丢失时由 uk_receipt 兜底，同样不再处理。
func TestProcess_Replay(t *testing.T) {
	for _, dropIdem := range []bool{false, true} {
		t.Run("drop idem "+strconv.FormatBool(dropIdem), func(t *testing.T) {
			f := newOpsFixture()
			s := f.svc(t)
			in := receiptIn("rc-dup", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(12)})
			in.Raw = []byte(`{"status":"DONE"}`)
			first, err := s.Process(t.Context(), in)
			if err != nil || first.Replayed || first.Outcome != model.OutcomeApplied {
				t.Fatalf("first = %+v, %v", first, err)
			}
			for i := 0; i < 2; i++ {
				if dropIdem {
					f.idem.m = map[string]string{}
				}
				txBefore := f.w.txCalls
				res, err := s.Process(t.Context(), in)
				if err != nil || !res.Replayed {
					t.Fatalf("replay %d = %+v, %v", i, res, err)
				}
				if !dropIdem && (f.w.txCalls != txBefore || res.Outcome != model.OutcomeApplied) {
					t.Fatalf("idem hit must short-circuit: tx %d -> %d, outcome %s", txBefore, f.w.txCalls, res.Outcome)
				}
			}
			if len(f.w.receipts) != 1 || len(f.w.transitions) != 1 || len(f.w.topics(repository.TopicFieldHistoryAppend)) != 1 {
				t.Fatalf("receipts %d, transitions %d, field.history %d; want 1/1/1", len(f.w.receipts), len(f.w.transitions),
					len(f.w.topics(repository.TopicFieldHistoryAppend)))
			}
			for _, r := range f.w.receipts {
				if r.RawObjectID != 501 {
					t.Fatalf("raw_object_id = %d", r.RawObjectID)
				}
			}
			if f.raw.puts[0].Kind != model.RawReceiptRaw {
				t.Fatalf("raw kind = %s", f.raw.puts[0].Kind)
			}
		})
	}
}

// idem:receipt 的归属与本次不符（别的租户 / 农场用了同一回执号）不当作重放。
func TestProcess_IdemOtherFarmNotReplay(t *testing.T) {
	f := newOpsFixture()
	s := f.svc(t)
	f.idem.m[s.d.Keys.IdemReceipt("rc-x")] = idemValue(model.OrderRef{TenantID: 9, FarmID: 99}, model.OutcomeApplied)
	res, err := s.Process(t.Context(), receiptIn("rc-x", model.DeviceReceipt{Status: model.AttemptExecuting, DeviceJobID: "J-1"}))
	if err != nil || res.Replayed || res.Outcome != model.OutcomeApplied {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestProcess_Failures(t *testing.T) {
	boom := errors.New("transition boom")
	tests := []struct {
		name    string
		mut     func(f *opsFixture)
		in      ReceiptInput
		wantErr error
	}{
		{name: "transition fails rolls back receipt and attempt", mut: func(f *opsFixture) { f.w.failTrans = boom },
			in: receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(1)}), wantErr: boom},
		{name: "order lock held", mut: func(f *opsFixture) { f.locked = true },
			in: receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1"}), wantErr: ErrOrderBusy},
		{name: "job mismatch", in: receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-9"}), wantErr: ErrBadInput},
		{name: "invalid receipt", in: receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptPartiallyDone, DeviceJobID: "J-1"}),
			wantErr: ErrBadInput},
		{name: "receipt id too long", in: receiptIn(strings.Repeat("x", receiptIDMax+1),
			model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1"}), wantErr: ErrBadInput},
		{name: "attempt of another farm", in: func() ReceiptInput {
			in := receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1"})
			in.AttemptID = 201
			return in
		}(), wantErr: model.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			if tt.mut != nil {
				tt.mut(f)
			}
			before := f.w.attempt(opsAtt)
			_, err := f.svc(t).Process(t.Context(), tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(f.w.receipts) != 0 || len(f.w.transitions) != 0 || len(f.w.outbox) != 0 || f.w.attempt(opsAtt).RowVer != before.RowVer {
				t.Fatalf("state changed on failure: receipts %d transitions %d outbox %d", len(f.w.receipts), len(f.w.transitions), len(f.w.outbox))
			}
			if len(f.idem.m) != 0 {
				t.Fatalf("idem written on failure: %v", f.idem.m)
			}
		})
	}
}

func TestReceiptIDOf(t *testing.T) {
	r := model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(1), DeviceTime: opsNow}
	a, b := ReceiptIDOf("", model.SourcePoll, r), ReceiptIDOf("", model.SourcePoll, r)
	if a != b || !strings.HasPrefix(a, "POLL-") || len(a) > receiptIDMax {
		t.Fatalf("derived ids %q / %q", a, b)
	}
	r2 := r
	r2.DoneAreaMu = area(2)
	if ReceiptIDOf("", model.SourcePoll, r2) == a || ReceiptIDOf("", model.SourceWebhook, r) == a {
		t.Fatal("different receipts must derive different ids")
	}
	if ReceiptIDOf("given", model.SourcePoll, r) != "given" {
		t.Fatal("given id must win")
	}
}

func TestNewOrderOpsService_RequiresDeps(t *testing.T) {
	if _, err := NewOrderOpsService(OrderOpsDeps{}); !errors.Is(err, ErrOrderOpsNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

// P1-L4 合并统一：max_attempts 只从 Deps（config dispatch.max_attempts）来，缺省不再回落本地常量（应失败：0 被当作 2）。
func TestNewOrderOpsService_RequiresMaxAttempts(t *testing.T) {
	f := newOpsFixture()
	keys := lockKeys(t)
	for _, n := range []int{0, -1} {
		_, err := NewOrderOpsService(OrderOpsDeps{Store: f.w, Tickets: f.w, Lookup: f.w, Tx: f.w, Orders: f.w, Devices: f.dev, Idem: f.idem,
			Locks: memLocks{}, Keys: keys, Raw: f.raw, Audit: f.audit, AuditTx: f.auditTx, Denials: f.denier,
			Tenants: memFarmTenants{}, History: f.hist, MaxAttempts: n, Clock: fixedClock{opsNow}, Logger: slog.New(slog.DiscardHandler)})
		if !errors.Is(err, ErrOrderOpsNotConfigured) {
			t.Fatalf("max_attempts %d: err = %v, want ErrOrderOpsNotConfigured", n, err)
		}
	}
}

// NOT_EXECUTED 是否再下发按注入的 max_attempts 判定（应失败：仍按固定 2 判定）。
func TestProcess_NotExecutedHonorsMaxAttempts(t *testing.T) {
	notExec := model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureNotExecuted}
	tests := []struct {
		name        string
		maxAttempts int
		attempt     int
		wantOrder   model.WorkOrderStatus
		wantRetry   string // 重试 outbox 的 dedup_key；空表示不重试
	}{
		{name: "max 3, attempt 2 retries", maxAttempts: 3, attempt: 2, wantOrder: model.OrderDispatched,
			wantRetry: "order.dispatch:100:300:3"},
		{name: "max 3, attempt 3 exhausted", maxAttempts: 3, attempt: 3, wantOrder: model.OrderFailed},
		{name: "max 1, attempt 1 exhausted", maxAttempts: 1, attempt: 1, wantOrder: model.OrderFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			f.maxAttempts = tt.maxAttempts
			f.setAttempt(model.AttemptAccepted, "", tt.attempt)
			if _, err := f.svc(t).Process(t.Context(), receiptIn("rc-1", notExec)); err != nil {
				t.Fatal(err)
			}
			if got := model.WorkOrderStatus(f.w.order(opsOrder).Status); got != tt.wantOrder {
				t.Fatalf("order = %s, want %s", got, tt.wantOrder)
			}
			retries := f.w.topics(repository.TopicOrderDispatch)
			if tt.wantRetry == "" {
				if len(retries) != 0 {
					t.Fatalf("retry outbox = %+v, want none", retries)
				}
				return
			}
			if len(retries) != 1 || retries[0].Dedup != tt.wantRetry {
				t.Fatalf("retry outbox = %+v, want %s", retries, tt.wantRetry)
			}
		})
	}
}

// ctxBlockedTx 模拟持锁事务中的慢 IO：等到 ctx 取消（丢锁）后返回其 cause，不执行 fn。
type ctxBlockedTx struct{}

func (ctxBlockedTx) InTx(ctx context.Context, _ func(*repository.DB) error) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(5 * time.Second):
		return errors.New("ctxBlockedTx: lock-lost ctx was not canceled")
	}
}

// P1-L4 合并统一：回执 / 对账 / 中止 / 超时置 NO_RECEIPT 都经 WithOrderLock，续租失败即取消持锁 ctx，
// 返回 ErrOrderLockLost 且不落任何写（应失败：沿用不续期的 Locker，事务照常跑完或错误不带 ErrOrderLockLost）。
func TestOrderOps_LockLostStopsHolder(t *testing.T) {
	withFastRenew(t)
	tests := []struct {
		name  string
		order model.WorkOrderStatus
		att   model.AttemptStatus
		kind  model.FailureKind
		call  func(ctx context.Context, s *OrderOpsService) error
	}{
		{name: "receipt", order: model.OrderDispatched, att: model.AttemptAccepted, call: func(ctx context.Context, s *OrderOpsService) error {
			_, err := s.Process(ctx, receiptIn("rc-1", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "J-1", DoneAreaMu: area(1)}))
			return err
		}},
		{name: "reconcile", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			call: func(ctx context.Context, s *OrderOpsService) error {
				_, err := s.Reconcile(ctx, opsScope(), ReconcileInput{OrderID: opsOrder, Conclusion: model.AttemptDone, DoneAreaMu: area(1)})
				return err
			}},
		{name: "cancel", order: model.OrderDispatched, att: model.AttemptAccepted, call: func(ctx context.Context, s *OrderOpsService) error {
			_, err := s.Cancel(ctx, opsScope(), opsOrder)
			return err
		}},
		{name: "mark no receipt", order: model.OrderDispatched, att: model.AttemptAccepted, call: func(ctx context.Context, s *OrderOpsService) error {
			return s.markNoReceipt(ctx, opsRef(), opsAtt, "test")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			f.setOrder(tt.order, model.DispatchReal)
			f.setAttempt(tt.att, tt.kind, 1)
			lease := &scriptedLease{renewErr: repository.ErrLockLost}
			f.locks, f.tx = &leaseLocks{lease: lease}, ctxBlockedTx{}
			err := tt.call(t.Context(), f.svc(t))
			if !errors.Is(err, ErrOrderLockLost) {
				t.Fatalf("err = %v, want ErrOrderLockLost", err)
			}
			if len(f.w.transitions) != 0 || len(f.w.receipts) != 0 || len(f.w.outbox) != 0 || len(f.auditTx.recs) != 0 ||
				f.w.attempt(opsAtt).Status != tt.att {
				t.Fatal("holder kept writing after lock lost")
			}
			if _, ok, err := f.idem.Get(t.Context(), lockKeys(t).IdemReceipt("rc-1")); err != nil || ok {
				t.Fatalf("idem:receipt written after lock lost (err %v)", err)
			}
			lease.mu.Lock()
			defer lease.mu.Unlock()
			if !lease.released {
				t.Fatal("lock not released")
			}
		})
	}
}

var _ context.Context // keep import when cases shrink
