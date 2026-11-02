package service

import (
	"context"
	"fmt"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// OrderReopener 是第 ④ 步约束不通过时的退回重审（ReopenService 实现）：Prepare 锁外写新快照，Reopen 持 lock:order 落库。
type OrderReopener interface {
	Prepare(ctx context.Context, req ReopenRequest) (ReopenPlan, error)
	Reopen(ctx context.Context, p ReopenPlan) (model.ApprovalTicket, error)
}

// failRetry 是重试（attempt > 1）在 ①②④ 被拦下：持 lock:order 同事务 DISPATCHED → FAILED(RETRY_BLOCKED)（P1 §6.5 第 8 步）。
// 不再调 Adapter；返回 nil 即本条 outbox DONE。
func (s *WorkOrderService) failRetry(ctx context.Context, ref model.OrderRef, m dispatchMessage, why string) error {
	d := s.dispatch
	err := WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, ref.OrderID, func(lctx context.Context) error {
		return s.failRetryLocked(lctx, ref, m, why)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch retry blocked order %d attempt %d: %w", ref.OrderID, m.Attempt, err)
	}
	return nil
}

// failRetryLocked 是 failRetry 的落库部分（调用方已持 lock:order）。
func (s *WorkOrderService) failRetryLocked(ctx context.Context, ref model.OrderRef, m dispatchMessage, why string) error {
	d := s.dispatch
	err := d.Tx.InTx(ctx, func(tx *repository.DB) error {
		return s.TransitionFrom(ctx, tx, ref, model.OrderDispatched, model.OrderFailed,
			model.Actor{Type: model.ActorSystem}, model.ReasonRetryBlocked)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch retry blocked order %d attempt %d: %w", ref.OrderID, m.Attempt, err)
	}
	d.Logger.WarnContext(ctx, "dispatch: retry blocked, order failed", "order_id", ref.OrderID, "attempt", m.Attempt,
		"why", why)
	return nil
}

// reopenBlocked 是首次下发第 ④ 步约束不通过（P1 §6.4 第 4 条）：锁外 Prepare（读旧票、RawStore.Put 新快照），
// 再持 lock:order 复核仍为 APPROVED 后 Reopen（APPROVED → PENDING_APPROVAL、换新票、通知入 outbox）。不调 Adapter。
func (s *WorkOrderService) reopenBlocked(ctx context.Context, o repository.DispatchOrder, m dispatchMessage,
	res PreDispatchResult, card model.AdviceCard) error {
	d := s.dispatch
	work := o.WorkOrder()
	work.ConstraintChecks = res.Checks
	plan, err := d.Reopener.Prepare(ctx, ReopenRequest{TicketID: m.TicketID, Order: work, Card: card, RuleIDs: res.Blocked})
	if err != nil {
		return fmt.Errorf("service: dispatch reopen order %d: %w", o.ID, err)
	}
	ref := o.Ref()
	var ticket model.ApprovalTicket
	err = WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, o.ID, func(lctx context.Context) error {
		cur, err := d.Store.OrderForDispatch(lctx, nil, ref)
		if err != nil {
			return err
		}
		if model.WorkOrderStatus(cur.Status) != model.OrderApproved {
			return fmt.Errorf("%w: order %d changed to %s before reopen", ErrDispatchState, o.ID, cur.Status)
		}
		ticket, err = d.Reopener.Reopen(lctx, plan)
		return err
	})
	if err != nil {
		return fmt.Errorf("service: dispatch reopen order %d: %w", o.ID, err)
	}
	d.Logger.WarnContext(ctx, "dispatch: blocked by pre-dispatch constraints, returned to approval", "order_id", o.ID,
		"rule_ids", res.Blocked, "old_ticket_id", m.TicketID, "ticket_id", ticket.TicketID, "verdict_changed", res.Changed)
	return nil
}
