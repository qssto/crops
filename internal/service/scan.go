package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// server 内扫描任务（P0 §6.7 流程 4）：每 30s 一轮，各任务以 lock:scan:{task} 互斥，跨农场查询只在 repository 内。
const (
	ScanApprovalExpire = "approval_expire"
	ScanTicketActivate = "ticket_activate"
	ScanRunFinalize    = "run_finalize"
	ScanStuckRun       = "stuck_run"
)

const (
	// DefaultScanInterval 是扫描周期。
	DefaultScanInterval = 30 * time.Second
	scanLockTTL         = 60 * time.Second
	// preparingGrace：PREPARING 超过 30s 才补激活（正常路径由 runner 中断后立即 Activate）。
	preparingGrace = 30 * time.Second
	// finalizeGrace：票据 APPROVED 超过 60s 而 run 仍 INTERRUPTED 才重触发 Resume。
	finalizeGrace = 60 * time.Second
	// stuckAfter：RUNNING 且 updated_at 超过 90s（3 个续期周期无心跳）。
	stuckAfter = 90 * time.Second
)

// TicketScanStore 是票据 / 作业单扫描的仓储能力，repository.DecisionRepo 实现。
type TicketScanStore interface {
	InTx(ctx context.Context, fn func(tx *repository.DB) error) error
	ListExpired(ctx context.Context, now time.Time) ([]repository.TicketRow, error)
	ListDispatchOverdue(ctx context.Context, now time.Time) ([]repository.OrderKey, error)
	ListPreparing(ctx context.Context, before time.Time) ([]repository.StaleTicket, error)
	VoidPreparingTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, now time.Time) (bool, error)
	ListApprovedInterrupted(ctx context.Context, before time.Time) ([]repository.RunKey, error)
}

// RunScanStore 是卡住 run 扫描的仓储能力，repository.RunRepo 实现。
type RunScanStore interface {
	ListStuck(ctx context.Context, before time.Time) ([]repository.RunKey, error)
	MarkSystemError(ctx context.Context, k repository.RunKey, before time.Time) (bool, error)
	ListSystemError(ctx context.Context) ([]repository.RunKey, error)
	FailInterruptedTx(ctx context.Context, tx *repository.DB, k repository.RunKey, it model.InterruptType, reason model.ReasonCode) error
	DraftOrdersTx(ctx context.Context, tx *repository.DB, scope model.Scope, runID uint64) ([]uint64, error)
}

// TicketExpirer 收尾一张过期 OPEN 票，DecisionService 实现。
type TicketExpirer interface {
	ExpireTicket(ctx context.Context, t repository.TicketRow) (bool, error)
}

// Activator 把 PREPARING 票置 OPEN，agent.ApprovalGate（L5 ApprovalService）实现。
type Activator interface {
	Activate(ctx context.Context, scope model.Scope, runID uint64) (model.ApprovalTicket, error)
}

// LockProber 在 KeyLocker 之上可探测锁是否存在（stuck_run 确认 lock:run 已失效），repository.Locker 实现。
type LockProber interface {
	KeyLocker
	Held(ctx context.Context, key string) (bool, error)
}

// ScanDeps 是 Scanner 的依赖。
type ScanDeps struct {
	Tickets  TicketScanStore
	Runs     RunScanStore
	Expirer  TicketExpirer
	Orders   OrderTransitioner
	Gate     Activator
	Resumer  agent.Resumer
	Locks    LockProber
	Keys     rediskey.Keys
	Clock    clock.Clock
	Logger   *slog.Logger
	Interval time.Duration // 0 取 DefaultScanInterval
}

// Scanner 执行四个 HITL 扫描任务；Resume 异步执行，Wait 等待其结束。
type Scanner struct {
	d     ScanDeps
	tasks []scanTask
	wg    sync.WaitGroup
}

type scanTask struct {
	name string
	fn   func(ctx context.Context, now time.Time) error
}

// NewScanner 构造 Scanner。
func NewScanner(d ScanDeps) *Scanner {
	if d.Interval <= 0 {
		d.Interval = DefaultScanInterval
	}
	s := &Scanner{d: d}
	s.tasks = []scanTask{
		{ScanApprovalExpire, s.approvalExpire},
		{ScanTicketActivate, s.ticketActivate},
		{ScanRunFinalize, s.runFinalize},
		{ScanStuckRun, s.stuckRun},
	}
	return s
}

// Run 每个周期执行一轮扫描，直到 ctx 取消；退出前等待异步 Resume。
func (s *Scanner) Run(ctx context.Context) {
	t := time.NewTicker(s.d.Interval)
	defer t.Stop()
	defer s.wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.RunOnce(ctx); err != nil {
				s.d.Logger.WarnContext(ctx, "scan round", "err", err)
			}
		}
	}
}

// Wait 等待已启动的异步 Resume 结束（测试用；不得与 Run 并发调用，优雅退出应等 Run 返回）。
func (s *Scanner) Wait() { s.wg.Wait() }

// RunOnce 依次执行各任务：lock:scan:{task} 已被其他实例持有即跳过该任务；单个任务失败不影响其余任务。
func (s *Scanner) RunOnce(ctx context.Context) error {
	var errs []error
	for _, task := range s.tasks {
		if err := s.runTask(ctx, task); err != nil {
			errs = append(errs, fmt.Errorf("scan %s: %w", task.name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Scanner) runTask(ctx context.Context, task scanTask) error {
	lease, err := s.d.Locks.AcquireLease(ctx, s.d.Keys.LockScan(task.name), scanLockTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	defer func() {
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			s.d.Logger.WarnContext(ctx, "release scan lock", "task", task.name, "err", err)
		}
	}()
	return task.fn(ctx, s.d.Clock.Now())
}

// skippable 是扫描逐条处理时可跳过（下一轮重试或已被他人收尾）的错误。
func skippable(err error) bool {
	return errors.Is(err, ErrOrderBusy) || errors.Is(err, model.ErrConcurrentUpdate) || errors.Is(err, model.ErrIllegalTransition)
}

// approvalExpire：过期 OPEN 票逐张收尾（票据 UPDATE 0 行即已被决策、跳过）；APPROVED 且过 dispatch_deadline_at 的作业单转 EXPIRED。
func (s *Scanner) approvalExpire(ctx context.Context, now time.Time) error {
	tickets, err := s.d.Tickets.ListExpired(ctx, now)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range tickets {
		if _, err := s.d.Expirer.ExpireTicket(ctx, t); err != nil && !skippable(err) {
			errs = append(errs, err)
		}
	}
	orders, err := s.d.Tickets.ListDispatchOverdue(ctx, now)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, o := range orders {
		if err := s.expireOrder(ctx, o.Ref()); err != nil && !skippable(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// expireOrder 持 lock:order 把超过下发时限的 APPROVED 单转 EXPIRED（已下发则 Transition 非法、跳过）。
func (s *Scanner) expireOrder(ctx context.Context, ref model.OrderRef) error {
	release, err := acquireOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, ref.OrderID)
	if err != nil {
		return err
	}
	defer release()
	return s.d.Tickets.InTx(ctx, func(tx *repository.DB) error {
		return s.d.Orders.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderExpired,
			model.Actor{Type: model.ActorSystem}, model.ReasonApprovalExpired)
	})
}

// ticketActivate：PREPARING 超过 30s 的票，run 已 INTERRUPTED(WAIT_APPROVAL) 则 Activate；run 已终态（失败 / 重试耗尽）
// 则作废票据并把作业单 PENDING_APPROVAL → EXPIRED，避免悬挂；run 仍 RUNNING / SYSTEM_ERROR 交给 stuck_run。
func (s *Scanner) ticketActivate(ctx context.Context, now time.Time) error {
	rows, err := s.d.Tickets.ListPreparing(ctx, now.Add(-preparingGrace))
	if err != nil {
		return err
	}
	var errs []error
	for _, st := range rows {
		status := model.RunStatus(st.RunStatus)
		switch {
		case status == model.RunInterrupted && st.InterruptType != nil && model.InterruptType(*st.InterruptType) == model.InterruptWaitApproval:
			if s.d.Gate == nil {
				continue
			}
			if _, err := s.d.Gate.Activate(ctx, st.Scope(), st.RunID); err != nil && !errors.Is(err, agent.ErrNotActivatable) {
				errs = append(errs, fmt.Errorf("activate run %d: %w", st.RunID, err))
			}
		case status.IsTerminal():
			if err := s.voidTicket(ctx, st.TicketRow, now); err != nil && !skippable(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// voidTicket 持 lock:order 单事务：票据 PREPARING → EXPIRED（row_ver 守卫，0 行跳过）→ Transition(PENDING_APPROVAL → EXPIRED)。
func (s *Scanner) voidTicket(ctx context.Context, t repository.TicketRow, now time.Time) error {
	release, err := acquireOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, t.OrderID)
	if err != nil {
		return err
	}
	defer release()
	err = s.d.Tickets.InTx(ctx, func(tx *repository.DB) error {
		ok, err := s.d.Tickets.VoidPreparingTx(ctx, tx, t, now)
		if err != nil || !ok {
			return err
		}
		return s.d.Orders.Transition(ctx, tx, t.OrderRef(), model.OrderExpired, model.Actor{Type: model.ActorSystem},
			model.ReasonApprovalExpired)
	})
	if err != nil {
		return fmt.Errorf("void ticket %d: %w", t.ID, err)
	}
	return nil
}

// runFinalize：票据已 APPROVED 超过 60s 而 run 仍 INTERRUPTED(WAIT_APPROVAL)（决策后的异步 Resume 失败），重新触发 Resume；
// 不可续（如 graph_version 已变、无检查点）时 run 置 FAILED(RETRY_EXHAUSTED)，不再每轮重试（作业单保持 APPROVED，下发不受影响）。
func (s *Scanner) runFinalize(ctx context.Context, now time.Time) error {
	runs, err := s.d.Tickets.ListApprovedInterrupted(ctx, now.Add(-finalizeGrace))
	if err != nil {
		return err
	}
	for _, k := range runs {
		s.resumeAsync(ctx, k, s.failUnresumable(ScanRunFinalize, k, model.InterruptWaitApproval))
	}
	return nil
}

// stuckRun：RUNNING 超过 90s 未续期且 lock:run 不存在 → INTERRUPTED(SYSTEM_ERROR)、sys_retry + 1；
// SYSTEM_ERROR 且 sys_retry ≤ 3 → Resume 重入（不可续，如无检查点或图版本已变，按重试耗尽收尾）；超过 3 次 → FAILED(RETRY_EXHAUSTED)。
func (s *Scanner) stuckRun(ctx context.Context, now time.Time) error {
	before := now.Add(-stuckAfter)
	stuck, err := s.d.Runs.ListStuck(ctx, before)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range stuck {
		held, err := s.d.Locks.Held(ctx, s.d.Keys.LockRun(k.ID))
		if err != nil {
			errs = append(errs, fmt.Errorf("probe run %d lock: %w", k.ID, err))
			continue
		}
		if held {
			continue
		}
		if _, err := s.d.Runs.MarkSystemError(ctx, k, before); err != nil {
			errs = append(errs, err)
		}
	}
	failed, err := s.d.Runs.ListSystemError(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, k := range failed {
		if k.SysRetry > repository.MaxSysRetry {
			if err := s.failRun(ctx, k, model.InterruptSystemError); err != nil && !skippable(err) {
				errs = append(errs, err)
			}
			continue
		}
		s.resumeAsync(ctx, k, s.failUnresumable(ScanStuckRun, k, model.InterruptSystemError))
	}
	return errors.Join(errs...)
}

// failUnresumable 返回 resumeAsync 的 onErr：Resume 报 agent.ErrNotResumable 时按 failRun 收尾，其余错误留给下一轮扫描。
func (s *Scanner) failUnresumable(task string, k repository.RunKey, it model.InterruptType) func(context.Context, error) {
	return func(ctx context.Context, err error) {
		if !errors.Is(err, agent.ErrNotResumable) {
			return
		}
		if ferr := s.failRun(ctx, k, it); ferr != nil && !skippable(ferr) {
			s.d.Logger.WarnContext(ctx, task+": fail unresumable run", "run_id", k.ID, "err", ferr)
		}
	}
}

// failRun 单事务：run INTERRUPTED(it) → FAILED(RETRY_EXHAUSTED)（row_ver 守卫），其 DRAFT 作业单 CANCELED。
func (s *Scanner) failRun(ctx context.Context, k repository.RunKey, it model.InterruptType) error {
	err := s.d.Tickets.InTx(ctx, func(tx *repository.DB) error {
		if err := s.d.Runs.FailInterruptedTx(ctx, tx, k, it, model.ReasonRetryExhausted); err != nil {
			return err
		}
		ids, err := s.d.Runs.DraftOrdersTx(ctx, tx, k.Scope(), k.ID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			ref := model.OrderRef{TenantID: k.TenantID, FarmID: k.FarmID, OrderID: id}
			if err := s.d.Orders.Transition(ctx, tx, ref, model.OrderCanceled, model.Actor{Type: model.ActorSystem},
				model.ReasonRetryExhausted); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fail run %d: %w", k.ID, err)
	}
	return nil
}

// resumeAsync 异步 Resume 一个 run（Resume 自持 lock:run，重复触发由锁与 row_ver 守卫兜住）；onErr 处理失败（为空只告警）。
func (s *Scanner) resumeAsync(ctx context.Context, k repository.RunKey, onErr func(ctx context.Context, err error)) {
	if s.d.Resumer == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeTimeout)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		_, err := s.d.Resumer.Resume(rctx, k.Scope(), k.ID)
		if err == nil {
			return
		}
		s.d.Logger.WarnContext(rctx, "scan: resume run", "run_id", k.ID, "err", err)
		if onErr != nil {
			onErr(rctx, err)
		}
	}()
}
