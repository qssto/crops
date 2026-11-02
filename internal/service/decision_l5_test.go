package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L5 决策红线：驳回原因必填、驳回原因入库、第二审批人生效守卫。

// 驳回带合法原因：rejection_reason 同事务写入，operator_role 为通过校验的角色，空文本存 NULL。
func TestDecide_RejectWritesReason(t *testing.T) {
	f := newDecideFixture(t, model.RiskLow, time.Hour, false)
	in := DecideInput{TicketID: 7, RequestID: "r1", Decision: DecisionReject, SnapshotDigest: testDigest,
		ReasonCode: model.RejectOther, ReasonText: "药剂与上次相同，需轮换"}
	res, err := f.decideInput(approverA, in, model.RoleOperator, model.RoleAgronomist)
	if err != nil || res.Status != model.TicketRejected {
		t.Fatalf("reject = %+v, %v", res, err)
	}
	if len(f.store.rejections) != 1 {
		t.Fatalf("rejections = %d", len(f.store.rejections))
	}
	r := f.store.rejections[0]
	if r.ReasonCode != model.RejectOther || r.ReasonText == nil || *r.ReasonText != in.ReasonText ||
		r.OperatorID != approverA || r.OperatorRole != model.RoleAgronomist {
		t.Fatalf("rejection = %+v", r)
	}

	f = newDecideFixture(t, model.RiskLow, time.Hour, false)
	if _, err := f.decide(approverA, "r1", DecisionReject); err != nil {
		t.Fatal(err)
	}
	if r := f.store.rejections[0]; r.ReasonCode != model.RejectDosage || r.ReasonText != nil {
		t.Fatalf("rejection without text = %+v", r)
	}
}

// 应失败：缺 reason_code / 非法 code / OTHER 缺文本 / 文本超长 → ErrBadInput + REJECT_REASON_INVALID，审计 DENIED，
// 票据、工单、决策行、驳回行都不变，idem 占位删除。
func TestDecide_RejectReasonInvalid(t *testing.T) {
	tests := []struct {
		name string
		code model.RejectReasonCode
		text string
	}{
		{"missing code", "", ""},
		{"missing code with text", "", "看着不对"},
		{"unknown code", "WEATHER", ""},
		{"other without text", model.RejectOther, ""},
		{"other with blank text", model.RejectOther, "  \t"},
		{"text too long", model.RejectDosage, strings.Repeat("字", model.MaxRejectReasonText+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDecideFixture(t, model.RiskLow, time.Hour, false)
			in := DecideInput{TicketID: 7, RequestID: "r1", Decision: DecisionReject, SnapshotDigest: testDigest,
				ReasonCode: tt.code, ReasonText: tt.text}
			_, err := f.decideInput(approverA, in, model.RoleAgronomist)
			if !errors.Is(err, ErrBadInput) || !errors.Is(err, ErrRejectReasonInvalid) {
				t.Fatalf("err = %v, want ErrBadInput + REJECT_REASON_INVALID", err)
			}
			if got := strings.Join(f.store.audits, ","); got != ErrRejectReasonInvalid.Error() {
				t.Errorf("audits = %q", got)
			}
			if f.store.ticket.Status != string(model.TicketOpen) || len(f.store.decisions) != 0 || len(f.store.rejections) != 0 ||
				len(f.orders.to) != 0 || f.store.runStatus != model.RunInterrupted {
				t.Errorf("state changed: ticket=%s decisions=%d rejections=%d transitions=%v run=%s", f.store.ticket.Status,
					len(f.store.decisions), len(f.store.rejections), f.orders.to, f.store.runStatus)
			}
			if _, ok := f.idem.m[f.keys.IdemApprove("r1")]; ok {
				t.Error("idem key kept after refusal")
			}
		})
	}
}

// 驳回原因在 P0 校验之后：票据已关时仍报 TICKET_CLOSED（P0 错误码优先），不报原因缺失。
func TestDecide_RejectReasonAfterP0Checks(t *testing.T) {
	f := newDecideFixture(t, model.RiskLow, time.Hour, false)
	f.store.ticket.Status = string(model.TicketApproved)
	in := DecideInput{TicketID: 7, RequestID: "r1", Decision: DecisionReject, SnapshotDigest: testDigest}
	if _, err := f.decideInput(approverA, in, model.RoleAgronomist); !errors.Is(err, ErrTicketClosed) || errors.Is(err, ErrRejectReasonInvalid) {
		t.Fatalf("err = %v, want TICKET_CLOSED", err)
	}
}

// 驳回原因写失败：整事务回滚，票据仍 OPEN。
func TestDecide_RejectionInsertFailureRollsBack(t *testing.T) {
	f := newDecideFixture(t, model.RiskLow, time.Hour, false)
	f.store.failOn = "rejection"
	if _, err := f.decide(approverA, "r1", DecisionReject); err == nil {
		t.Fatal("want error")
	}
	if f.store.ticket.Status != string(model.TicketOpen) || len(f.store.decisions) != 0 || len(f.store.rejections) != 0 {
		t.Fatalf("not rolled back: ticket=%s decisions=%d", f.store.ticket.Status, len(f.store.decisions))
	}
}

// 生效守卫：HIGH 第二人走 ConfirmTicketTx；LOW 票沿用 P0 DecideTicketTx。
func TestDecide_ConfirmUsesGuardedUpdate(t *testing.T) {
	f := newDecideFixture(t, model.RiskHigh, time.Hour, false)
	if _, err := f.decide(approverA, "r1", DecisionApprove); err != nil {
		t.Fatal(err)
	}
	res, err := f.decide(approverB, "r2", DecisionApprove)
	if err != nil || res.Status != model.TicketApproved || f.store.confirms != 1 {
		t.Fatalf("confirm = %+v, %v, confirms = %d", res, err, f.store.confirms)
	}
	low := newDecideFixture(t, model.RiskLow, time.Hour, false)
	if res, err := low.decide(approverA, "r1", DecisionApprove); err != nil || res.Status != model.TicketApproved || low.store.confirms != 0 {
		t.Fatalf("low approve = %+v, %v, confirms = %d", res, err, low.store.confirms)
	}
}

// 应失败：守卫 UPDATE 影响 0 行（库中 first_approver_id 为空，或等于本人）→ 不 APPROVED、无 outbox、无迁移。
// 读到的行与库中不一致模拟并发：service 以读到的 first_approver 判定为 CONFIRM，库中守卫拦下。
func TestDecide_ConfirmGuardZeroRows(t *testing.T) {
	tests := []struct {
		name   string
		stored func(*repository.TicketRow)
	}{
		{"first approver empty in db", func(r *repository.TicketRow) { r.FirstApproverID = nil }},
		{"first approver is self in db", func(r *repository.TicketRow) { self := uint64(approverB); r.FirstApproverID = &self }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDecideFixture(t, model.RiskHigh, time.Hour, false)
			first := uint64(approverA)
			f.store.ticket.FirstApproverID = &first
			f.store.beforeWrite = func(r *repository.TicketRow) { tt.stored(r) }
			_, err := f.decide(approverB, "r2", DecisionApprove)
			if err == nil || !errors.Is(err, model.ErrConcurrentUpdate) {
				t.Fatalf("err = %v, want concurrent update", err)
			}
			if f.store.ticket.Status != string(model.TicketOpen) || f.store.outbox != 0 || len(f.orders.to) != 0 ||
				len(f.store.decisions) != 0 || f.store.confirms != 0 {
				t.Fatalf("state changed: ticket=%s outbox=%d transitions=%v decisions=%d", f.store.ticket.Status,
					f.store.outbox, f.orders.to, len(f.store.decisions))
			}
		})
	}
}
