package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"agri-steward/internal/adapter"
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
// outbox 置 DONE 由轮询器带租约 row_ver 完成（P1 §6.5 技术实现第 2 步），下发器不再自己置 DONE。
type DispatchStore interface {
	OrderForDispatch(ctx context.Context, db *repository.DB, ref model.OrderRef) (repository.DispatchOrder, error)
	RecheckSource(ctx context.Context, scope model.Scope, runID uint64) (repository.RecheckSource, error)
	SupersedeRun(ctx context.Context, scope model.Scope, oldRunID, newRunID uint64, now time.Time) (bool, error)
	InsertPreDispatchChecks(ctx context.Context, ref model.OrderRef, attempt int, checks []model.ConstraintCheck, now time.Time) error
	SetDispatchResultTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, result []byte, rowVer int, now time.Time) error
	BindDeviceTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, deviceID string, rowVer int, now time.Time) error
}

// AttemptStore 是 dispatch_attempt 台账（repository.DispatchAttemptRepo 实现）；*Tx 方法只在调用方事务内使用。
type AttemptStore interface {
	InsertSentTx(ctx context.Context, tx *repository.DB, a model.DispatchAttempt) (uint64, error)
	SetJobTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, id uint64, rowVer int, rc model.DeviceReceipt,
		receiptDueAt, now time.Time) error
	UpdateReceiptTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, id uint64, rowVer int,
		status model.AttemptStatus, kind model.FailureKind, doneAreaMu *float64, receiptDueAt, now time.Time) error
	RefreshSentTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, id uint64, rowVer int,
		flagReadAt, sentAt, receiptDueAt time.Time) error
	ByIdemKey(ctx context.Context, tx *repository.DB, scope model.Scope, idemKey string) (model.DispatchAttempt, error)
}

// DeviceRegistry 按 device_type 取真实 Adapter（*adapter.Registry 实现，登记的是 adapter.Traced 装饰后的实现）。
type DeviceRegistry interface {
	Get(deviceType string) (adapter.DeviceAdapter, error)
}

// DispatchIdem 是 Redis idem:dispatch 键写入（repository.RedisIdem 实现）。
type DispatchIdem interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
}

// Prechecker 是下发前复检，RecheckService 实现。
type Prechecker interface {
	PreDispatch(ctx context.Context, in PreDispatchInput) (PreDispatchResult, error)
}

// EpochAllocator 为复检分配新 epoch（resume_epoch+1 并持久化），repository.RunRepo 实现。
type EpochAllocator interface {
	RecheckEpoch(ctx context.Context, scope model.Scope, runID uint64) (int, error)
}

// LiveFlagReader 直读开关（不走缓存），FlagService 实现。
type LiveFlagReader interface {
	BoolLive(ctx context.Context, key model.FlagKey, scope model.Scope) (bool, error)
}

// DispatchDeps 是 WorkOrderService.Dispatch 的依赖。
//   - Adapter：DRY_RUN 单的 DryRunner；Devices / Attempts / Idem：REAL 单的真实通道（P1-L4-A）。
//   - Picker：第 ③ 步选设备；Epochs：③④ 共用的复检 epoch。
//   - Recheck 为 nil 时 DRY_RUN 单跳过 ④ 并告警，REAL 单失败关闭；非 nil 时须有 Cards 与 Reopener（④ 不通过退回）。
//   - MaxAttempts：order.dispatch 的 attempt 上限（config dispatch.max_attempts）；ReceiptTimeout：回执时限，写 receipt_due_at；
//     DeviceWaitMax：选不到设备的最长等待（config dispatch.device_wait_max，单无 dispatch_deadline_at 时生效）。
//   - Notify / OfflineTextKey：设备离线通知（P1 L6，经 L5 / L6 共用接缝写 notify.send outbox；文案 key 取自 fault_matrix）。
//   - Tickets：设备调用前重读审批票（P1 L7-B 审批前设备调用拦截，必填）；Alerts：P0 告警出口，nil 时用 LogAlertSink；
//     Observer：下发结果指标 as_dispatch_total（可选，nil 不计）。
type DispatchDeps struct {
	Adapter        adapter.DryRunner
	Devices        DeviceRegistry
	Attempts       AttemptStore
	Idem           DispatchIdem
	Picker         DevicePicker
	Recheck        Prechecker
	Reopener       OrderReopener
	Store          DispatchStore
	Tx             TxRunner
	Cards          AdviceReader
	Epochs         EpochAllocator
	Locks          KeyLocker
	Keys           rediskey.Keys
	Flags          LiveFlagReader
	Audit          AuditTxAppender
	Logger         *slog.Logger
	MaxAttempts    int
	ReceiptTimeout time.Duration
	DeviceWaitMax  time.Duration
	Notify         NotifyOutboxWriter
	OfflineTextKey string
	Tickets        DispatchTicketReader
	Alerts         model.AlertSink
	Observer       Observer
}

func (d DispatchDeps) validate() error {
	switch {
	case d.Adapter == nil, d.Store == nil, d.Tx == nil, d.Locks == nil, d.Flags == nil, d.Audit == nil, d.Logger == nil:
		return fmt.Errorf("%w: adapter / store / tx / locks / flags / audit / logger required", ErrDispatchNotConfigured)
	case d.Devices == nil, d.Attempts == nil, d.Idem == nil, d.Picker == nil, d.Epochs == nil:
		return fmt.Errorf("%w: devices / attempts / idem / picker / epochs required", ErrDispatchNotConfigured)
	case d.Recheck != nil && (d.Cards == nil || d.Reopener == nil):
		return fmt.Errorf("%w: recheck needs cards and reopener", ErrDispatchNotConfigured)
	case d.MaxAttempts < 1 || d.ReceiptTimeout <= 0 || d.DeviceWaitMax <= 0:
		return fmt.Errorf("%w: max_attempts >= 1, receipt_timeout > 0 and device_wait_max > 0 required", ErrDispatchNotConfigured)
	case d.Notify == nil || d.OfflineTextKey == "":
		return fmt.Errorf("%w: notify and offline text key required", ErrDispatchNotConfigured)
	case d.Tickets == nil:
		return fmt.Errorf("%w: tickets required (pre-approval device guard)", ErrDispatchNotConfigured)
	}
	return nil
}

// SetDispatch 装配下发器依赖（app 在 Recheck / Adapter 就绪后调用）。
func (s *WorkOrderService) SetDispatch(d DispatchDeps) error {
	if err := d.validate(); err != nil {
		return err
	}
	if d.Alerts == nil {
		d.Alerts = LogAlertSink{Logger: d.Logger}
	}
	if d.Observer == nil {
		d.Observer = NopObserver{}
	}
	s.dispatch = &d
	return nil
}

// dispatchMessage 是 order.dispatch 的消息体：DecisionService 审批通过时写 attempt 缺省（= 1）的首次消息，
// L4-B 在 NOT_EXECUTED 回执事务里写 attempt = n+1 的重试消息（P1-L4 接缝 2）。
type dispatchMessage struct {
	OrderID  uint64 `json:"order_id"`
	TicketID uint64 `json:"ticket_id"`
	RunID    uint64 `json:"run_id"`
	Attempt  int    `json:"attempt,omitempty"`
}

// DispatchIdemKey 是 order.dispatch 的 dedup_key = 下发 idem_key：order.dispatch:{order_id}:{ticket_id}:{attempt}。
func DispatchIdemKey(orderID, ticketID uint64, attempt int) string {
	return fmt.Sprintf("%s:%d:%d:%d", repository.TopicOrderDispatch, orderID, ticketID, attempt)
}

// parseDispatch 解析并校验消息：attempt 缺省 1、须在 [1, MaxAttempts]；dedup_key 非空时须与 (order, ticket, attempt) 一致。
func (s *WorkOrderService) parseDispatch(msg repository.OutboxMessage) (dispatchMessage, error) {
	var m dispatchMessage
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		return m, fmt.Errorf("%w: outbox %d: %w", ErrBadOutboxPayload, msg.ID, err)
	}
	if m.OrderID == 0 || m.OrderID != msg.AggregateID {
		return m, fmt.Errorf("%w: outbox %d order %d aggregate %d", ErrBadOutboxPayload, msg.ID, m.OrderID, msg.AggregateID)
	}
	if m.Attempt == 0 {
		m.Attempt = 1
	}
	if m.Attempt < 1 || m.Attempt > s.dispatch.MaxAttempts {
		return m, fmt.Errorf("%w: outbox %d attempt %d not in [1, %d]", ErrBadOutboxPayload, msg.ID, m.Attempt,
			s.dispatch.MaxAttempts)
	}
	if msg.DedupKey != "" && msg.DedupKey != DispatchIdemKey(m.OrderID, m.TicketID, m.Attempt) {
		return m, fmt.Errorf("%w: outbox %d dedup_key %q does not match payload", ErrBadOutboxPayload, msg.ID, msg.DedupKey)
	}
	return m, nil
}

// dispatchParams 是 dispatch_result.params：预期作业参数（work_order.payload 与时间窗）。
type dispatchParams struct {
	WorkType   model.WorkType   `json:"work_type"`
	DeviceType string           `json:"device_type"`
	Payload    json.RawMessage  `json:"payload"`
	TimeWindow model.TimeWindow `json:"time_window"`
}

// Dispatch 处理一条 order.dispatch（P1 §6.5 技术实现第 3、4 条，母文档 §4.6.2-3）。返回 nil 即轮询器带租约置 outbox DONE；
// 包装 ErrOutboxDeferred / ErrOrderBusy 的错误推后 5s 不计 attempts；其余错误退避重试，耗尽 DEAD。
//   - REAL 单先按 idem_key 查 dispatch_attempt：已受理 / 已有结果 → 重复投递；SENT 无 device_job_id → 持锁续跑（resumeLocked）。
//   - ① 读单：attempt=1 要求 APPROVED（DRY_RUN 的 DISPATCHED 无结果从 ⑥ 续跑，过时效 → EXPIRED）；attempt>1 要求 REAL 且 DISPATCHED。
//   - ② DRY_RUN 不看开关；REAL 直读 enable_robot_dispatch，关 → 首次写 DISPATCH_HELD、重试 → FAILED(RETRY_BLOCKED)。
//   - ③ 选设备并绑定 work_order.payload.device_id（bindDevice；选不到推后，等待超限放弃）；④ 锁外复检（recheckGate）。
//   - ⑤-⑦ DRY_RUN：lockedDispatch（P0 dry-run 写法）；REAL：dispatchReal（Archive → InsertSentTx → DispatchArchived → 回填 job）。
//
// 重试在 ①②④ 被拦下一律 DISPATCHED → FAILED(RETRY_BLOCKED)（§6.5 第 8 步）。不写 episodic_memory、不回写 field.history。
func (s *WorkOrderService) Dispatch(ctx context.Context, msg repository.OutboxMessage) error {
	d := s.dispatch
	if d == nil {
		return ErrDispatchNotConfigured
	}
	m, err := s.parseDispatch(msg)
	if err != nil {
		return err
	}
	ref := model.OrderRef{TenantID: msg.TenantID, FarmID: msg.FarmID, OrderID: m.OrderID}
	o, err := d.Store.OrderForDispatch(ctx, nil, ref)
	if err != nil {
		return fmt.Errorf("service: dispatch: %w", err)
	}
	if m.RunID != 0 && m.RunID != o.RunID {
		return fmt.Errorf("%w: outbox %d run %d, order run %d", ErrBadOutboxPayload, msg.ID, m.RunID, o.RunID)
	}
	mode := model.DispatchMode(o.DispatchMode)
	if mode == model.DispatchReal {
		if msg.DedupKey == "" || m.TicketID == 0 {
			return fmt.Errorf("%w: outbox %d REAL order %d needs dedup_key and ticket_id", ErrBadOutboxPayload, msg.ID, o.ID)
		}
		if done, err := s.resumeAttempt(ctx, msg, o); err != nil || done {
			return err
		}
	}
	if done, err := s.admit(ctx, msg, m, o); err != nil || done {
		return err
	}
	flagReadAt := s.clock.Now()
	switch mode {
	case model.DispatchDryRun:
	case model.DispatchReal:
		on, err := d.Flags.BoolLive(ctx, model.FlagEnableRobotDispatch, model.Scope{TenantID: o.TenantID, FarmID: o.FarmID})
		if err != nil {
			return fmt.Errorf("service: dispatch order %d read flag: %w", o.ID, err)
		}
		if !on && m.Attempt > 1 {
			return s.failRetry(ctx, ref, m, "enable_robot_dispatch off")
		}
		if !on {
			return s.holdReal(ctx, msg, m, o)
		}
		if d.Recheck == nil {
			d.Logger.ErrorContext(ctx, "dispatch: REAL order but recheck unavailable", "alert", "REAL_DISPATCH_UNAVAILABLE",
				"order_id", o.ID, "outbox_id", msg.ID)
			return fmt.Errorf("%w: order %d is REAL, pre-dispatch recheck not configured", ErrDispatchState, o.ID)
		}
	default:
		return fmt.Errorf("%w: order %d dispatch_mode %q", ErrDispatchState, o.ID, o.DispatchMode)
	}
	epoch, err := d.Epochs.RecheckEpoch(ctx, model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}, o.RunID)
	if err != nil {
		return fmt.Errorf("service: dispatch order %d epoch: %w", o.ID, err)
	}
	if o, err = s.bindDevice(ctx, msg, o, m, epoch); errors.Is(err, errDeviceGaveUp) {
		return nil
	} else if err != nil {
		return err
	}
	if done, err := s.recheckGate(ctx, o, m, epoch); err != nil || done {
		return err
	}
	if mode == model.DispatchDryRun {
		return s.lockedDispatch(ctx, msg, m, ref, true)
	}
	return s.dispatchReal(ctx, msg, m, ref, flagReadAt)
}

// admit 是第 ① 步的状态要求，放行前 admitTicket 校验审批票（任何写入之前）；返回 true 表示消息已处理完毕。
func (s *WorkOrderService) admit(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	o repository.DispatchOrder) (bool, error) {
	d := s.dispatch
	ref := o.Ref()
	st, mode := model.WorkOrderStatus(o.Status), model.DispatchMode(o.DispatchMode)
	if st.IsTerminal() {
		d.Logger.InfoContext(ctx, "dispatch: duplicate delivery", "order_id", o.ID, "status", o.Status, "outbox_id", msg.ID)
		return true, nil
	}
	if m.Attempt > 1 {
		switch {
		case mode != model.DispatchReal || st != model.OrderDispatched:
			d.Logger.WarnContext(ctx, "dispatch: retry for non-dispatched order dropped", "order_id", o.ID,
				"status", o.Status, "dispatch_mode", o.DispatchMode, "attempt", m.Attempt, "outbox_id", msg.ID)
			return true, nil
		case s.overdue(o):
			return true, s.failRetry(ctx, ref, m, "dispatch deadline passed")
		}
		return s.admitTicket(ctx, o, m.TicketID)
	}
	switch {
	case st == model.OrderDispatched && !o.HasResult && mode == model.DispatchDryRun:
		d.Logger.WarnContext(ctx, "dispatch: resume after crash", "order_id", o.ID, "outbox_id", msg.ID)
		return true, s.lockedDispatch(ctx, msg, m, ref, false)
	case st == model.OrderPendingApproval:
		// 复检退回重审后会另开票、另写消息，本条作废
		d.Logger.WarnContext(ctx, "dispatch: order back to approval, message dropped", "order_id", o.ID, "outbox_id", msg.ID)
		return true, nil
	case st != model.OrderApproved:
		return true, fmt.Errorf("%w: order %d is %s (has_result=%t)", ErrDispatchState, o.ID, o.Status, o.HasResult)
	case s.overdue(o):
		return true, s.expire(ctx, ref)
	}
	return s.admitTicket(ctx, o, m.TicketID)
}

// lockedDispatch 是 DRY_RUN 单的 ⑤（intent=true 时）→ ⑥ → ⑦，持 lock:order 执行；⑤ 前在锁内重读，防止复检期间状态已变。
func (s *WorkOrderService) lockedDispatch(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	ref model.OrderRef, intent bool) error {
	d := s.dispatch
	return WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, ref.OrderID, func(lctx context.Context) error {
		o, err := d.Store.OrderForDispatch(lctx, nil, ref)
		if err != nil {
			return fmt.Errorf("service: dispatch: %w", err)
		}
		if intent {
			if done, err := s.recordIntent(lctx, m, o); err != nil || done {
				return err
			}
		} else if model.WorkOrderStatus(o.Status) != model.OrderDispatched || o.HasResult {
			return fmt.Errorf("%w: order %d changed to %s before resume", ErrDispatchState, o.ID, o.Status)
		}
		if o, err = s.guardDeviceCall(lctx, ref, deviceCallCheck{TicketID: m.TicketID, DryRun: true}); err != nil {
			return err
		}
		out, err := d.Adapter.DryRun(lctx, o.WorkOrder())
		if err != nil {
			return fmt.Errorf("service: dispatch dry-run order %d: %w", o.ID, err)
		}
		return s.recordResult(lctx, msg, m, ref, o, out)
	})
}

// recordIntent 是 DRY_RUN 单的 ⑤：锁内复核仍为 APPROVED 且未过期、审批票仍有效（admitTicket），同事务 APPROVED → DISPATCHED + DISPATCHED(INTENT)。
// 复核时已终态 / 票无效返回 done=true；过期转 EXPIRED 收尾。
func (s *WorkOrderService) recordIntent(ctx context.Context, m dispatchMessage, o repository.DispatchOrder) (bool, error) {
	d := s.dispatch
	ref := o.Ref()
	st := model.WorkOrderStatus(o.Status)
	switch {
	case st.IsTerminal():
		return true, nil
	case st != model.OrderApproved:
		return false, fmt.Errorf("%w: order %d changed to %s before dispatch", ErrDispatchState, o.ID, o.Status)
	case s.overdue(o):
		return true, s.expireLocked(ctx, ref)
	}
	if done, err := s.admitTicket(ctx, o, m.TicketID); err != nil || done {
		return done, err
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

// recordResult 是 DRY_RUN 单的 ⑦：同事务写 dispatch_result → DISPATCHED → DONE（跳过 EXECUTING）→ DISPATCHED(RESULT)。
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
		return s.appendDispatched(ctx, tx, o, m, model.DispatchPhaseResult)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch result order %d: %w", o.ID, err)
	}
	d.Observer.Dispatched(o.FarmID, model.DispatchDryRun, DispatchOK)
	d.Logger.InfoContext(ctx, "dispatch: dry-run done", "order_id", o.ID, "run_id", o.RunID, "outbox_id", msg.ID)
	return nil
}

// appendDispatched 追加 DRY_RUN 单的 DISPATCHED 事件（EventRepo 同事务投影 audit DISPATCH，summary 即本 payload，含 dry_run=true）。
func (s *WorkOrderService) appendDispatched(ctx context.Context, tx *repository.DB, o repository.DispatchOrder,
	m dispatchMessage, phase string) error {
	return s.appendEvent(ctx, tx, o, phase, model.DispatchedPayload{OrderID: o.ID, TicketID: m.TicketID,
		DispatchMode: model.DispatchDryRun, DryRun: true, DeviceTouched: false, Phase: phase})
}

// appendEvent 追加一条 DISPATCHED 事件。
func (s *WorkOrderService) appendEvent(ctx context.Context, tx *repository.DB, o repository.DispatchOrder, phase string,
	payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("service: encode DISPATCHED payload: %w", err)
	}
	if _, err := s.events.AppendTx(ctx, tx, model.RunEvent{TenantID: o.TenantID, FarmID: o.FarmID, RunID: o.RunID,
		Type: model.EventDispatched, Actor: model.ActorSystem, Payload: b, CreatedAt: s.clock.Now()}); err != nil {
		return fmt.Errorf("service: DISPATCHED %s order %d: %w", phase, o.ID, err)
	}
	return nil
}

// adapterName 取 Adapter 的 Name()，未实现返回 custom。
func adapterName(a adapter.DryRunner) string {
	if n, ok := a.(interface{ Name() string }); ok {
		return n.Name()
	}
	return adapterNameCustom
}

// ---- REAL 单真实通道（⑤-⑦ 与崩溃续跑） ----

// resumeAttempt 是 REAL 单进入 ① 前的幂等检查：按 idem_key（= dedup_key）查 dispatch_attempt。
// 不存在 → 继续正常流程；已有 device_job_id 或已非 SENT → 重复投递，返回 done；SENT 无 device_job_id → 持锁续跑。
func (s *WorkOrderService) resumeAttempt(ctx context.Context, msg repository.OutboxMessage,
	o repository.DispatchOrder) (bool, error) {
	d := s.dispatch
	scope := model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}
	att, err := d.Attempts.ByIdemKey(ctx, nil, scope, msg.DedupKey)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("service: dispatch order %d attempt by idem: %w", o.ID, err)
	case att.DeviceJobID != "" || att.Status != model.AttemptSent:
		d.Logger.InfoContext(ctx, "dispatch: attempt already settled, duplicate delivery", "order_id", o.ID,
			"attempt", att.Attempt, "status", att.Status, "outbox_id", msg.ID)
		return true, nil
	}
	return true, WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, o.ID, func(lctx context.Context) error {
		cur, err := d.Attempts.ByIdemKey(lctx, nil, scope, msg.DedupKey)
		if err != nil {
			return fmt.Errorf("service: dispatch order %d attempt by idem: %w", o.ID, err)
		}
		return s.resumeLocked(lctx, cur)
	})
}

// resumeLocked 续跑一条 SENT 尝试（调用方已持 lock:order）：设备支持 ExternalQuery 先 QueryStatus(idem_key)，查到即回填。
// 只有 caps.IdemKey 为真（平台按 idem_key 去重）才带原键与原 command_raw_id 重发；查无此单但不支持 IdemKey 时，
// 平台可能已受理而外部单号索引尚未可见，重发会产生二次作业（§6.5），故与两者都不支持一样 FAILED(UNKNOWN) 转人工，禁止重发。
// 重发前直读 enable_robot_dispatch（L8-B 审查 P0-2）：关 → FAILED(UNKNOWN) + DISPATCH_MANUAL，不调 Adapter；开 → refreshSent 后重发。
func (s *WorkOrderService) resumeLocked(ctx context.Context, att model.DispatchAttempt) error {
	d := s.dispatch
	if att.DeviceJobID != "" || att.Status != model.AttemptSent {
		return nil
	}
	o, err := d.Store.OrderForDispatch(ctx, nil, att.Ref())
	if err != nil {
		return fmt.Errorf("service: dispatch resume: %w", err)
	}
	if model.WorkOrderStatus(o.Status) != model.OrderDispatched {
		d.Logger.WarnContext(ctx, "dispatch: resume skipped, order not dispatched", "order_id", o.ID, "status", o.Status,
			"attempt", att.Attempt)
		return nil
	}
	dev, err := d.Devices.Get(att.DeviceType)
	if err != nil {
		return fmt.Errorf("service: dispatch resume order %d: %w", o.ID, err)
	}
	caps := dev.Capabilities()
	tctx := adapter.WithTraceOrder(ctx, traceOrder(o))
	if caps.ExternalQuery {
		rc, err := dev.QueryStatus(tctx, adapter.StatusRef{IdemKey: att.IdemKey})
		switch {
		case err == nil:
			d.Logger.WarnContext(ctx, "dispatch: resume found job by idem_key", "order_id", o.ID, "attempt", att.Attempt)
			return s.recordSent(ctx, o, att, rc, nil)
		case !errors.Is(err, adapter.ErrJobNotFound):
			return fmt.Errorf("service: dispatch resume order %d query: %w", o.ID, err)
		}
		// 查无此单不能证明请求未到达（外部单号索引可能延迟），只有平台按 idem_key 去重时重发才安全
		if !caps.IdemKey {
			return s.toManual(ctx, o, att, manualQueryMissNoIdem)
		}
	} else if !caps.IdemKey {
		return s.toManual(ctx, o, att, manualNoIdemNoQuery)
	}
	disp, ok := dev.(adapter.ArchivedDispatcher)
	if !ok {
		return fmt.Errorf("%w: order %d device_type %s adapter cannot dispatch archived command", ErrDispatchState, o.ID,
			att.DeviceType)
	}
	// 重发即可能产生真实作业：与首发一样先直读开关（覆盖 resumeAttempt 与 dispatchReal 两个入口）。关 → 不调 Adapter 转人工：
	// 首发请求是否已到平台未知，不能判 FAILED 或重试
	flagReadAt := s.clock.Now()
	on, err := d.Flags.BoolLive(ctx, model.FlagEnableRobotDispatch, model.Scope{TenantID: o.TenantID, FarmID: o.FarmID})
	if err != nil {
		return fmt.Errorf("service: dispatch resume order %d read flag: %w", o.ID, err)
	}
	if !on {
		return s.toManual(ctx, o, att, manualResumeFlagOff)
	}
	if _, err := s.guardDeviceCall(ctx, att.Ref(), deviceCallCheck{TicketID: att.TicketID}); err != nil {
		return err
	}
	if err := s.markIdem(ctx, att, true); err != nil {
		return err
	}
	if att, err = s.refreshSent(ctx, att, flagReadAt); err != nil {
		return err
	}
	d.Logger.WarnContext(ctx, "dispatch: resume resend with original idem_key", "order_id", o.ID, "attempt", att.Attempt)
	rc, err := disp.DispatchArchived(tctx, o.WorkOrder(), att.IdemKey, att.CommandRawID)
	return s.recordSent(ctx, o, att, rc, err)
}

// refreshSent 在续跑重发前把尝试的 flag_read_at 刷成本次读开关时刻、sent_at 刷成现在、receipt_due_at 顺延为现在 + 回执时限
// （回执时限从本次重发起算），关停报告据此覆盖续跑重发；返回 row_ver+1 后的尝试。
func (s *WorkOrderService) refreshSent(ctx context.Context, att model.DispatchAttempt,
	flagReadAt time.Time) (model.DispatchAttempt, error) {
	d := s.dispatch
	now := s.clock.Now()
	due := now.Add(d.ReceiptTimeout)
	err := d.Tx.InTx(ctx, func(tx *repository.DB) error {
		return d.Attempts.RefreshSentTx(ctx, tx, att.Ref(), att.ID, att.RowVer, flagReadAt, now, due)
	})
	if err != nil {
		return att, fmt.Errorf("service: dispatch resume order %d attempt %d refresh: %w", att.OrderID, att.Attempt, err)
	}
	att.FlagReadAt, att.SentAt, att.ReceiptDueAt, att.RowVer = flagReadAt, now, due, att.RowVer+1
	return att, nil
}

// dispatchManual 是 DISPATCH_MANUAL 审计的 summary。
type dispatchManual struct {
	OrderID    uint64 `json:"order_id"`
	Attempt    int    `json:"attempt"`
	DeviceType string `json:"device_type"`
	IdemKey    string `json:"idem_key"`
	Reason     string `json:"reason"`
}

// DISPATCH_MANUAL 的原因：SENT 无 device_job_id 且无法安全重发。
const (
	manualNoIdemNoQuery   = "SENT without device_job_id; adapter supports neither idem_key nor external query"
	manualQueryMissNoIdem = "SENT without device_job_id; external query found no job and adapter has no idem_key dedup"
	manualResumeFlagOff   = "SENT without device_job_id; resend blocked because enable_robot_dispatch is off"
)

// toManual 把 SENT 尝试置 FAILED(UNKNOWN)（进待对账列表）并审计 DISPATCH_MANUAL；作业单保持 DISPATCHED，不重发。
func (s *WorkOrderService) toManual(ctx context.Context, o repository.DispatchOrder, att model.DispatchAttempt,
	reason string) error {
	d := s.dispatch
	payload, err := json.Marshal(dispatchManual{OrderID: o.ID, Attempt: att.Attempt, DeviceType: att.DeviceType,
		IdemKey: att.IdemKey, Reason: reason})
	if err != nil {
		return fmt.Errorf("service: encode DISPATCH_MANUAL order %d: %w", o.ID, err)
	}
	now := s.clock.Now()
	runID := o.RunID
	rec := repository.NewAuditRecord(model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}, &runID,
		model.Actor{Type: model.ActorSystem}, model.AuditDispatchManual, "order:"+strconv.FormatUint(o.ID, 10), payload,
		model.AuditResultFailed, now)
	err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
		if err := d.Attempts.UpdateReceiptTx(ctx, tx, att.Ref(), att.ID, att.RowVer, model.AttemptFailed,
			model.FailureUnknown, nil, att.ReceiptDueAt, now); err != nil {
			return err
		}
		return d.Audit.AppendTx(ctx, tx, rec)
	})
	if err != nil {
		return fmt.Errorf("service: dispatch manual order %d: %w", o.ID, err)
	}
	d.Logger.ErrorContext(ctx, "dispatch: SENT attempt cannot be resumed safely, manual reconcile", "alert", "DISPATCH_MANUAL",
		"order_id", o.ID, "attempt", att.Attempt, "device_type", att.DeviceType)
	return nil
}

// dispatchReal 是 REAL 单的 ⑤-⑦，全程持 lock:order（续期失败即取消 ctx，设备 IO 随之中断）：
// 锁内再按 idem_key 查尝试（并发 worker 已写过 → 续跑）→ 重读状态 → 校验已绑 device_id → Traced.Archive →
// 同事务 InsertSentTx（含 flag_read_at、command_raw_id）+ attempt=1 时 APPROVED → DISPATCHED + DISPATCHED(INTENT) →
// Redis idem:dispatch 7d → DispatchArchived(idem_key) → recordSent。
func (s *WorkOrderService) dispatchReal(ctx context.Context, msg repository.OutboxMessage, m dispatchMessage,
	ref model.OrderRef, flagReadAt time.Time) error {
	d := s.dispatch
	scope := model.Scope{TenantID: ref.TenantID, FarmID: ref.FarmID}
	return WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, ref.OrderID, func(lctx context.Context) error {
		prev, err := d.Attempts.ByIdemKey(lctx, nil, scope, msg.DedupKey)
		switch {
		case err == nil:
			return s.resumeLocked(lctx, prev)
		case !errors.Is(err, model.ErrNotFound):
			return fmt.Errorf("service: dispatch order %d attempt by idem: %w", ref.OrderID, err)
		}
		o, err := d.Store.OrderForDispatch(lctx, nil, ref)
		if err != nil {
			return fmt.Errorf("service: dispatch: %w", err)
		}
		if done, err := s.realReady(lctx, m, o); err != nil || done {
			return err
		}
		deviceID := payloadDeviceID(o.Payload)
		if deviceID == "" {
			return fmt.Errorf("%w: order %d has no bound device_id", ErrDispatchState, o.ID)
		}
		dev, err := d.Devices.Get(o.DeviceType)
		if err != nil {
			return fmt.Errorf("service: dispatch order %d: %w", o.ID, err)
		}
		arch, okA := dev.(adapter.CommandArchiver)
		disp, okD := dev.(adapter.ArchivedDispatcher)
		if !okA || !okD {
			return fmt.Errorf("%w: order %d device_type %s adapter is not traced", ErrDispatchState, o.ID, o.DeviceType)
		}
		work := o.WorkOrder()
		raw, err := arch.Archive(lctx, work, msg.DedupKey)
		if err != nil {
			return fmt.Errorf("service: dispatch archive order %d: %w", o.ID, err)
		}
		att, err := s.recordSentIntent(lctx, o, m, msg.DedupKey, deviceID, raw.ID, flagReadAt)
		if err != nil {
			return err
		}
		if err := s.markIdem(lctx, att, false); err != nil {
			return err
		}
		if _, err := s.guardDeviceCall(lctx, ref, deviceCallCheck{TicketID: m.TicketID}); err != nil {
			return err
		}
		rc, err := disp.DispatchArchived(adapter.WithTraceOrder(lctx, traceOrder(o)), work, att.IdemKey, att.CommandRawID)
		return s.recordSent(lctx, o, att, rc, err)
	})
}

// realReady 是锁内状态复核：attempt=1 要求 APPROVED（过时效 → EXPIRED），attempt>1 要求 DISPATCHED；放行前 admitTicket
// 复核审批票（Archive / SENT 尝试 / 迁移之前）。返回 true 表示处理完毕。
func (s *WorkOrderService) realReady(ctx context.Context, m dispatchMessage, o repository.DispatchOrder) (bool, error) {
	st := model.WorkOrderStatus(o.Status)
	switch {
	case st.IsTerminal():
		return true, nil
	case m.Attempt > 1 && st != model.OrderDispatched:
		s.dispatch.Logger.WarnContext(ctx, "dispatch: retry for non-dispatched order dropped", "order_id", o.ID,
			"status", o.Status, "attempt", m.Attempt)
		return true, nil
	case m.Attempt > 1:
	case st != model.OrderApproved:
		return false, fmt.Errorf("%w: order %d changed to %s before dispatch", ErrDispatchState, o.ID, o.Status)
	case s.overdue(o):
		return true, s.expireLocked(ctx, o.Ref())
	}
	return s.admitTicket(ctx, o, m.TicketID)
}

// recordSentIntent 是 REAL 单的 ⑤：同事务写 SENT 尝试、attempt=1 时 APPROVED → DISPATCHED、追加 DISPATCHED(INTENT)。
// 同 idem_key / 同 attempt 已写过 → repository.ErrDuplicateAttempt（返回错误，outbox 重试后走续跑）。
func (s *WorkOrderService) recordSentIntent(ctx context.Context, o repository.DispatchOrder, m dispatchMessage, idemKey,
	deviceID string, commandRawID uint64, flagReadAt time.Time) (model.DispatchAttempt, error) {
	d := s.dispatch
	now := s.clock.Now()
	att := model.DispatchAttempt{TenantID: o.TenantID, FarmID: o.FarmID, OrderID: o.ID, TicketID: m.TicketID,
		Attempt: m.Attempt, DeviceType: o.DeviceType, DeviceID: deviceID, IdemKey: idemKey, Mode: model.DispatchReal,
		Status: model.AttemptSent, CommandRawID: commandRawID, FlagReadAt: flagReadAt, SentAt: now,
		ReceiptDueAt: now.Add(d.ReceiptTimeout)}
	err := d.Tx.InTx(ctx, func(tx *repository.DB) error {
		id, err := d.Attempts.InsertSentTx(ctx, tx, att)
		if err != nil {
			return err
		}
		att.ID = id
		if m.Attempt == 1 {
			if err := s.TransitionFrom(ctx, tx, o.Ref(), model.OrderApproved, model.OrderDispatched,
				model.Actor{Type: model.ActorSystem}, ""); err != nil {
				return err
			}
		}
		return s.appendEvent(ctx, tx, o, model.DispatchPhaseIntent, realPayload(o, att, model.DispatchPhaseIntent, false))
	})
	if err != nil {
		return model.DispatchAttempt{}, fmt.Errorf("service: dispatch intent order %d attempt %d: %w", o.ID, m.Attempt, err)
	}
	return att, nil
}

// markIdem 写 Redis idem:dispatch:{order_id}:{ticket_id}:{attempt}（7d，值为 idem_key）。首次下发键已存在说明另有处理者，
// 不调设备（返回错误，outbox 重试走续跑）；续跑重发时键已存在属正常。
func (s *WorkOrderService) markIdem(ctx context.Context, att model.DispatchAttempt, resend bool) error {
	d := s.dispatch
	ok, err := d.Idem.SetNX(ctx, d.Keys.IdemDispatch(att.OrderID, att.TicketID, att.Attempt), att.IdemKey,
		rediskey.IdemDispatchTTL)
	if err != nil {
		return fmt.Errorf("service: dispatch order %d idem key: %w", att.OrderID, err)
	}
	if !ok && !resend {
		return fmt.Errorf("%w: order %d attempt %d idem:dispatch already set", ErrDispatchState, att.OrderID, att.Attempt)
	}
	return nil
}

// recordSent 是 REAL 单的 ⑦：按设备返回处置 SENT 尝试。
//   - 受理（有 device_job_id）：同事务回填 job / 回执状态 / receipt_due_at（row_ver 守卫）+ DISPATCHED(RESULT)；后续回执归 L4-B。
//   - 平台明确拒绝（ErrRejected，或 FAILED 回执无 job）：设备未动作，同事务尝试 FAILED + DISPATCHED → FAILED(DISPATCH_REJECTED)。
//   - 其余错误（超时 / 不可用 / 回执不合规）：尝试保持 SENT，返回错误；outbox 重试走续跑（ExternalQuery / IdemKey / 转人工）。
func (s *WorkOrderService) recordSent(ctx context.Context, o repository.DispatchOrder, att model.DispatchAttempt,
	rc model.DeviceReceipt, callErr error) error {
	d := s.dispatch
	now := s.clock.Now()
	failedNoJob := callErr == nil && rc.Status == model.AttemptFailed && rc.DeviceJobID == ""
	rejected := errors.Is(callErr, adapter.ErrRejected) || (failedNoJob && rc.FailureKind == model.FailureNotExecuted)
	var err error
	switch {
	case failedNoJob && !rejected:
		// 无任务号却报执行中失败 / 无法判断：不敢判未动作，尝试置 FAILED(原 kind) 进待对账，单保持 DISPATCHED、不重发
		err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
			return d.Attempts.UpdateReceiptTx(ctx, tx, att.Ref(), att.ID, att.RowVer, model.AttemptFailed, rc.FailureKind,
				rc.DoneAreaMu, att.ReceiptDueAt, now)
		})
		if err == nil {
			d.Observer.Dispatched(o.FarmID, model.DispatchReal, DispatchManual)
			d.Logger.ErrorContext(ctx, "dispatch: failed receipt without job, manual reconcile", "alert", "DISPATCH_MANUAL",
				"order_id", o.ID, "attempt", att.Attempt, "failure_kind", rc.FailureKind, "code", rc.Code)
		}
	case rejected:
		err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
			if err := d.Attempts.UpdateReceiptTx(ctx, tx, att.Ref(), att.ID, att.RowVer, model.AttemptFailed,
				model.FailureNotExecuted, nil, att.ReceiptDueAt, now); err != nil {
				return err
			}
			if err := s.TransitionFrom(ctx, tx, o.Ref(), model.OrderDispatched, model.OrderFailed,
				model.Actor{Type: model.ActorSystem}, model.ReasonDispatchRejected); err != nil {
				return err
			}
			return s.appendEvent(ctx, tx, o, model.DispatchPhaseResult, realPayload(o, att, model.DispatchPhaseResult, false))
		})
		if err == nil {
			d.Observer.Dispatched(o.FarmID, model.DispatchReal, DispatchRejected)
			d.Logger.WarnContext(ctx, "dispatch: rejected by device platform, order failed", "order_id", o.ID,
				"attempt", att.Attempt, "err", callErr, "code", rc.Code)
		}
	case callErr != nil:
		d.Observer.Dispatched(o.FarmID, model.DispatchReal, DispatchError)
		return fmt.Errorf("service: dispatch order %d attempt %d device: %w", o.ID, att.Attempt, callErr)
	case rc.DeviceJobID == "":
		d.Observer.Dispatched(o.FarmID, model.DispatchReal, DispatchError)
		return fmt.Errorf("service: dispatch order %d attempt %d: %w: %s receipt without device_job_id", o.ID, att.Attempt,
			model.ErrBadReceipt, rc.Status)
	default:
		att.DeviceJobID = rc.DeviceJobID
		err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
			if err := d.Attempts.SetJobTx(ctx, tx, att.Ref(), att.ID, att.RowVer, rc, now.Add(d.ReceiptTimeout), now); err != nil {
				return err
			}
			return s.appendEvent(ctx, tx, o, model.DispatchPhaseResult, realPayload(o, att, model.DispatchPhaseResult, true))
		})
		if err == nil {
			d.Observer.Dispatched(o.FarmID, model.DispatchReal, DispatchOK)
			d.Logger.InfoContext(ctx, "dispatch: accepted by device platform", "order_id", o.ID, "attempt", att.Attempt,
				"device_id", att.DeviceID, "device_job_id", rc.DeviceJobID, "status", rc.Status)
		}
	}
	if err != nil {
		return fmt.Errorf("service: dispatch result order %d attempt %d: %w", o.ID, att.Attempt, err)
	}
	return nil
}

// realPayload 是 REAL 单 DISPATCHED 事件 payload。
func realPayload(o repository.DispatchOrder, att model.DispatchAttempt, phase string, touched bool) model.RealDispatchedPayload {
	return model.RealDispatchedPayload{
		DispatchedPayload: model.DispatchedPayload{OrderID: o.ID, TicketID: att.TicketID, DispatchMode: model.DispatchReal,
			DryRun: false, DeviceTouched: touched, Phase: phase},
		Attempt: att.Attempt, DeviceType: att.DeviceType, DeviceID: att.DeviceID, DeviceJobID: att.DeviceJobID}
}

// traceOrder 是设备调用 trace 的作业单归属。
func traceOrder(o repository.DispatchOrder) adapter.TraceOrder {
	return adapter.TraceOrder{TenantID: o.TenantID, FarmID: o.FarmID, OrderID: o.ID, RunID: o.RunID}
}
