package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 回执处置告警（结构化日志 alert 字段，L7 接告警规则）。
const (
	alertReceiptConflict  = "RECEIPT_CONFLICT"
	alertInExecution      = "RECEIPT_FAILED_IN_EXECUTION"
	alertNeedsReconcile   = "RECEIPT_NEEDS_RECONCILE"
	alertRetryExhausted   = "DISPATCH_RETRY_EXHAUSTED"
	alertNoReceipt        = "RECEIPT_TIMEOUT_NO_RECEIPT"
	alertReceiptUnmatched = "RECEIPT_UNMATCHED"
	alertDoneAreaMissing  = "DONE_AREA_MISSING"
)

// receiptTarget 是回执最终会把工单带到的终态（含 IN_EXECUTION 有面积 → PARTIALLY_DONE 的覆盖）；非终结回执或转人工返回空。
func receiptTarget(r model.DeviceReceipt) model.WorkOrderStatus {
	rule, err := model.MapReceipt(r.Status, r.FailureKind, r.DoneAreaMu)
	if err != nil || rule.Effect == model.EffectReconcile || rule.Effect == model.EffectNone {
		return ""
	}
	if r.Status == model.AttemptFailed && r.FailureKind == model.FailureInExecution && hasArea(r.DoneAreaMu) {
		return model.OrderPartiallyDone
	}
	if rule.To.IsTerminal() {
		return rule.To
	}
	return ""
}

func hasArea(a *float64) bool { return a != nil && *a > 0 }

// classifyReceipt 判 effect（§6.5 第 6 步，前进序 DISPATCHED < EXECUTING < 终态）：
// 工单已终态 → LATE（终结回执指向别的终态 → CONFLICT）；尝试已终结（待对账除外）→ STALE（终结回执与之矛盾 → CONFLICT）；
// 回执前进序不大于尝试 → STALE；其余 APPLIED。待对账（NO_RECEIPT、FAILED+UNKNOWN）与 ACCEPTED 同级，迟到回执照常推进。
func classifyReceipt(cur model.WorkOrderStatus, att model.DispatchAttempt, r model.DeviceReceipt) model.ReceiptOutcome {
	if cur.IsTerminal() {
		if t := receiptTarget(r); t != "" && t != cur {
			return model.OutcomeConflict
		}
		return model.OutcomeLate
	}
	if att.Status.IsFinal() && !model.AwaitingReconcile(att.Status, att.FailureKind) {
		if r.Status.IsFinal() && (r.Status != att.Status || r.FailureKind != att.FailureKind) {
			return model.OutcomeConflict
		}
		return model.OutcomeStale
	}
	// 设备自报 FAILED+UNKNOWN：在途尝试据此转待对账（前进序与 ACCEPTED 同级，单独放行）；已待对账则 STALE。
	if model.AwaitingReconcile(r.Status, r.FailureKind) {
		if inFlight(att.Status) {
			return model.OutcomeApplied
		}
		return model.OutcomeStale
	}
	if model.AttemptRank(r.Status, r.FailureKind) <= model.AttemptRank(att.Status, att.FailureKind) {
		return model.OutcomeStale
	}
	return model.OutcomeApplied
}

// retryMessage 是重试 order.dispatch 的消息体（P1-L4 接缝 2：现有字段 + attempt）。
type retryMessage struct {
	OrderID  uint64 `json:"order_id"`
	TicketID uint64 `json:"ticket_id"`
	RunID    uint64 `json:"run_id"`
	Attempt  int    `json:"attempt"`
}

// applyAdvance 处置前进的回执：更新尝试（非终结回执顺延 receipt_due_at），按映射表迁移 / 重试 / 转人工，DONE 类同事务回写。
func (s *OrderOpsService) applyAdvance(ctx context.Context, tx *repository.DB, o repository.DispatchOrder,
	att model.DispatchAttempt, r model.DeviceReceipt, now time.Time) (ReceiptResult, error) {
	ref := o.Ref()
	cur := model.WorkOrderStatus(o.Status)
	res := ReceiptResult{Outcome: model.OutcomeApplied, OrderStatus: cur}
	due := att.ReceiptDueAt
	if !r.Status.IsFinal() {
		due = now.Add(s.d.ReceiptWindow)
	}
	if err := s.d.Store.UpdateReceiptTx(ctx, tx, ref, att.ID, att.RowVer, r.Status, r.FailureKind, r.DoneAreaMu, due, now); err != nil {
		return res, err
	}
	rule, err := model.MapReceipt(r.Status, r.FailureKind, r.DoneAreaMu)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	device := model.Actor{Type: model.ActorDevice}
	switch rule.Effect {
	case model.EffectNone:
		return res, nil
	case model.EffectReconcile:
		res.alert = alertNeedsReconcile
		return res, nil
	case model.EffectRetryOrFail:
		if att.Attempt < s.d.MaxAttempts {
			payload, err := json.Marshal(retryMessage{OrderID: o.ID, TicketID: att.TicketID, RunID: o.RunID, Attempt: att.Attempt + 1})
			if err != nil {
				return res, fmt.Errorf("service: encode retry order %d: %w", o.ID, err)
			}
			return res, s.d.Store.InsertRetryDispatchOutboxTx(ctx, tx, ref, att.TicketID, att.Attempt+1, payload, now)
		}
		res.alert = alertRetryExhausted
		return s.transitionTx(ctx, tx, o, att, res, model.OrderFailed, device, model.ReasonRetryExhausted, nil)
	}
	to := rule.To
	if r.Status == model.AttemptFailed && r.FailureKind == model.FailureInExecution {
		res.alert = alertInExecution
		if hasArea(r.DoneAreaMu) {
			to = model.OrderPartiallyDone
		}
	}
	area := r.DoneAreaMu
	if area == nil {
		area = att.DoneAreaMu
	}
	return s.transitionTx(ctx, tx, o, att, res, to, device, model.ReasonReceipt, area)
}

// transitionTx 迁移工单（目标即当前状态时跳过），REAL 单进入 DONE / PARTIALLY_DONE 时同事务回写。
func (s *OrderOpsService) transitionTx(ctx context.Context, tx *repository.DB, o repository.DispatchOrder, att model.DispatchAttempt,
	res ReceiptResult, to model.WorkOrderStatus, actor model.Actor, reason model.ReasonCode, area *float64) (ReceiptResult, error) {
	if to == model.WorkOrderStatus(o.Status) {
		return res, nil
	}
	if err := s.d.Orders.Transition(ctx, tx, o.Ref(), to, actor, reason); err != nil {
		return res, err
	}
	res.OrderStatus = to
	if err := s.writeDoneTx(ctx, tx, o, att.TicketID, to, area); err != nil {
		return res, err
	}
	if (to == model.OrderDone || to == model.OrderPartiallyDone) && area == nil && res.alert == "" &&
		model.DispatchMode(o.DispatchMode) == model.DispatchReal {
		res.alert = alertDoneAreaMissing
	}
	return res, nil
}

// fieldHistoryMessage 是 field.history.append 的消息体（处理器据此调 mcp-field 回写，order_id 作外部键）。
type fieldHistoryMessage struct {
	OrderID    uint64                `json:"order_id"`
	TicketID   uint64                `json:"ticket_id"`
	FieldID    uint64                `json:"field_id"`
	WorkType   string                `json:"work_type"`
	Status     model.WorkOrderStatus `json:"status"`
	DoneAreaMu *float64              `json:"done_area_mu,omitempty"`
	DoneAt     time.Time             `json:"done_at"`
}

// writeDoneTx 是 §6.5 第 9 步：REAL 单进入 DONE / PARTIALLY_DONE 的同一事务里回填 done_area_mu（有面积时）、
// INSERT IGNORE episodic_memory(WORK_DONE) 与 outbox(field.history.append)。其余情形不写。
func (s *OrderOpsService) writeDoneTx(ctx context.Context, tx *repository.DB, o repository.DispatchOrder, ticketID uint64,
	to model.WorkOrderStatus, area *float64) error {
	if (to != model.OrderDone && to != model.OrderPartiallyDone) || model.DispatchMode(o.DispatchMode) != model.DispatchReal {
		return nil
	}
	ref := o.Ref()
	now := s.d.Clock.Now()
	if area != nil {
		if err := s.d.Store.SetDoneAreaTx(ctx, tx, ref, *area, now); err != nil {
			return err
		}
	}
	summary := fmt.Sprintf("作业单 %d（%s）%s", o.ID, o.WorkType, to)
	if area != nil {
		summary += fmt.Sprintf("，完成 %.2f 亩", *area)
	}
	if err := s.d.Store.InsertWorkDoneMemoryTx(ctx, tx, ref, o.FieldID, summary, now); err != nil {
		return err
	}
	payload, err := json.Marshal(fieldHistoryMessage{OrderID: o.ID, TicketID: ticketID, FieldID: o.FieldID, WorkType: o.WorkType,
		Status: to, DoneAreaMu: area, DoneAt: now.UTC()})
	if err != nil {
		return fmt.Errorf("service: encode field.history order %d: %w", o.ID, err)
	}
	return s.d.Store.InsertFieldHistoryOutboxTx(ctx, tx, ref, ticketID, payload, now)
}
