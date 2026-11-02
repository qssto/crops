package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func TestCancel(t *testing.T) {
	tests := []struct {
		name        string
		order       model.WorkOrderStatus
		att         model.AttemptStatus
		kind        model.FailureKind
		noJob       bool
		wantErr     error
		wantOrder   model.WorkOrderStatus
		wantPending bool
	}{
		{name: "APPROVED cancels directly", order: model.OrderApproved, att: model.AttemptAccepted, wantOrder: model.OrderCanceled},
		{name: "DRAFT cancels directly", order: model.OrderDraft, att: model.AttemptAccepted, wantOrder: model.OrderCanceled},
		{name: "DISPATCHED in flight goes through device", order: model.OrderDispatched, att: model.AttemptAccepted,
			wantOrder: model.OrderDispatched, wantPending: true},
		{name: "EXECUTING pending reconcile goes through device", order: model.OrderExecuting, att: model.AttemptNoReceipt,
			kind: model.FailureUnknown, wantOrder: model.OrderExecuting, wantPending: true},
		{name: "NOT_EXECUTED awaiting retry cancels directly", order: model.OrderDispatched, att: model.AttemptFailed,
			kind: model.FailureNotExecuted, wantOrder: model.OrderCanceled},
		{name: "in flight without job id", order: model.OrderDispatched, att: model.AttemptSent, noJob: true, wantErr: ErrCancelState},
		{name: "DONE cannot cancel", order: model.OrderDone, att: model.AttemptDone, wantErr: ErrCancelState},
		{name: "REJECTED cannot cancel", order: model.OrderRejected, att: model.AttemptAccepted, wantErr: ErrCancelState},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			f.setOrder(tt.order, model.DispatchReal)
			f.setAttempt(tt.att, tt.kind, 1)
			if tt.noJob {
				a := f.w.attempts[opsAtt]
				a.DeviceJobID = ""
				f.w.attempts[opsAtt] = a
			}
			s := f.svc(t)
			res, err := s.Cancel(t.Context(), opsScope(), opsOrder)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if len(f.w.transitions) != 0 || len(f.w.outbox) != 0 || len(f.auditTx.recs) != 0 {
					t.Fatal("failed cancel changed state")
				}
				return
			}
			if res.OrderStatus != tt.wantOrder || res.Pending != tt.wantPending || model.WorkOrderStatus(f.w.order(opsOrder).Status) != tt.wantOrder {
				t.Fatalf("res = %+v, order %s", res, f.w.order(opsOrder).Status)
			}
			if len(f.auditTx.recs) != 1 || f.auditTx.recs[0].Action != string(model.AuditOrderCancel) {
				t.Fatalf("audit = %+v", f.auditTx.recs)
			}
			if tt.wantPending {
				// 重复中止只留一条 order.cancel（dedup_key 幂等）。
				if _, err := s.Cancel(t.Context(), opsScope(), opsOrder); err != nil {
					t.Fatal(err)
				}
				if msgs := f.w.topics(repository.TopicOrderCancel); len(msgs) != 1 || msgs[0].Dedup != "order.cancel:100:300:1" {
					t.Fatalf("order.cancel outbox = %+v", msgs)
				}
				return
			}
			if len(f.w.outbox) != 0 || len(f.w.transitions) != 1 || f.w.transitions[0].Reason != model.ReasonCanceledByUser {
				t.Fatalf("outbox %+v transitions %+v", f.w.outbox, f.w.transitions)
			}
		})
	}
}

// 别的农场的作业单：中止与对账都按 ErrNotInScope 拒绝，不改状态。
func TestOrderOps_OtherFarmOrder(t *testing.T) {
	f := newOpsFixture()
	s := f.svc(t)
	if _, err := s.Cancel(t.Context(), opsScope(), 101); !errors.Is(err, ErrNotInScope) {
		t.Fatalf("cancel err = %v", err)
	}
	f.w.attempts[201] = model.DispatchAttempt{ID: 201, TenantID: opsTenant, FarmID: 11, OrderID: 101, Attempt: 1,
		Status: model.AttemptNoReceipt, FailureKind: model.FailureUnknown, RowVer: 1}
	if _, err := s.Reconcile(t.Context(), opsScope(), ReconcileInput{OrderID: 101, Conclusion: model.AttemptDone,
		DoneAreaMu: area(1)}); !errors.Is(err, ErrNotInScope) {
		t.Fatalf("reconcile err = %v", err)
	}
	if len(f.w.transitions) != 0 || len(f.w.outbox) != 0 || f.w.order(101).Status != string(model.OrderDispatched) {
		t.Fatal("other farm order changed")
	}
}

// cancelMsg 发起中止并取出 order.cancel 消息。
func cancelMsg(t *testing.T, f *opsFixture, s *OrderOpsService) repository.OutboxMessage {
	t.Helper()
	if _, err := s.Cancel(t.Context(), opsScope(), opsOrder); err != nil {
		t.Fatal(err)
	}
	msgs := f.w.topics(repository.TopicOrderCancel)
	if len(msgs) != 1 {
		t.Fatalf("order.cancel = %d", len(msgs))
	}
	return repository.OutboxMessage{ID: 1, TenantID: opsTenant, FarmID: opsFarm, Topic: repository.TopicOrderCancel, Payload: msgs[0].Payload}
}

func TestHandleCancel(t *testing.T) {
	tests := []struct {
		name        string
		cancels     []devResult
		finalBefore bool // 设备确认前回执已把工单推到 DONE
		wantErr     bool
		wantCalls   int
		wantOrder   model.WorkOrderStatus
		wantHistory int
	}{
		{name: "device confirms partial area", cancels: []devResult{{r: model.DeviceReceipt{Status: model.AttemptCanceled, DoneAreaMu: area(4), DeviceTime: opsNow}}},
			wantCalls: 1, wantOrder: model.OrderPartiallyDone, wantHistory: 1},
		{name: "device confirms nothing done", cancels: []devResult{{r: model.DeviceReceipt{Status: model.AttemptCanceled, DeviceTime: opsNow}}},
			wantCalls: 1, wantOrder: model.OrderCanceled},
		{name: "device error retries via outbox", cancels: []devResult{{err: errors.New("device busy")}}, wantErr: true,
			wantCalls: 1, wantOrder: model.OrderDispatched},
		{name: "invalid device answer", cancels: []devResult{{r: model.DeviceReceipt{Status: model.AttemptPartiallyDone}}}, wantErr: true,
			wantCalls: 1, wantOrder: model.OrderDispatched},
		{name: "already finished skips device", finalBefore: true, wantOrder: model.OrderDone, wantHistory: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			s := f.svc(t)
			msg := cancelMsg(t, f, s)
			if tt.finalBefore {
				if _, err := s.Webhook(t.Context(), hook(opsFarm, opsOrder, nil)); err != nil {
					t.Fatal(err)
				}
			}
			f.dev.cancels = tt.cancels
			err := s.HandleCancel(t.Context(), msg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if f.dev.cancelN != tt.wantCalls {
				t.Fatalf("Cancel calls = %d, want %d", f.dev.cancelN, tt.wantCalls)
			}
			if got := model.WorkOrderStatus(f.w.order(opsOrder).Status); got != tt.wantOrder {
				t.Fatalf("order = %s, want %s", got, tt.wantOrder)
			}
			if n := len(f.w.topics(repository.TopicFieldHistoryAppend)); n != tt.wantHistory {
				t.Fatalf("field.history = %d, want %d", n, tt.wantHistory)
			}
			if tt.wantErr && !strings.Contains(f.log.String(), "alert="+alertCancelFailed) {
				t.Fatalf("missing alert:\n%s", f.log)
			}
			if !tt.wantErr && !tt.finalBefore {
				for _, r := range f.w.receipts {
					if r.Source != model.SourceManual {
						t.Fatalf("cancel receipt source = %s", r.Source)
					}
				}
			}
		})
	}
}

// 中止确认 PARTIALLY_DONE → field.history.append 处理器写回地块记录；outbox 重投不重复。
func TestHandleFieldHistory_Idempotent(t *testing.T) {
	f := newOpsFixture()
	s := f.svc(t)
	msg := cancelMsg(t, f, s)
	f.dev.cancels = []devResult{{r: model.DeviceReceipt{Status: model.AttemptCanceled, DoneAreaMu: area(4), DeviceTime: opsNow}}}
	if err := s.HandleCancel(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	hist := f.w.topics(repository.TopicFieldHistoryAppend)
	if len(hist) != 1 {
		t.Fatalf("field.history = %d", len(hist))
	}
	out := repository.OutboxMessage{ID: 2, TenantID: opsTenant, FarmID: opsFarm, Topic: repository.TopicFieldHistoryAppend, Payload: hist[0].Payload}
	for i := 0; i < 2; i++ {
		if err := s.HandleFieldHistory(t.Context(), out); err != nil {
			t.Fatal(err)
		}
	}
	recs := f.hist.Records(opsTenant, opsFarm, 7)
	if len(recs) != 1 || recs[0].OrderID != opsOrder || recs[0].Status != model.OrderPartiallyDone || recs[0].DoneAreaMu == nil ||
		*recs[0].DoneAreaMu != 4 || recs[0].TicketID != opsTicket {
		t.Fatalf("records = %+v", recs)
	}
	if other := f.hist.Records(opsTenant, 11, 0); len(other) != 0 {
		t.Fatalf("other farm sees %+v", other)
	}
}

func TestHandleFieldHistory_Failures(t *testing.T) {
	f := newOpsFixture()
	f.history = UnwiredFieldHistory{}
	s := f.svc(t)
	good := repository.OutboxMessage{ID: 3, TenantID: opsTenant, FarmID: opsFarm, Payload: []byte(`{"order_id":100,"field_id":7}`)}
	if err := s.HandleFieldHistory(t.Context(), good); !errors.Is(err, ErrFieldHistoryUnwired) ||
		!strings.Contains(f.log.String(), "alert="+alertFieldHistory) {
		t.Fatalf("unwired err = %v\n%s", err, f.log)
	}
	if err := s.HandleFieldHistory(t.Context(), repository.OutboxMessage{ID: 4, Payload: []byte(`{"order_id":100}`)}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("missing scope err = %v", err)
	}
	if err := s.HandleFieldHistory(t.Context(), repository.OutboxMessage{ID: 5, TenantID: 1, FarmID: 10, Payload: []byte(`nope`)}); err == nil {
		t.Fatal("bad payload must fail")
	}
}

// P1-L4 审查修复 P1-a：中止 PENDING_APPROVAL 单同事务作废未决票（OPEN / PREPARING → SUPERSEDED）并收尾等审批的 run；
// 之后审批该票得 ErrTicketClosed（approval_expire 只取 OPEN 票，不再重扫）。
// 应失败：票据仍 OPEN（过期扫描 CANCELED → EXPIRED 非法、被 skippable 吞掉，票据永远 OPEN、run 永停 WAIT_APPROVAL）。
func TestCancel_PendingApprovalClosesTicket(t *testing.T) {
	tests := []struct {
		name       string
		ticket     model.TicketStatus
		runWaiting bool
		wantRun    model.ReasonCode
	}{
		{name: "OPEN ticket, run waiting approval", ticket: model.TicketOpen, runWaiting: true, wantRun: model.ReasonCanceledByUser},
		{name: "PREPARING ticket, run already terminal", ticket: model.TicketPreparing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			f.setOrder(model.OrderPendingApproval, model.DispatchReal)
			f.w.tickets[opsTicket] = repository.TicketRow{ID: opsTicket, TenantID: opsTenant, FarmID: opsFarm, RunID: 5,
				OrderID: opsOrder, TicketSeq: 1, Status: string(tt.ticket), RowVer: 3}
			f.w.waitingRuns[5] = tt.runWaiting
			if _, err := f.svc(t).Cancel(t.Context(), opsScope(), opsOrder); err != nil {
				t.Fatal(err)
			}
			tk := f.w.tickets[opsTicket]
			if tk.Status != string(model.TicketSuperseded) || tk.RowVer != 4 {
				t.Fatalf("ticket = %s row_ver %d, want SUPERSEDED / 4", tk.Status, tk.RowVer)
			}
			if f.w.closedRuns[5] != tt.wantRun {
				t.Fatalf("run closed with %q, want %q", f.w.closedRuns[5], tt.wantRun)
			}
			// 关掉的票再审批：TICKET_CLOSED，作业单不再迁移。
			d := newDecideFixture(t, model.RiskLow, time.Hour, false)
			d.store.ticket.Status = tk.Status
			if _, err := d.decide(approverA, "r-closed", DecisionApprove); !errors.Is(err, ErrTicketClosed) {
				t.Fatalf("decide closed ticket err = %v, want ErrTicketClosed", err)
			}
			if len(d.orders.to) != 0 || d.store.outbox != 0 {
				t.Fatalf("closed ticket decision moved order %v / outbox %d", d.orders.to, d.store.outbox)
			}
		})
	}
}

// 关票与作业单迁移同一事务：票据 row_ver 已变（并发决策）→ 整个中止回滚，作业单保持 PENDING_APPROVAL。
func TestCancel_TicketConflictRollsBack(t *testing.T) {
	f := newOpsFixture()
	f.setOrder(model.OrderPendingApproval, model.DispatchReal)
	f.w.tickets[opsTicket] = repository.TicketRow{ID: opsTicket, TenantID: opsTenant, FarmID: opsFarm, RunID: 5,
		OrderID: opsOrder, TicketSeq: 1, Status: string(model.TicketOpen), RowVer: 3}
	s := f.svc(t)
	s.d.Tickets = conflictTickets{f.w}
	if _, err := s.Cancel(t.Context(), opsScope(), opsOrder); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("err = %v, want ErrConcurrentUpdate", err)
	}
	if f.w.order(opsOrder).Status != string(model.OrderPendingApproval) || len(f.w.transitions) != 0 || len(f.auditTx.recs) != 0 {
		t.Fatalf("not rolled back: order %s transitions %+v", f.w.order(opsOrder).Status, f.w.transitions)
	}
}

// conflictTickets 模拟读到票后被并发决策改了 row_ver。
type conflictTickets struct{ *receiptWorld }

func (conflictTickets) SupersedeLiveTicketTx(context.Context, *repository.DB, repository.TicketRow, time.Time) error {
	return model.ErrConcurrentUpdate
}
