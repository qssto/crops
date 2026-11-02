package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 中止补偿（§6.5 L4-B 第 6 条）：未下发的单直接 CANCELED；已下发的单写 outbox(order.cancel)，
// 处理器调 Adapter.Cancel，设备确认后按回执（done_area_mu）经 Process 定 CANCELED 或 PARTIALLY_DONE。

// alertCancelFailed 是中止调用设备失败的告警（outbox 退避重试，耗尽转人工）。
const alertCancelFailed = "ORDER_CANCEL_DEVICE_FAILED"

// scopedOrderErr 把用户请求里按 Scope 取工单未命中映射为 ErrNotInScope（controller 回 403 并审计，不暴露是否存在）。
func scopedOrderErr(err error) error {
	if errors.Is(err, model.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotInScope, err)
	}
	return err
}

// CancelResult 是中止请求的结论。Pending 为 true 表示已写 order.cancel，等设备确认（工单状态暂不变）。
type CancelResult struct {
	OrderID     uint64                `json:"order_id"`
	OrderStatus model.WorkOrderStatus `json:"order_status"`
	Pending     bool                  `json:"pending"`
}

// cancelMessage 是 order.cancel 的消息体。
type cancelMessage struct {
	OrderID     uint64 `json:"order_id"`
	TicketID    uint64 `json:"ticket_id"`
	AttemptID   uint64 `json:"attempt_id"`
	DeviceJobID string `json:"device_job_id"`
}

// cancelAudit 是 ORDER_CANCEL 审计的 payload。
type cancelAudit struct {
	OrderID   uint64                `json:"order_id"`
	From      model.WorkOrderStatus `json:"from"`
	AttemptID uint64                `json:"attempt_id,omitempty"`
	Pending   bool                  `json:"pending"`
}

// Cancel 中止作业单（权限 order.cancel 由路由层判定），持 lock:order 单事务：
// DRAFT / PENDING_APPROVAL / APPROVED → CANCELED(CANCELED_BY_USER)，同事务作废未决票并收尾等审批的 run（closeTicketsTx）；
// DISPATCHED / EXECUTING：最后一次尝试在途或待对账 →
// 须有 device_job_id（否则 ErrCancelState，转人工）并写 outbox(order.cancel)；尝试已确认未执行（等重试）→ 直接 CANCELED；
// 终态 → ErrCancelState。同事务写 audit_log(ORDER_CANCEL)。
func (s *OrderOpsService) Cancel(ctx context.Context, scope model.Scope, orderID uint64) (CancelResult, error) {
	if scope.TenantID == 0 || scope.FarmID == 0 || orderID == 0 {
		return CancelResult{}, fmt.Errorf("%w: cancel needs tenant / farm / order", ErrBadInput)
	}
	ref := model.OrderRef{TenantID: scope.TenantID, FarmID: scope.FarmID, OrderID: orderID}
	var out CancelResult
	err := WithOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, orderID, func(lctx context.Context) error {
		var lerr error
		out, lerr = s.cancelLocked(lctx, scope, ref)
		return lerr
	})
	if err != nil {
		return CancelResult{}, fmt.Errorf("service: cancel order %d: %w", orderID, err)
	}
	return out, nil
}

// cancelLocked 是 Cancel 的落库部分（调用方已持 lock:order）。
func (s *OrderOpsService) cancelLocked(ctx context.Context, scope model.Scope, ref model.OrderRef) (CancelResult, error) {
	actor := model.Actor{Type: model.ActorUser, ID: scope.UserID}
	out := CancelResult{OrderID: ref.OrderID}
	err := s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		o, err := s.d.Store.OrderForDispatch(ctx, tx, ref)
		if err != nil {
			return scopedOrderErr(err)
		}
		cur := model.WorkOrderStatus(o.Status)
		out.OrderStatus = cur
		audit := cancelAudit{OrderID: o.ID, From: cur}
		now := s.d.Clock.Now()
		switch cur {
		case model.OrderDraft, model.OrderPendingApproval, model.OrderApproved:
			if err := s.d.Orders.Transition(ctx, tx, ref, model.OrderCanceled, actor, model.ReasonCanceledByUser); err != nil {
				return err
			}
			if err := s.closeTicketsTx(ctx, tx, ref, now); err != nil {
				return err
			}
			out.OrderStatus = model.OrderCanceled
		case model.OrderDispatched, model.OrderExecuting:
			att, err := s.d.Store.LatestAttemptTx(ctx, tx, ref)
			if err != nil {
				return err
			}
			audit.AttemptID = att.ID
			if !inFlight(att.Status) && !model.AwaitingReconcile(att.Status, att.FailureKind) {
				// 最后一次尝试已确认未执行、等重试：设备上没有在跑的任务，直接中止（重试下发见 CANCELED 即停）。
				if err := s.d.Orders.Transition(ctx, tx, ref, model.OrderCanceled, actor, model.ReasonCanceledByUser); err != nil {
					return err
				}
				out.OrderStatus = model.OrderCanceled
				break
			}
			if att.DeviceJobID == "" {
				return fmt.Errorf("%w: order %d attempt %d has no device_job_id, needs manual handling", ErrCancelState, o.ID, att.ID)
			}
			payload, err := json.Marshal(cancelMessage{OrderID: o.ID, TicketID: att.TicketID, AttemptID: att.ID, DeviceJobID: att.DeviceJobID})
			if err != nil {
				return fmt.Errorf("service: encode order.cancel %d: %w", o.ID, err)
			}
			if _, err := s.d.Store.InsertCancelOutboxTx(ctx, tx, ref, att.TicketID, payload, now); err != nil {
				return err
			}
			out.Pending, audit.Pending = true, true
		default:
			return fmt.Errorf("%w: order %d is %s", ErrCancelState, o.ID, cur)
		}
		payload, err := json.Marshal(audit)
		if err != nil {
			return fmt.Errorf("service: encode ORDER_CANCEL: %w", err)
		}
		runID := o.RunID
		return s.d.AuditTx.AppendTx(ctx, tx, repository.NewAuditRecord(scope, &runID, actor, model.AuditOrderCancel,
			"order:"+strconv.FormatUint(o.ID, 10), payload, model.AuditResultOK, now))
	})
	return out, err
}

// closeTicketsTx 在中止事务内作废作业单上未决的票（OPEN / PREPARING → SUPERSEDED，row_ver 守卫），并按 closeRunTx 语义
// 收尾仍 INTERRUPTED(WAIT_APPROVAL) 的 run（COMPLETED(CANCELED_BY_USER)；run 已终态或续跑中则不动）。
// 选 SUPERSEDED 而非 EXPIRED / REJECTED：票未到期也无人驳回，是被作业单层面的中止取代；approval_expire 只取 OPEN 票，
// 不会再把它当过期票重扫（否则 CANCELED → EXPIRED 是非法迁移，票据永远 OPEN、run 永停 WAIT_APPROVAL）。
func (s *OrderOpsService) closeTicketsTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, now time.Time) error {
	tickets, err := s.d.Tickets.LiveTicketsTx(ctx, tx, ref)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		if err := s.d.Tickets.SupersedeLiveTicketTx(ctx, tx, t, now); err != nil {
			return err
		}
		if _, err := s.d.Tickets.CloseWaitingRunTx(ctx, tx, t.Scope(), t.RunID, model.ReasonCanceledByUser, now); err != nil {
			return err
		}
	}
	return nil
}

// HandleCancel 是 outbox(order.cancel) 处理器：尝试已被回执终结 → 完成；否则调 Adapter.Cancel(device_job_id)，
// 设备确认的回执交 Process（source=MANUAL）定 CANCELED / PARTIALLY_DONE。设备调用失败返回错误（outbox 退避重试）并告警。
func (s *OrderOpsService) HandleCancel(ctx context.Context, msg repository.OutboxMessage) error {
	var p cancelMessage
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return fmt.Errorf("service: order.cancel outbox %d payload: %w", msg.ID, err)
	}
	if p.OrderID == 0 || p.AttemptID == 0 || p.DeviceJobID == "" || msg.TenantID == 0 || msg.FarmID == 0 {
		return fmt.Errorf("%w: order.cancel outbox %d missing tenant / farm / order / attempt / job", ErrBadInput, msg.ID)
	}
	ref := model.OrderRef{TenantID: msg.TenantID, FarmID: msg.FarmID, OrderID: p.OrderID}
	var att model.DispatchAttempt
	var o repository.DispatchOrder
	if err := s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		var err error
		if att, err = s.d.Store.AttemptTx(ctx, tx, ref, p.AttemptID); err != nil {
			return err
		}
		o, err = s.d.Store.OrderForDispatch(ctx, tx, ref)
		return err
	}); err != nil {
		return fmt.Errorf("service: order.cancel read order %d: %w", p.OrderID, err)
	}
	if model.WorkOrderStatus(o.Status).IsTerminal() ||
		(!inFlight(att.Status) && !model.AwaitingReconcile(att.Status, att.FailureKind)) {
		return nil
	}
	r, err := s.d.Devices.Cancel(ctx, DeviceTarget{DeviceType: att.DeviceType, Ref: ref, RunID: o.RunID}, p.DeviceJobID)
	if err == nil {
		if r.DeviceJobID == "" {
			r.DeviceJobID = p.DeviceJobID
		}
		err = r.Validate()
	}
	if err != nil {
		s.d.Logger.ErrorContext(ctx, "order cancel: device cancel failed", "alert", alertCancelFailed, "order_id", p.OrderID,
			"attempt_id", p.AttemptID, "outbox_id", msg.ID, "attempts", msg.Attempts, "err", err)
		return fmt.Errorf("service: order.cancel device order %d: %w", p.OrderID, err)
	}
	if _, err := s.Process(ctx, ReceiptInput{Ref: ref, AttemptID: att.ID, ReceiptID: ReceiptIDOf("", model.SourceManual, r),
		Receipt: r, Source: model.SourceManual}); err != nil {
		return fmt.Errorf("service: order.cancel receipt order %d: %w", p.OrderID, err)
	}
	return nil
}
