// Package runner 是一次 Agent run 的执行外壳：持有 run 锁（lock:run:{run_id}）并定时续期、
// 续期成功后刷新心跳；把 Eino 图节点的开始 / 结束 / 失败经回调落为 run_step；结束时写终态。
// 续期失败即视为失去执行权：取消 ctx，且此后不再做任何 DB 写入（SaveStep / FinishRun 与业务写入都经 Alive 把关）。
// 本包只依赖 agent 接缝与 model，不 import 业务包（archtest）。
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// DefaultRenewEvery 是 run 锁续期间隔（锁 TTL 由 RunLocker 实现决定，须明显大于此值）。
const DefaultRenewEvery = 10 * time.Second

// 错误码与错误。
var (
	// ErrLeaseLost 表示续期失败、执行权已丢失；此后的写入一律拒绝。
	ErrLeaseLost = errors.New("runner: run lease lost")
	// ErrNoGuard 表示 ctx 不是由 Runner.Run 派生（业务写入无法确认执行权）。
	ErrNoGuard = errors.New("runner: context has no run guard")
)

// Outcome 是一次执行的终态：Status 写入 FinishRun，ReasonCode 可空。
type Outcome struct {
	Status     model.RunStatus
	ReasonCode string
}

// Exec 是被托管的执行体：用 Runner 提供的 ctx 与回调运行图。返回 error 时 run 记 FAILED。
type Exec func(ctx context.Context, cb callbacks.Handler) (Outcome, error)

// Config 是 Runner 的依赖。Logger 可空（默认丢弃）。
type Config struct {
	Locker     agent.RunLocker
	Runs       agent.RunStateStore
	Clock      clock.Clock
	RenewEvery time.Duration // 0 取 DefaultRenewEvery
	Logger     *slog.Logger
}

// Runner 是线程安全的；每次 Run 独立持锁与守卫。
type Runner struct {
	locker     agent.RunLocker
	runs       agent.RunStateStore
	clk        clock.Clock
	renewEvery time.Duration
	log        *slog.Logger
}

// New 校验依赖并构造 Runner。
func New(cfg Config) (*Runner, error) {
	var errs []error
	if cfg.Locker == nil {
		errs = append(errs, errors.New("runner: locker is required"))
	}
	if cfg.Runs == nil {
		errs = append(errs, errors.New("runner: run state store is required"))
	}
	if cfg.Clock == nil {
		errs = append(errs, errors.New("runner: clock is required"))
	}
	if cfg.RenewEvery < 0 {
		errs = append(errs, errors.New("runner: renew interval must be >= 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	every := cfg.RenewEvery
	if every == 0 {
		every = DefaultRenewEvery
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{locker: cfg.Locker, runs: cfg.Runs, clk: cfg.Clock, renewEvery: every, log: log}, nil
}

// guard 记录一次 run 的执行权与回调中的写入错误。
type guard struct {
	mu      sync.Mutex
	lost    bool
	saveErr error
}

func (g *guard) markLost() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lost = true
}

func (g *guard) alive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.lost
}

func (g *guard) recordSave(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.saveErr == nil {
		g.saveErr = err
	}
}

func (g *guard) firstSaveErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.saveErr
}

type guardKey struct{}

// Alive 返回 nil 表示当前 run 仍持有执行权；业务节点在每次 DB 写入前调用。
func Alive(ctx context.Context) error {
	g, ok := ctx.Value(guardKey{}).(*guard)
	if !ok {
		return ErrNoGuard
	}
	if !g.alive() {
		return ErrLeaseLost
	}
	return ctx.Err()
}

// Run 取锁 → 启动续期 → 执行 exec → 写终态 → 释放锁。返回 exec 的结果；
// 执行权丢失时返回 ErrLeaseLost（不写终态，由下一次接管者处理）。
func (r *Runner) Run(ctx context.Context, scope model.Scope, runID uint64, exec Exec) (Outcome, error) {
	lease, err := r.locker.AcquireRun(ctx, runID)
	if err != nil {
		return Outcome{}, fmt.Errorf("runner: acquire run %d: %w", runID, err)
	}
	g := &guard{}
	rctx, cancel := context.WithCancelCause(context.WithValue(ctx, guardKey{}, g))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.keepAlive(rctx, scope, runID, lease, g, cancel, done)
	}()

	out, execErr := exec(rctx, r.handler(scope, runID, g))

	close(done)
	wg.Wait()
	defer cancel(nil)
	defer r.release(ctx, runID, lease)

	if !g.alive() {
		return Outcome{}, fmt.Errorf("run %d: %w", runID, ErrLeaseLost)
	}
	if execErr == nil {
		execErr = g.firstSaveErr()
	}
	if execErr != nil {
		out = Outcome{Status: model.RunFailed, ReasonCode: out.ReasonCode}
	}
	if err := r.runs.FinishRun(ctx, scope, runID, out.Status, out.ReasonCode); err != nil {
		return out, errors.Join(execErr, fmt.Errorf("runner: finish run %d: %w", runID, err))
	}
	return out, execErr
}

// keepAlive 每 renewEvery 续期一次，成功后心跳；续期失败则标记丢失并取消 ctx。
func (r *Runner) keepAlive(ctx context.Context, scope model.Scope, runID uint64, lease agent.Lease,
	g *guard, cancel context.CancelCauseFunc, done <-chan struct{}) {
	t := time.NewTicker(r.renewEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := lease.Renew(ctx); err != nil {
				g.markLost()
				cancel(fmt.Errorf("%w: %w", ErrLeaseLost, err))
				r.log.Warn("run lease lost", "run_id", runID, "err", err)
				return
			}
			if err := r.runs.Heartbeat(ctx, scope, runID); err != nil {
				r.log.Warn("run heartbeat failed", "run_id", runID, "err", err) // 心跳只是观测，失败不夺执行权
			}
		}
	}
}

// StepErrorCode 是失败节点写入 run_step.error_code 的缺省值；错误实现 Coder 时取其 Code()。
const StepErrorCode = "NODE_ERROR"

// Coder 是可携带错误码的错误（节点失败时写入 run_step.error_code）。
type Coder interface{ Code() string }

type stepStartKey struct{}

// handler 把图节点的开始 / 结束 / 失败落为 run_step；整图（ComponentOfGraph）回调跳过。
// 执行权丢失后不再写；写入错误记入 guard，run 以 FAILED 收尾。
func (r *Runner) handler(scope model.Scope, runID uint64, g *guard) callbacks.Handler {
	save := func(ctx context.Context, step model.RunStep) {
		if !g.alive() {
			return
		}
		if err := r.runs.SaveStep(context.WithoutCancel(ctx), scope, runID, step); err != nil {
			g.recordSave(fmt.Errorf("runner: save step %s: %w", step.StepID, err))
		}
	}
	isNode := func(info *callbacks.RunInfo) bool {
		return info != nil && info.Name != "" && info.Component != compose.ComponentOfGraph
	}
	finish := func(ctx context.Context, info *callbacks.RunInfo, status model.StepStatus, code string) {
		started, ok := ctx.Value(stepStartKey{}).(time.Time)
		end := r.clk.Now()
		if !ok {
			started = end
		}
		save(ctx, model.RunStep{StepID: info.Name, Node: info.Name, Status: status, StartedAt: started, EndedAt: &end, ErrorCode: code})
	}
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			if !isNode(info) {
				return ctx
			}
			now := r.clk.Now()
			save(ctx, model.RunStep{StepID: info.Name, Node: info.Name, Status: model.StepStarted, StartedAt: now})
			return context.WithValue(ctx, stepStartKey{}, now)
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackOutput) context.Context {
			if isNode(info) {
				finish(ctx, info, model.StepCompleted, "")
			}
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			if isNode(info) {
				code := StepErrorCode
				var c Coder
				if errors.As(err, &c) && c.Code() != "" {
					code = c.Code()
				}
				finish(ctx, info, model.StepFailed, code)
			}
			return ctx
		}).
		Build()
}

func (r *Runner) release(ctx context.Context, runID uint64, lease agent.Lease) {
	if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
		r.log.Warn("run lease release failed", "run_id", runID, "err", err)
	}
}
