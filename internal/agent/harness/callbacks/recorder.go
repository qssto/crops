// Package callbacks 把 Agent 图的执行过程落为 run_event 链路（L7 trace，替代暂缓的 OTel）：
// 节点开始 / 结束 → STEP_STARTED / STEP_ENDED（step_id 与 runner 的 run_step 一致），
// 模型调用 → 原文写 raw_object(LLM_IO) 后发 LLM_CALL_END（raw_object_id 关联，payload 只含 token / 耗时 / 状态）；
// 工具调用的 TOOL_CALL_END 由 MCP 网关发出（step_id + tool_call_id），三级贯通 run_id → step_id → tool_call_id。
// 同一份 ID / 耗时 / token 另交 Tracer（默认写 slog），接 OTel 留到依赖获批后。
// 本包只依赖 agent 接缝、llm、runner 与 model，不 import 业务包（archtest）。
package callbacks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	einocb "github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// Run 是当前 run 的标识（graph 在执行体内经 WithRun 放入 ctx）。
type Run struct {
	Scope model.Scope
	RunID uint64
}

type runKey struct{}

// WithRun 把 run 标识放入 ctx；没有它的 ctx（如下发前复检）不发事件。
func WithRun(ctx context.Context, r Run) context.Context { return context.WithValue(ctx, runKey{}, r) }

// RunFrom 取 ctx 里的 run 标识。
func RunFrom(ctx context.Context) (Run, bool) {
	r, ok := ctx.Value(runKey{}).(Run)
	return r, ok
}

// Config 是 Recorder 的依赖。Events / Clock 必填；Raw 可空（不落 LLM_IO 原文）；Tracer / Logger 可空。
type Config struct {
	Events agent.EventSink
	Raw    agent.RawStore
	Clock  clock.Clock
	Tracer Tracer
	Logger *slog.Logger
}

// Recorder 是线程安全的；每次 run 的状态都在 ctx 里。
type Recorder struct {
	events agent.EventSink
	raw    agent.RawStore
	clk    clock.Clock
	tracer Tracer
	log    *slog.Logger
}

// New 校验依赖并构造 Recorder。
func New(cfg Config) (*Recorder, error) {
	var errs []error
	if cfg.Events == nil {
		errs = append(errs, errors.New("callbacks: event sink is required"))
	}
	if cfg.Clock == nil {
		errs = append(errs, errors.New("callbacks: clock is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = nopTracer{}
	}
	return &Recorder{events: cfg.Events, raw: cfg.Raw, clk: cfg.Clock, tracer: tracer, log: log}, nil
}

// step 是节点回调放进 ctx 的当前步骤（节点内的模型调用据此带 step_id）。
type step struct {
	id    string
	start time.Time
}

type stepKey struct{}

func stepFrom(ctx context.Context) (step, bool) {
	s, ok := ctx.Value(stepKey{}).(step)
	return s, ok
}

// writable 判断能否写事件：ctx 带 run 标识（只有 runner 托管的执行体会设），且执行权未丢失。
// ctx 被取消仍写（WithoutCancel），保证每个 STEP_STARTED 都有对应 STEP_ENDED。
func writable(ctx context.Context) (Run, bool) {
	r, ok := RunFrom(ctx)
	if !ok || errors.Is(runner.Alive(ctx), runner.ErrLeaseLost) {
		return Run{}, false
	}
	return r, true
}

// emit 追加一条事件；失败只告警（观测链路不影响主链路）。
func (r *Recorder) emit(ctx context.Context, run Run, e model.RunEvent) {
	e.TenantID, e.FarmID, e.RunID, e.CreatedAt = run.Scope.TenantID, run.Scope.FarmID, run.RunID, r.clk.Now()
	if _, err := r.events.Emit(context.WithoutCancel(ctx), e); err != nil {
		r.log.WarnContext(ctx, "emit event failed", "run_id", run.RunID, "event_type", string(e.Type), "err", err)
	}
}

// Handler 返回 Eino 回调：节点开始 / 结束 / 失败发 STEP_STARTED / STEP_ENDED；整图回调跳过。
// 中断（interrupt-rerun）记 STEP_ENDED(INTERRUPTED)，resume 时该节点重跑并另起一对 STEP 事件。
func (r *Recorder) Handler() einocb.Handler {
	return einocb.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *einocb.RunInfo, _ einocb.CallbackInput) context.Context {
			run, ok := writable(ctx)
			if !ok || !isNode(info) {
				return ctx
			}
			s := step{id: info.Name, start: r.clk.Now()}
			r.emitStep(ctx, run, model.EventStepStarted, s.id, model.StepPayload{Node: info.Name})
			return context.WithValue(ctx, stepKey{}, s)
		}).
		OnEndFn(func(ctx context.Context, info *einocb.RunInfo, _ einocb.CallbackOutput) context.Context {
			r.endStep(ctx, info, model.StepStatusOK, "")
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *einocb.RunInfo, err error) context.Context {
			if _, interrupted := compose.IsInterruptRerunError(err); interrupted {
				r.endStep(ctx, info, model.StepStatusInterrupted, "")
				return ctx
			}
			r.endStep(ctx, info, model.StepStatusError, errorCode(err))
			return ctx
		}).
		Build()
}

func isNode(info *einocb.RunInfo) bool {
	return info != nil && info.Name != "" && info.Component != compose.ComponentOfGraph
}

// endStep 发 STEP_ENDED；只对本 Recorder 发过 STEP_STARTED 的节点发（ctx 里有同名 step）。
func (r *Recorder) endStep(ctx context.Context, info *einocb.RunInfo, status, code string) {
	run, ok := writable(ctx)
	if !ok || !isNode(info) {
		return
	}
	s, ok := stepFrom(ctx)
	if !ok || s.id != info.Name {
		return
	}
	latency := r.clk.Now().Sub(s.start).Milliseconds()
	r.emitStep(ctx, run, model.EventStepEnded, s.id,
		model.StepPayload{Node: info.Name, Status: status, LatencyMS: latency, Error: code})
	r.tracer.Record(ctx, Span{Kind: SpanStep, Name: info.Name, RunID: run.RunID, StepID: s.id,
		Status: status, LatencyMS: latency, Error: code})
}

func (r *Recorder) emitStep(ctx context.Context, run Run, typ model.EventType, stepID string, p model.StepPayload) {
	body, err := marshal(p)
	if err != nil {
		r.log.WarnContext(ctx, "encode step payload failed", "run_id", run.RunID, "err", err)
		return
	}
	r.emit(ctx, run, model.RunEvent{StepID: stepID, Type: typ, Actor: model.ActorAgent, Payload: body})
}

// errorCode 取节点错误码（与 run_step.error_code 同口径）；不写错误原文，避免 PII 进事件。
func errorCode(err error) string {
	var c runner.Coder
	if errors.As(err, &c) && c.Code() != "" {
		return c.Code()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "CANCELED"
	}
	return runner.StepErrorCode
}
