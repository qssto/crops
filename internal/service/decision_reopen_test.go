package service

import (
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// newReopenedDecideFixture 是复检退回后的新票（ticket_seq=2）：原 run 早已 COMPLETED，不再处于 WAIT_APPROVAL。
func newReopenedDecideFixture(t *testing.T, expiresIn time.Duration) *decideFixture {
	t.Helper()
	f := newDecideFixture(t, model.RiskLow, expiresIn, false)
	f.store.ticket.TicketSeq = 2
	f.store.runStatus = model.RunCompleted
	return f
}

// 新票批准：只写 order.dispatch，绝不 resume run（应失败：Resume 被调用）。
func TestDecide_ReopenedApproveDoesNotResume(t *testing.T) {
	f := newReopenedDecideFixture(t, time.Hour)
	res, err := f.decide(approverA, "r1", DecisionApprove)
	if err != nil || res.Status != model.TicketApproved {
		t.Fatalf("approve = %+v, %v", res, err)
	}
	if f.store.outbox != 1 || len(f.resumer.runs) != 0 {
		t.Fatalf("outbox = %d resumed = %v, want outbox only", f.store.outbox, f.resumer.runs)
	}
}

// 新票驳回 / 到期：作业单照常终结；run 已 COMPLETED 时不动（条件收尾，不因 0 行回滚整个决策）。
func TestDecide_ReopenedRejectAndExpireKeepRun(t *testing.T) {
	f := newReopenedDecideFixture(t, time.Hour)
	res, err := f.decide(approverA, "r1", DecisionReject)
	if err != nil || res.Status != model.TicketRejected || f.store.runReason != "" {
		t.Fatalf("reject = %+v, %v; run reason %q", res, err, f.store.runReason)
	}
	f = newReopenedDecideFixture(t, 0)
	if _, err := f.decide(approverA, "r1", DecisionApprove); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("expire err = %v", err)
	}
	if f.store.ticket.Status != string(model.TicketExpired) || len(f.orders.to) != 1 || f.orders.to[0] != model.OrderExpired ||
		f.store.runReason != "" {
		t.Fatalf("ticket = %s orders = %v run reason %q", f.store.ticket.Status, f.orders.to, f.store.runReason)
	}
}

// P1-L4 审查修复：首张票批准后 Resume 失败（run 仍 INTERRUPTED(WAIT_APPROVAL)）→ 复检退回 Reopen → 新票驳回 / 过期：
// 同事务把 run 收尾为 COMPLETED(APPROVAL_REJECTED / APPROVAL_EXPIRED)。
// 应失败：新票决策跳过收尾，run 永停 WAIT_APPROVAL（首张票已 SUPERSEDED，run_finalize 不再取到它）。
func TestDecide_ReopenedClosesRunStillWaiting(t *testing.T) {
	tests := []struct {
		name      string
		expiresIn time.Duration
		decision  string
		wantErr   error
		wantOrder model.WorkOrderStatus
		wantRun   model.ReasonCode
	}{
		{name: "new ticket rejected", expiresIn: time.Hour, decision: DecisionReject, wantOrder: model.OrderRejected,
			wantRun: model.ReasonApprovalRejected},
		{name: "new ticket expired", expiresIn: 0, decision: DecisionApprove, wantErr: ErrTicketExpired,
			wantOrder: model.OrderExpired, wantRun: model.ReasonApprovalExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReopenedDecideFixture(t, tt.expiresIn)
			f.store.runStatus = model.RunInterrupted // 首张票批准后 Resume 失败
			if _, err := f.decide(approverA, "r1", tt.decision); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(f.orders.to) != 1 || f.orders.to[0] != tt.wantOrder {
				t.Fatalf("orders = %v, want %s", f.orders.to, tt.wantOrder)
			}
			if f.store.runStatus != model.RunCompleted || f.store.runReason != tt.wantRun {
				t.Fatalf("run = %s/%q, want COMPLETED/%s", f.store.runStatus, f.store.runReason, tt.wantRun)
			}
			if len(f.resumer.runs) != 0 {
				t.Fatalf("resumed %v, want none", f.resumer.runs)
			}
		})
	}
}
