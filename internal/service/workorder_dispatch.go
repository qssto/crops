package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// adapterNameCustom 是未实现 Name() 的 Adapter 在 dispatch_result.adapter 中的取值。
const adapterNameCustom = "custom"

// 下发器错误。
var (
	// ErrDispatchNotConfigured 表示 SetDispatch 未调用。
	ErrDispatchNotConfigured = errors.New("service: dispatch not configured")
	// ErrDispatchState 表示作业单处于下发器不处理的状态（由 outbox 退避重试，耗尽后 DEAD 告警）。
	ErrDispatchState = errors.New("service: work order not dispatchable")
	// ErrBadOutboxPayload 表示 order.dispatch 消息体与 outbox 行不一致。
	ErrBadOutboxPayload = errors.New("service: bad order.dispatch payload")
)

// DispatchStore 是下发器的仓储能力，repository.DispatchRepo 实现；*Tx 方法只在调用方事务内使用。
type DispatchStore interface {
	OrderForDispatch(ctx context.Context, db *repository.DB, ref model.OrderRef) (repository.DispatchOrder, error)
	RecheckSource(ctx context.Context, scope model.Scope, runID uint64) (repository.RecheckSource, error)
	SupersedeRun(ctx context.Context, scope model.Scope, oldRunID, newRunID uint64, now time.Time) (bool, error)
	InsertPreDispatchChecks(ctx context.Context, ref model.OrderRef, attempt int, checks []model.ConstraintCheck, now time.Time) error
	SetDispatchResultTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, result []byte, rowVer int, now time.Time) error
	MarkDoneTx(ctx context.Context, tx *repository.DB, id uint64, now time.Time) (bool, error)
}

// Prechecker 是下发前复检，RecheckService 实现。
type Prechecker interface {
	PreDispatch(ctx context.Context, in PreDispatchInput) (PreDispatchResult, error)
}

// EpochAllocator 为复检分配新 epoch（resume_epoch+1 并持久化），repository.RunRepo 实现。
type EpochAllocator interface {
	RecheckEpoch(ctx context.Context, scope model.Scope, runID uint64) (int, error)
}

// DispatchDeps 是 WorkOrderService.Dispatch 的依赖。Recheck 为 nil 时跳过 ④（主链路不支持复检），只告警。
type DispatchDeps struct {
	Adapter adapter.DeviceAdapter
	Recheck Prechecker
	Store   DispatchStore
	Tx      TxRunner
	Cards   AdviceReader
	Epochs  EpochAllocator
	Locks   KeyLocker
	Keys    rediskey.Keys
	Logger  *slog.Logger
}

func (d DispatchDeps) validate() error {
	switch {
	case d.Adapter == nil, d.Store == nil, d.Tx == nil, d.Locks == nil, d.Logger == nil:
		return fmt.Errorf("%w: adapter / store / tx / locks / logger required", ErrDispatchNotConfigured)
	case d.Recheck != nil && (d.Cards == nil || d.Epochs == nil):
		return fmt.Errorf("%w: recheck needs cards and epochs", ErrDispatchNotConfigured)
	}
	return nil
}

// SetDispatch 装配下发器依赖（app 在 Recheck / Adapter 就绪后调用）。
func (s *WorkOrderService) SetDispatch(d DispatchDeps) error {
	if err := d.validate(); err != nil {
		return err
	}
	s.dispatch = &d
	return nil
}

// dispatchMessage 是 order.dispatch 的消息体（DecisionService 审批通过时写入）。
type dispatchMessage struct {
	OrderID  uint64 `json:"order_id"`
	TicketID uint64 `json:"ticket_id"`
	RunID    uint64 `json:"run_id"`
}

// dispatchParams 是 dispatch_result.params：预期作业参数（work_order.payload 与时间窗）。
type dispatchParams struct {
	WorkType   model.WorkType   `json:"work_type"`
	DeviceType string           `json:"device_type"`
	Payload    json.RawMessage  `json:"payload"`
	TimeWindow model.TimeWindow `json:"time_window"`
}

// Dispatch 处理一条 order.dispatch（P0 §6.9 流程 ①4，母文档 §4.6.2-3）。返回 nil 即 outbox 可置 DONE
// （⑦ 已在同事务置过时轮询器的 DONE 影响 0 行）；返回错误由轮询器退避重试。
// ① 读单：终态视为重复投递；DISPATCHED 无结果从 ⑥ 续跑；过 dispatch_deadline_at 同事务 APPROVED → EXPIRED。
// ② P0 只接受 DRY_RUN。④ 锁外复检（结论变化：原单已取消、新 run 已建，写旧 run superseded_by_run_id）。
// ⑤ 持 lock:order 同事务 APPROVED → DISPATCHED + DISPATCHED(INTENT)；⑥ DryRun；⑦ 同事务写结果 → DONE → outbox DONE →
// DISPATCHED(RESULT)。不写 episodic_memory、不回写 field.history。
func (s *WorkOrderService) Dispatch(ctx context.Context, msg repository.OutboxMessage) error {
	d := s.dispatch
	if d == nil {
		return ErrDispatchNotConfigured
	}
	var m dispatchMessage
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		return fmt.Errorf("%w: outbox %d: %w", ErrBadOutboxPayload, msg.ID, err)
	}
	if m.OrderID == 0 || m.OrderID != msg.AggregateID {
		return fmt.Errorf("%w: outbox %d order %d aggregate %d", ErrBadOutboxPayload, msg.ID, m.OrderID, msg.AggregateID)
	}
	ref := model.OrderRef{TenantID: msg.TenantID, FarmID: msg.FarmID, OrderID: m.OrderID}
	o, err := d.Store.OrderForDispatch(ctx, nil, ref)
	if err != nil {
		return fmt.Errorf("service: dispatch: %w", err)
	}
	if m.RunID != 0 && m.RunID != o.RunID {
		return fmt.Errorf("%w: outbox %d run %d, order run %d", ErrBadOutboxPayload, msg.ID, m.RunID, o.RunID)
	}
	st := model.WorkOrderStatus(o.Status)
	switch {
	case st.IsTerminal():
		d.Logger.InfoContext(ctx, "dispatch: duplicate delivery", "order_id", o.ID, "status", o.Status, "outbox_id", msg.ID)
		return nil
	case st == model.OrderDispatched && !o.HasResult:
		d.Logger.WarnContext(ctx, "dispatch: resume after crash", "order_id", o.ID, "outbox_id", msg.ID)
		return s.lockedDispatch(ctx, msg, m, ref, false)
	case st == model.OrderPendingApproval:
		// 复检退回重审后会另开票、另写消息，本条作废
		d.Logger.WarnContext(ctx, "dispatch: order back to approval, message dropped", "order_id", o.ID, "outbox_id", msg.ID)
		return nil
	case st != model.OrderApproved:
		return fmt.Errorf("%w: order %d is %s (has_result=%t)", ErrDispatchState, o.ID, o.Status, o.HasResult)
	case model.DispatchMode(o.DispatchMode) != model.DispatchDryRun:
		return fmt.Errorf("%w: order %d dispatch_mode %s, P0 only %s", ErrDispatchState, o.ID, o.DispatchMode, model.DispatchDryRun)
	case s.overdue(o):
		return s.expire(ctx, msg, ref)
	}
	done, err := s.precheck(ctx, o)
	if err != nil || done {
		return err
	}
	return s.lockedDispatch(ctx, msg, m, ref, true)
}

// overdue 判断是否已过下发截止时间。
func (s *WorkOrderService) overdue(o repository.DispatchOrder) bool {
	return o.DispatchDeadlineAt != nil && !s.clock.Now().Before(*o.DispatchDeadlineAt)
}

// expire 持锁同事务 APPROVED → EXPIRED(APPROVAL_EXPIRED) 并置 outbox DONE（与 approval_expire 扫描同一迁移）。
func (s *WorkOrderService) expire(ctx context.Context, msg repository.OutboxMessage, ref model.OrderRef) error {
	d := s.dispatch
	release, err := acquireOrderLock(ctx, d.Locks, d.Keys, d.Logger, ref.OrderID)
	if err != nil {
		return err
	}
	defer release()
	err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
		if err := s.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderExpired,
			model.Actor{Type: model.ActorSystem}, model.ReasonApprovalExpired); err != nil {
			return err
		}
		return s.markDoneTx(ctx, tx, msg.ID, ref.OrderID)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch expire order %d: %w", ref.OrderID, err)
	}
	d.Logger.InfoContext(ctx, "dispatch: order expired", "order_id", ref.OrderID, "outbox_id", msg.ID)
	return nil
}

// precheck 是 ④ 锁外复检；返回 true 表示原单已因结论变化取消（消息处理完毕）。
func (s *WorkOrderService) precheck(ctx context.Context, o repository.DispatchOrder) (bool, error) {
	d := s.dispatch
	if d.Recheck == nil {
		d.Logger.WarnContext(ctx, "dispatch: recheck unavailable, skipped", "order_id", o.ID, "run_id", o.RunID)
		return false, nil
	}
	ref := o.Ref()
	scope := model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}
	src, err := d.Store.RecheckSource(ctx, scope, o.RunID)
	if err != nil {
		return false, fmt.Errorf("service: dispatch recheck order %d: %w", o.ID, err)
	}
	card, err := d.Cards.GetByRun(ctx, scope, o.RunID)
	if err != nil {
		return false, fmt.Errorf("service: dispatch recheck order %d card: %w", o.ID, err)
	}
	epoch, err := d.Epochs.RecheckEpoch(ctx, scope, o.RunID)
	if err != nil {
		return false, fmt.Errorf("service: dispatch recheck order %d epoch: %w", o.ID, err)
	}
	scope.UserID = src.UserID
	q := agent.Question{Text: src.Question, Scope: scope, FieldID: src.FieldID, RunID: o.RunID}
	res, err := d.Recheck.PreDispatch(ctx, PreDispatchInput{Order: ref, Question: q, Card: card, Epoch: epoch})
	if err != nil {
		return false, fmt.Errorf("service: dispatch: %w", err)
	}
	if err := d.Store.InsertPreDispatchChecks(ctx, ref, epoch, res.Checks, s.clock.Now()); err != nil {
		if !res.Changed {
			return false, fmt.Errorf("service: dispatch: %w", err)
		}
		// 原单已取消，校验记录写失败不影响处置结论
		d.Logger.ErrorContext(ctx, "dispatch: record PRE_DISPATCH checks", "order_id", o.ID, "err", err)
	}
	if !res.Changed {
		return false, nil
	}
	if res.NewRunID != 0 {
		ok, err := d.Store.SupersedeRun(ctx, scope, o.RunID, res.NewRunID, s.clock.Now())
		if err != nil {
			return true, fmt.Errorf("service: dispatch: %w", err)
		}
		if !ok {
			d.Logger.WarnContext(ctx, "dispatch: run already superseded", "run_id", o.RunID, "new_run_id", res.NewRunID)
		}
	}
	d.Logger.InfoContext(ctx, "dispatch: canceled by recheck", "order_id", o.ID, "new_run_id", res.NewRunID, "detail", res.Detail)
	return true, nil
}

// lockedDispatch 持 lock:order 执行 ⑤（intent=true 时）→ ⑥ → ⑦。⑤ 前在锁内重读，防止复检期间状态已变。
func (s *WorkOrderService) lockedDispatch(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	ref model.OrderRef, intent bool) error {
	d := s.dispatch
	release, err := acquireOrderLock(ctx, d.Locks, d.Keys, d.Logger, ref.OrderID)
	if err != nil {
		return err
	}
	defer release()
	o, err := d.Store.OrderForDispatch(ctx, nil, ref)
	if err != nil {
		return fmt.Errorf("service: dispatch: %w", err)
	}
	if intent {
		if done, err := s.recordIntent(ctx, msg, m, o); err != nil || done {
			return err
		}
	} else if model.WorkOrderStatus(o.Status) != model.OrderDispatched || o.HasResult {
		return fmt.Errorf("%w: order %d changed to %s before resume", ErrDispatchState, o.ID, o.Status)
	}
	out, err := d.Adapter.DryRun(ctx, o.WorkOrder())
	if err != nil {
		return fmt.Errorf("service: dispatch dry-run order %d: %w", o.ID, err)
	}
	return s.recordResult(ctx, msg, m, ref, o, out)
}

// recordIntent 是 ⑤：锁内复核仍为 APPROVED 且未过期，同事务 APPROVED → DISPATCHED + DISPATCHED(INTENT)。
// 复核时已终态返回 done=true；过期转 EXPIRED 收尾。
func (s *WorkOrderService) recordIntent(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	o repository.DispatchOrder) (bool, error) {
	d := s.dispatch
	ref := o.Ref()
	st := model.WorkOrderStatus(o.Status)
	switch {
	case st.IsTerminal():
		return true, nil
	case st != model.OrderApproved:
		return false, fmt.Errorf("%w: order %d changed to %s before dispatch", ErrDispatchState, o.ID, o.Status)
	case s.overdue(o):
		return true, d.Tx.InTx(ctx, func(tx *repository.DB) error {
			if err := s.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderExpired,
				model.Actor{Type: model.ActorSystem}, model.ReasonApprovalExpired); err != nil {
				return fmt.Errorf("service: dispatch expire order %d: %w", o.ID, err)
			}
			return s.markDoneTx(ctx, tx, msg.ID, o.ID)
		})
	}
	err := d.Tx.InTx(ctx, func(tx *repository.DB) error {
		if err := s.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderDispatched,
			model.Actor{Type: model.ActorSystem}, ""); err != nil {
			return err
		}
		return s.appendDispatched(ctx, tx, o, m, model.DispatchPhaseIntent)
	})
	if err != nil {
		return false, fmt.Errorf("service: dispatch intent order %d: %w", o.ID, err)
	}
	return false, nil
}

// recordResult 是 ⑦：同事务写 dispatch_result → DISPATCHED → DONE（跳过 EXECUTING）→ outbox DONE → DISPATCHED(RESULT)。
func (s *WorkOrderService) recordResult(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	ref model.OrderRef, o repository.DispatchOrder, out adapter.DryRunResult) error {
	d := s.dispatch
	params, err := json.Marshal(dispatchParams{WorkType: model.WorkType(o.WorkType), DeviceType: o.DeviceType,
		Payload: o.Payload, TimeWindow: model.TimeWindow{From: o.TimeWindowFrom, To: o.TimeWindowTo}})
	if err != nil {
		return fmt.Errorf("service: dispatch params order %d: %w", o.ID, err)
	}
	now := s.clock.Now()
	result, err := json.Marshal(model.DryRunDispatchResult{DryRun: true, DeviceTouched: false, Adapter: adapterName(d.Adapter),
		Commands: out.Commands, Params: params, SimulatedAt: now})
	if err != nil {
		return fmt.Errorf("service: dispatch result order %d: %w", o.ID, err)
	}
	err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
		cur, err := d.Store.OrderForDispatch(ctx, tx, ref)
		if err != nil {
			return err
		}
		if err := d.Store.SetDispatchResultTx(ctx, tx, ref, result, cur.RowVer, now); err != nil {
			return err
		}
		if err := s.Transition(ctx, tx, ref, model.OrderDone, model.Actor{Type: model.ActorSystem}, ""); err != nil {
			return err
		}
		if err := s.markDoneTx(ctx, tx, msg.ID, o.ID); err != nil {
			return err
		}
		return s.appendDispatched(ctx, tx, o, m, model.DispatchPhaseResult)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch result order %d: %w", o.ID, err)
	}
	d.Logger.InfoContext(ctx, "dispatch: dry-run done", "order_id", o.ID, "run_id", o.RunID, "outbox_id", msg.ID)
	return nil
}

// appendDispatched 追加 DISPATCHED 事件（EventRepo 同事务投影 audit DISPATCH，summary 即本 payload，含 dry_run=true）。
func (s *WorkOrderService) appendDispatched(ctx context.Context, tx *repository.DB, o repository.DispatchOrder,
	m dispatchMessage, phase string) error {
	payload, err := json.Marshal(model.DispatchedPayload{OrderID: o.ID, TicketID: m.TicketID, DispatchMode: model.DispatchDryRun,
		DryRun: true, DeviceTouched: false, Phase: phase})
	if err != nil {
		return fmt.Errorf("service: encode DISPATCHED payload: %w", err)
	}
	if _, err := s.events.AppendTx(ctx, tx, model.RunEvent{TenantID: o.TenantID, FarmID: o.FarmID, RunID: o.RunID,
		Type: model.EventDispatched, Actor: model.ActorSystem, Payload: payload, CreatedAt: s.clock.Now()}); err != nil {
		return fmt.Errorf("service: DISPATCHED %s order %d: %w", phase, o.ID, err)
	}
	return nil
}

// markDoneTx 同事务置 outbox DONE；已非 PENDING（并发处理已置过）只告警。
func (s *WorkOrderService) markDoneTx(ctx context.Context, tx *repository.DB, outboxID, orderID uint64) error {
	ok, err := s.dispatch.Store.MarkDoneTx(ctx, tx, outboxID, s.clock.Now())
	if err != nil {
		return err
	}
	if !ok {
		s.dispatch.Logger.WarnContext(ctx, "dispatch: outbox not PENDING", "outbox_id", outboxID, "order_id", orderID)
	}
	return nil
}

// adapterName 取 Adapter 的 Name()，未实现返回 custom。
func adapterName(a adapter.DeviceAdapter) string {
	if n, ok := a.(interface{ Name() string }); ok {
		return n.Name()
	}
	return adapterNameCustom
}
