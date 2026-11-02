package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// makeDue 让尝试 200 过了 receipt_due_at。
func (f *opsFixture) makeDue() {
	a := f.w.attempts[opsAtt]
	a.ReceiptDueAt = opsNow.Add(-time.Minute)
	f.w.attempts[opsAtt] = a
}

// 超时扫描红线：查询失败 / 结果非法 / 不前进 → 只置 NO_RECEIPT（UNKNOWN），工单不动、不写 outbox（不重发）、不转 FAILED。
func TestScanReceiptDue(t *testing.T) {
	tests := []struct {
		name      string
		due       bool
		locked    bool
		queries   []devResult
		wantQuery int
		wantAtt   model.AttemptStatus
		wantOrder model.WorkOrderStatus
		wantAlert bool
	}{
		{name: "query error marks NO_RECEIPT", due: true, queries: []devResult{{err: errors.New("timeout")}}, wantQuery: 1,
			wantAtt: model.AttemptNoReceipt, wantOrder: model.OrderDispatched, wantAlert: true},
		{name: "invalid result marks NO_RECEIPT", due: true, queries: []devResult{{r: model.DeviceReceipt{Status: model.AttemptFailed}}},
			wantQuery: 1, wantAtt: model.AttemptNoReceipt, wantOrder: model.OrderDispatched, wantAlert: true},
		{name: "no progress marks NO_RECEIPT", due: true, queries: []devResult{{r: model.DeviceReceipt{Status: model.AttemptAccepted}}},
			wantQuery: 1, wantAtt: model.AttemptNoReceipt, wantOrder: model.OrderDispatched, wantAlert: true},
		{name: "query DONE applies via Process", due: true,
			queries:   []devResult{{r: model.DeviceReceipt{Status: model.AttemptDone, DoneAreaMu: area(12), DeviceTime: opsNow}}},
			wantQuery: 1, wantAtt: model.AttemptDone, wantOrder: model.OrderDone},
		{name: "query EXECUTING advances and extends due", due: true,
			queries:   []devResult{{r: model.DeviceReceipt{Status: model.AttemptExecuting, DeviceTime: opsNow}}},
			wantQuery: 1, wantAtt: model.AttemptExecuting, wantOrder: model.OrderExecuting},
		{name: "not due is not queried", wantAtt: model.AttemptAccepted, wantOrder: model.OrderDispatched},
		{name: "scan lock held skips round", due: true, locked: true, wantAtt: model.AttemptAccepted, wantOrder: model.OrderDispatched},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			if tt.due {
				f.makeDue()
			}
			f.locked = tt.locked
			f.dev.queries = tt.queries
			if err := f.svc(t).ScanReceiptDueOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.dev.queryN != tt.wantQuery {
				t.Fatalf("QueryStatus calls = %d, want %d", f.dev.queryN, tt.wantQuery)
			}
			a := f.w.attempt(opsAtt)
			if a.Status != tt.wantAtt {
				t.Fatalf("attempt = %s, want %s", a.Status, tt.wantAtt)
			}
			if tt.wantAtt == model.AttemptNoReceipt && a.FailureKind != model.FailureUnknown {
				t.Fatalf("failure_kind = %s", a.FailureKind)
			}
			if got := model.WorkOrderStatus(f.w.order(opsOrder).Status); got != tt.wantOrder {
				t.Fatalf("order = %s, want %s", got, tt.wantOrder)
			}
			if len(f.w.topics(repository.TopicOrderDispatch)) != 0 {
				t.Fatal("scan must never re-dispatch")
			}
			if tt.wantAtt == model.AttemptNoReceipt && (len(f.w.transitions) != 0 || len(f.w.outbox) != 0) {
				t.Fatalf("NO_RECEIPT must not transition or write outbox: %+v %+v", f.w.transitions, f.w.outbox)
			}
			if got := strings.Contains(f.log.String(), "alert="+alertNoReceipt); got != tt.wantAlert {
				t.Fatalf("alert %v, want %v:\n%s", got, tt.wantAlert, f.log)
			}
			for _, r := range f.w.receipts {
				if r.Source != model.SourcePoll || !strings.HasPrefix(r.ReceiptID, "POLL-") {
					t.Fatalf("poll receipt row = %+v", r)
				}
			}
		})
	}
}

// 超时 → 待对账列表 → 迟到的 webhook DONE 照常推进（离开待对账列表）。
func TestScan_PendingThenLateWebhook(t *testing.T) {
	f := newOpsFixture()
	f.makeDue()
	f.dev.queries = []devResult{{err: errors.New("device offline")}}
	s := f.svc(t)
	if err := s.ScanReceiptDueOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	scope := model.Scope{TenantID: opsTenant, FarmID: opsFarm}
	pending, err := s.PendingReconcile(t.Context(), scope)
	if err != nil || len(pending) != 1 || pending[0].ID != opsAtt {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if other, _ := s.PendingReconcile(t.Context(), model.Scope{TenantID: opsTenant, FarmID: 11}); len(other) != 0 {
		t.Fatalf("other farm sees %+v", other)
	}
	// 第二轮：已不在途，不再查询。
	if err := s.ScanReceiptDueOnce(t.Context()); err != nil || f.dev.queryN != 1 {
		t.Fatalf("second round queried again: %d, %v", f.dev.queryN, err)
	}
	res, err := s.Webhook(t.Context(), hook(opsFarm, opsOrder, nil))
	if err != nil || res.Outcome != model.OutcomeApplied || res.OrderStatus != model.OrderDone {
		t.Fatalf("late webhook = %+v, %v", res, err)
	}
	if pending, _ := s.PendingReconcile(t.Context(), scope); len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}
}

func opsScope() model.Scope { return model.Scope{TenantID: opsTenant, FarmID: opsFarm, UserID: 42} }

func TestReconcile(t *testing.T) {
	tests := []struct {
		name        string
		order       model.WorkOrderStatus
		att         model.AttemptStatus
		kind        model.FailureKind
		locked      bool
		in          ReconcileInput
		wantErr     error
		wantOrder   model.WorkOrderStatus
		wantHistory int
	}{
		{name: "attempt not pending", order: model.OrderDispatched, att: model.AttemptAccepted,
			in: ReconcileInput{Conclusion: model.AttemptDone, DoneAreaMu: area(1)}, wantErr: ErrReconcileState},
		{name: "terminal order", order: model.OrderDone, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptDone, DoneAreaMu: area(1)}, wantErr: ErrReconcileState},
		{name: "conclusion FAILED UNKNOWN", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptFailed, FailureKind: model.FailureUnknown}, wantErr: ErrBadInput},
		{name: "conclusion not final", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptExecuting}, wantErr: ErrBadInput},
		{name: "PARTIALLY_DONE without area", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptPartiallyDone}, wantErr: ErrBadInput},
		{name: "comment too long", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptDone, Comment: strings.Repeat("长", maxReconcileComment+1)}, wantErr: ErrBadInput},
		{name: "order lock held", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown, locked: true,
			in: ReconcileInput{Conclusion: model.AttemptDone, DoneAreaMu: area(1)}, wantErr: ErrOrderBusy},
		{name: "NO_RECEIPT reconciled DONE", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptDone, DoneAreaMu: area(12), Comment: "飞手确认"}, wantOrder: model.OrderDone, wantHistory: 1},
		{name: "FAILED UNKNOWN reconciled CANCELED with area", order: model.OrderExecuting, att: model.AttemptFailed, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptCanceled, DoneAreaMu: area(2)}, wantOrder: model.OrderPartiallyDone, wantHistory: 1},
		{name: "reconciled NOT_EXECUTED fails order", order: model.OrderDispatched, att: model.AttemptNoReceipt, kind: model.FailureUnknown,
			in: ReconcileInput{Conclusion: model.AttemptFailed, FailureKind: model.FailureNotExecuted}, wantOrder: model.OrderFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			f.setOrder(tt.order, model.DispatchReal)
			f.setAttempt(tt.att, tt.kind, 1)
			f.locked = tt.locked
			tt.in.OrderID = opsOrder
			res, err := f.svc(t).Reconcile(t.Context(), opsScope(), tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if len(f.w.transitions) != 0 || len(f.auditTx.recs) != 0 || f.w.attempt(opsAtt).Status != tt.att {
					t.Fatalf("failed reconcile changed state")
				}
				return
			}
			if res.OrderStatus != tt.wantOrder || res.AttemptID != opsAtt || model.WorkOrderStatus(f.w.order(opsOrder).Status) != tt.wantOrder {
				t.Fatalf("res = %+v, order %s", res, f.w.order(opsOrder).Status)
			}
			if a := f.w.attempt(opsAtt); a.Status != tt.in.Conclusion || a.FailureKind != tt.in.FailureKind {
				t.Fatalf("attempt = %s/%s", a.Status, a.FailureKind)
			}
			if len(f.w.transitions) != 1 || f.w.transitions[0].Actor != (model.Actor{Type: model.ActorUser, ID: 42}) ||
				f.w.transitions[0].Reason != model.ReasonReconciled {
				t.Fatalf("transitions = %+v", f.w.transitions)
			}
			if len(f.auditTx.recs) != 1 || f.auditTx.recs[0].Action != string(model.AuditReconcile) || f.auditTx.recs[0].FarmID != opsFarm {
				t.Fatalf("audit = %+v", f.auditTx.recs)
			}
			if n := len(f.w.topics(repository.TopicFieldHistoryAppend)); n != tt.wantHistory {
				t.Fatalf("field.history = %d, want %d", n, tt.wantHistory)
			}
			if len(f.w.topics(repository.TopicOrderDispatch)) != 0 {
				t.Fatal("reconcile must not re-dispatch")
			}
		})
	}
}
