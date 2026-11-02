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

// 回执超时扫描与人工对账（§6.5 L4-B 第 4、5 条）。红线：扫描只 QueryStatus 与置 NO_RECEIPT，不转 FAILED、不重发。

// pendingReconcileLimit 是待对账列表单次最多返回的条数。
const pendingReconcileLimit = 100

// RunReceiptDue 每 ScanInterval 跑一轮回执超时扫描，直到 ctx 取消（cmd/worker 起 goroutine）。
func (s *OrderOpsService) RunReceiptDue(ctx context.Context) {
	t := time.NewTicker(s.d.ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.ScanReceiptDueOnce(ctx); err != nil {
				s.d.Logger.WarnContext(ctx, "receipt due scan round", "err", err)
			}
		}
	}
}

// ScanReceiptDueOnce 持 lock:scan:receipt_due 跑一轮（已被其他实例持有即跳过）：对每条过 receipt_due_at 的在途尝试
// 主动 QueryStatus，有前进结果交 Process（source=POLL）；查询失败、结果非法或不前进 → 尝试置 NO_RECEIPT / UNKNOWN 进待对账。
// 单条失败不影响其余（并发 / 锁占用类错误跳过，下一轮重试）。
func (s *OrderOpsService) ScanReceiptDueOnce(ctx context.Context) error {
	lease, err := s.d.Locks.AcquireLease(ctx, s.d.Keys.LockScan(receiptDueScope.Task()), scanLockTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: receipt due scan lock: %w", err)
	}
	defer func() {
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			s.d.Logger.WarnContext(ctx, "release receipt due scan lock", "err", err)
		}
	}()
	due, err := s.d.Lookup.DueAttempts(ctx, receiptDueScope, s.d.Clock.Now())
	if err != nil {
		return fmt.Errorf("service: receipt due scan: %w", err)
	}
	var errs []error
	for _, d := range due {
		if err := s.pollAttempt(ctx, d); err != nil && !skippable(err) {
			errs = append(errs, fmt.Errorf("attempt %d: %w", d.ID, err))
		}
	}
	return errors.Join(errs...)
}

// inFlight 判断尝试仍在途（扫描只处理这些状态，与 DueAttempts / MarkNoReceiptTx 的 status IN 一致）。
func inFlight(s model.AttemptStatus) bool {
	return s == model.AttemptSent || s == model.AttemptAccepted || s == model.AttemptExecuting
}

// pollAttempt 处理一条到期尝试。
func (s *OrderOpsService) pollAttempt(ctx context.Context, d repository.DueAttempt) error {
	ref := d.Ref()
	var att model.DispatchAttempt
	var o repository.DispatchOrder
	if err := s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		var err error
		if att, err = s.d.Store.AttemptTx(ctx, tx, ref, d.ID); err != nil {
			return err
		}
		o, err = s.d.Store.OrderForDispatch(ctx, tx, ref)
		return err
	}); err != nil {
		return fmt.Errorf("service: read due attempt: %w", err)
	}
	if !inFlight(att.Status) {
		return nil
	}
	target := DeviceTarget{DeviceType: att.DeviceType, Ref: ref, RunID: o.RunID}
	r, qerr := s.d.Devices.QueryStatus(ctx, target, att.IdemKey, att.DeviceJobID)
	if qerr == nil && r.DeviceJobID == "" {
		r.DeviceJobID = att.DeviceJobID // 查询按任务号发起，结果可不带
	}
	reason := ""
	switch {
	case qerr != nil:
		reason = "query status: " + qerr.Error()
	case r.Validate() != nil:
		reason = "query status: invalid receipt: " + r.Validate().Error()
	default:
		res, err := s.Process(ctx, ReceiptInput{Ref: ref, AttemptID: att.ID, ReceiptID: ReceiptIDOf("", model.SourcePoll, r),
			Receipt: r, Source: model.SourcePoll})
		if err != nil {
			return err
		}
		if res.Outcome == model.OutcomeApplied || r.Status.IsFinal() {
			return nil
		}
		reason = "query status: no progress (" + string(r.Status) + ")"
	}
	return s.markNoReceipt(ctx, ref, att.ID, reason)
}

// markNoReceipt 持 lock:order 把仍在途的尝试置 NO_RECEIPT / UNKNOWN：不改工单（保持 DISPATCHED / EXECUTING）、不写 outbox（不重发），告警。
func (s *OrderOpsService) markNoReceipt(ctx context.Context, ref model.OrderRef, attemptID uint64, reason string) error {
	marked := false
	if err := WithOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, ref.OrderID, func(lctx context.Context) error {
		return s.d.Tx.InTx(lctx, func(tx *repository.DB) error {
			att, err := s.d.Store.AttemptTx(lctx, tx, ref, attemptID)
			if err != nil {
				return err
			}
			if !inFlight(att.Status) { // 加锁前已有回执推进
				return nil
			}
			marked = true
			return s.d.Store.MarkNoReceiptTx(lctx, tx, ref, att.ID, att.RowVer, s.d.Clock.Now())
		})
	}); err != nil {
		return fmt.Errorf("service: mark attempt %d NO_RECEIPT: %w", attemptID, err)
	}
	if marked {
		s.d.Logger.ErrorContext(ctx, "receipt timeout, attempt pending reconcile", "alert", alertNoReceipt,
			"order_id", ref.OrderID, "attempt_id", attemptID, "reason", reason)
	}
	return nil
}

// PendingReconcile 列出本农场待对账的尝试（NO_RECEIPT、FAILED+UNKNOWN；按 Scope）。
func (s *OrderOpsService) PendingReconcile(ctx context.Context, scope model.Scope) ([]model.DispatchAttempt, error) {
	rows, err := s.d.Store.PendingReconcile(ctx, scope, pendingReconcileLimit)
	if err != nil {
		return nil, fmt.Errorf("service: pending reconcile: %w", err)
	}
	return rows, nil
}

// ReconcileInput 是人工对账结论：Conclusion 用尝试状态表达（DONE / PARTIALLY_DONE / CANCELED / FAILED+NOT_EXECUTED|IN_EXECUTION）。
type ReconcileInput struct {
	OrderID     uint64              `json:"-"`
	Conclusion  model.AttemptStatus `json:"conclusion"`
	FailureKind model.FailureKind   `json:"failure_kind,omitempty"`
	DoneAreaMu  *float64            `json:"done_area_mu,omitempty"`
	Comment     string              `json:"comment,omitempty"`
}

// ReconcileResult 是对账结果。
type ReconcileResult struct {
	OrderID     uint64                `json:"order_id"`
	AttemptID   uint64                `json:"attempt_id"`
	OrderStatus model.WorkOrderStatus `json:"order_status"`
}

// maxReconcileComment 是对账备注的最大字符数。
const maxReconcileComment = 200

// target 校验结论并返回工单终态；结论必须能定案（FAILED+UNKNOWN、在途状态都不行）。
func (in ReconcileInput) target() (model.WorkOrderStatus, error) {
	if in.Conclusion == model.AttemptFailed && in.FailureKind == model.FailureUnknown {
		return "", fmt.Errorf("%w: reconcile conclusion FAILED needs NOT_EXECUTED or IN_EXECUTION", ErrBadInput)
	}
	if err := model.CheckAttemptState(in.Conclusion, in.FailureKind, in.DoneAreaMu); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if in.Conclusion == model.AttemptPartiallyDone && !hasArea(in.DoneAreaMu) {
		return "", fmt.Errorf("%w: PARTIALLY_DONE needs done_area_mu > 0", ErrBadInput)
	}
	if len([]rune(in.Comment)) > maxReconcileComment {
		return "", fmt.Errorf("%w: comment longer than %d", ErrBadInput, maxReconcileComment)
	}
	to := receiptTarget(model.DeviceReceipt{Status: in.Conclusion, FailureKind: in.FailureKind, DoneAreaMu: in.DoneAreaMu})
	if to == "" {
		return "", fmt.Errorf("%w: reconcile conclusion %q is not final", ErrBadInput, in.Conclusion)
	}
	return to, nil
}

// reconcileAudit 是 RECONCILE 审计的 payload。
type reconcileAudit struct {
	OrderID     uint64                `json:"order_id"`
	AttemptID   uint64                `json:"attempt_id"`
	From        model.AttemptStatus   `json:"from"`
	Conclusion  model.AttemptStatus   `json:"conclusion"`
	FailureKind model.FailureKind     `json:"failure_kind,omitempty"`
	DoneAreaMu  *float64              `json:"done_area_mu,omitempty"`
	OrderStatus model.WorkOrderStatus `json:"order_status"`
	Comment     string                `json:"comment,omitempty"`
}

// Reconcile 人工对账（farm_manager / platform_admin，权限由路由层判定）：持 lock:order 单事务——
// 最后一次尝试须在待对账列表（否则 ErrReconcileState）→ 尝试写结论 → Transition（actor=user、RECONCILED），
// REAL 单进入 DONE / PARTIALLY_DONE 同事务回写 → audit_log(RECONCILE)。
func (s *OrderOpsService) Reconcile(ctx context.Context, scope model.Scope, in ReconcileInput) (ReconcileResult, error) {
	if scope.TenantID == 0 || scope.FarmID == 0 || in.OrderID == 0 {
		return ReconcileResult{}, fmt.Errorf("%w: reconcile needs tenant / farm / order", ErrBadInput)
	}
	to, err := in.target()
	if err != nil {
		return ReconcileResult{}, err
	}
	ref := model.OrderRef{TenantID: scope.TenantID, FarmID: scope.FarmID, OrderID: in.OrderID}
	var out ReconcileResult
	err = WithOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, ref.OrderID, func(lctx context.Context) error {
		var lerr error
		out, lerr = s.reconcileLocked(lctx, scope, ref, in, to)
		return lerr
	})
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("service: reconcile order %d: %w", in.OrderID, err)
	}
	return out, nil
}

// reconcileLocked 是 Reconcile 的落库部分（调用方已持 lock:order）。
func (s *OrderOpsService) reconcileLocked(ctx context.Context, scope model.Scope, ref model.OrderRef, in ReconcileInput,
	to model.WorkOrderStatus) (ReconcileResult, error) {
	actor := model.Actor{Type: model.ActorUser, ID: scope.UserID}
	out := ReconcileResult{OrderID: in.OrderID}
	err := s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		o, err := s.d.Store.OrderForDispatch(ctx, tx, ref)
		if err != nil {
			return scopedOrderErr(err)
		}
		att, err := s.d.Store.LatestAttemptTx(ctx, tx, ref)
		if err != nil {
			return err
		}
		if model.WorkOrderStatus(o.Status).IsTerminal() || !model.AwaitingReconcile(att.Status, att.FailureKind) {
			return fmt.Errorf("%w: order %d is %s, attempt %d is %s/%s", ErrReconcileState, o.ID, o.Status, att.ID,
				att.Status, att.FailureKind)
		}
		now := s.d.Clock.Now()
		if err := s.d.Store.UpdateReceiptTx(ctx, tx, ref, att.ID, att.RowVer, in.Conclusion, in.FailureKind, in.DoneAreaMu,
			att.ReceiptDueAt, now); err != nil {
			return err
		}
		area := in.DoneAreaMu
		if area == nil {
			area = att.DoneAreaMu
		}
		res, err := s.transitionTx(ctx, tx, o, att, ReceiptResult{OrderStatus: model.WorkOrderStatus(o.Status)}, to, actor,
			model.ReasonReconciled, area)
		if err != nil {
			return err
		}
		out.AttemptID, out.OrderStatus = att.ID, res.OrderStatus
		payload, err := json.Marshal(reconcileAudit{OrderID: o.ID, AttemptID: att.ID, From: att.Status, Conclusion: in.Conclusion,
			FailureKind: in.FailureKind, DoneAreaMu: in.DoneAreaMu, OrderStatus: res.OrderStatus, Comment: in.Comment})
		if err != nil {
			return fmt.Errorf("service: encode RECONCILE: %w", err)
		}
		runID := o.RunID
		return s.d.AuditTx.AppendTx(ctx, tx, repository.NewAuditRecord(scope, &runID, actor, model.AuditReconcile,
			"order:"+strconv.FormatUint(o.ID, 10), payload, model.AuditResultOK, now))
	})
	return out, err
}
