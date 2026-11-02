package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 下发器的拦截关卡（① 时效、② 开关、④ 下发前复检）：不碰 Adapter，命中即本条消息处理完毕。

// overdue 判断是否已过下发截止时间。
func (s *WorkOrderService) overdue(o repository.DispatchOrder) bool {
	return o.DispatchDeadlineAt != nil && !s.clock.Now().Before(*o.DispatchDeadlineAt)
}

// dispatchHeld 是 DISPATCH_HELD 审计的 summary。
type dispatchHeld struct {
	OrderID      uint64             `json:"order_id"`
	TicketID     uint64             `json:"ticket_id"`
	DispatchMode model.DispatchMode `json:"dispatch_mode"`
	FlagKey      model.FlagKey      `json:"flag_key"`
}

// holdReal 是第 ② 步首次下发、enable_robot_dispatch 直读为关（P1 §6.1 功能任务 4）：不调 Adapter、单保持 APPROVED，
// 写 audit_log(DISPATCH_HELD)，返回 nil 由轮询器置 outbox DONE（到 dispatch_deadline_at 由时效扫描转 EXPIRED，开关恢复也不补发）。
func (s *WorkOrderService) holdReal(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	o repository.DispatchOrder) error {
	d := s.dispatch
	payload, err := json.Marshal(dispatchHeld{OrderID: o.ID, TicketID: m.TicketID, DispatchMode: model.DispatchReal,
		FlagKey: model.FlagEnableRobotDispatch})
	if err != nil {
		return fmt.Errorf("service: encode DISPATCH_HELD order %d: %w", o.ID, err)
	}
	runID := o.RunID
	rec := repository.NewAuditRecord(model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}, &runID,
		model.Actor{Type: model.ActorSystem}, model.AuditDispatchHeld, "order:"+strconv.FormatUint(o.ID, 10), payload,
		model.AuditResultDenied, s.clock.Now())
	if err := d.Tx.InTx(ctx, func(tx *repository.DB) error { return d.Audit.AppendTx(ctx, tx, rec) }); err != nil {
		return fmt.Errorf("service: dispatch hold order %d: %w", o.ID, err)
	}
	d.Logger.WarnContext(ctx, "dispatch: held, enable_robot_dispatch off", "order_id", o.ID, "outbox_id", msg.ID)
	return nil
}

// expire 持锁同事务 APPROVED → EXPIRED(APPROVAL_EXPIRED)（与 approval_expire 扫描同一迁移）。
func (s *WorkOrderService) expire(ctx context.Context, ref model.OrderRef) error {
	d := s.dispatch
	return WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, ref.OrderID, func(lctx context.Context) error {
		return s.expireLocked(lctx, ref)
	})
}

// expireLocked 是 expire 的落库部分（调用方已持 lock:order）。
func (s *WorkOrderService) expireLocked(ctx context.Context, ref model.OrderRef) error {
	d := s.dispatch
	err := d.Tx.InTx(ctx, func(tx *repository.DB) error {
		return s.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderExpired,
			model.Actor{Type: model.ActorSystem}, model.ReasonApprovalExpired)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch expire order %d: %w", ref.OrderID, err)
	}
	d.Logger.InfoContext(ctx, "dispatch: order expired", "order_id", ref.OrderID)
	return nil
}

// recheckGate 是第 ④ 步（锁外）：复检并按结论处置，返回 true 表示本条消息已处理完毕（不再下发）。
//   - 约束不通过（Blocked 非空）：首次 → 退回重审（reopenBlocked）；重试 → DISPATCHED → FAILED(RETRY_BLOCKED)。
//   - 结论变化（Changed）：首次 → P0 行为（原单已由复检取消，写旧 run superseded_by_run_id）；重试 → FAILED(RETRY_BLOCKED)。
//
// 两者同时出现时以约束不通过优先：约束不通过说明这张已批单当下不可执行，应交回审批人看见拦截证据并重新决定，
// 而不是被新 run 静默替换（复检在 Blocked 非空时也不会取消原单）。
// Recheck 未配置时只告警跳过（REAL 单在调用前已失败关闭）。
func (s *WorkOrderService) recheckGate(ctx context.Context, o repository.DispatchOrder, m dispatchMessage,
	epoch int) (bool, error) {
	d := s.dispatch
	if d.Recheck == nil {
		d.Logger.WarnContext(ctx, "dispatch: recheck unavailable, skipped", "order_id", o.ID, "run_id", o.RunID)
		return false, nil
	}
	retry := m.Attempt > 1
	res, card, err := s.recheck(ctx, o, epoch, retry)
	if err != nil {
		return false, err
	}
	switch {
	case len(res.Blocked) > 0 && retry:
		return true, s.failRetry(ctx, o.Ref(), m, "pre_dispatch blocked: "+strings.Join(res.Blocked, ","))
	case len(res.Blocked) > 0:
		return true, s.reopenBlocked(ctx, o, m, res, card)
	case res.Changed && retry:
		return true, s.failRetry(ctx, o.Ref(), m, "recheck changed: "+res.Detail)
	case res.Changed:
		return true, s.supersede(ctx, o, res)
	}
	return false, nil
}

// recheck 调复检并写 constraint_check(PRE_DISPATCH, attempt=epoch)。只有「原单已被复检取消」时校验记录写失败才容忍
// （处置结论已落库）；其余情况返回错误，由 outbox 重试重新复检。
func (s *WorkOrderService) recheck(ctx context.Context, o repository.DispatchOrder, epoch int,
	retry bool) (PreDispatchResult, model.AdviceCard, error) {
	d := s.dispatch
	scope := model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}
	src, err := d.Store.RecheckSource(ctx, scope, o.RunID)
	if err != nil {
		return PreDispatchResult{}, model.AdviceCard{}, fmt.Errorf("service: dispatch recheck order %d: %w", o.ID, err)
	}
	card, err := d.Cards.GetByRun(ctx, scope, o.RunID)
	if err != nil {
		return PreDispatchResult{}, model.AdviceCard{}, fmt.Errorf("service: dispatch recheck order %d card: %w", o.ID, err)
	}
	scope.UserID = src.UserID
	q := agent.Question{Text: src.Question, Scope: scope, FieldID: src.FieldID, RunID: o.RunID}
	res, err := d.Recheck.PreDispatch(ctx, PreDispatchInput{Order: o.Ref(), Question: q, Card: card, Epoch: epoch,
		Work: o.WorkOrder(), Retry: retry})
	if err != nil {
		return PreDispatchResult{}, model.AdviceCard{}, fmt.Errorf("service: dispatch: %w", err)
	}
	if err := d.Store.InsertPreDispatchChecks(ctx, o.Ref(), epoch, res.Checks, s.clock.Now()); err != nil {
		if !res.Changed || len(res.Blocked) > 0 || retry {
			return PreDispatchResult{}, model.AdviceCard{}, fmt.Errorf("service: dispatch: %w", err)
		}
		// 原单已取消，校验记录写失败不影响处置结论
		d.Logger.ErrorContext(ctx, "dispatch: record PRE_DISPATCH checks", "order_id", o.ID, "err", err)
	}
	return res, card, nil
}

// supersede 是首次下发结论变化的 P0 收尾：原单已由复检取消，写旧 run superseded_by_run_id。
func (s *WorkOrderService) supersede(ctx context.Context, o repository.DispatchOrder, res PreDispatchResult) error {
	d := s.dispatch
	if res.NewRunID != 0 {
		ok, err := d.Store.SupersedeRun(ctx, model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}, o.RunID, res.NewRunID,
			s.clock.Now())
		if err != nil {
			return fmt.Errorf("service: dispatch: %w", err)
		}
		if !ok {
			d.Logger.WarnContext(ctx, "dispatch: run already superseded", "run_id", o.RunID, "new_run_id", res.NewRunID)
		}
	}
	d.Logger.InfoContext(ctx, "dispatch: canceled by recheck", "order_id", o.ID, "new_run_id", res.NewRunID, "detail", res.Detail)
	return nil
}
