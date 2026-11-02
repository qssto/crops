package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/model"
)

// 提问入口参数与异步执行约束。
const (
	// MaxQuestionRunes 是问题文本上限（按字符）。
	MaxQuestionRunes = 500
	// askRunTimeout 是单次异步主链路的兜底超时，超时由 Ask 返回错误并置 FAILED。
	askRunTimeout = 5 * time.Minute
	// 异步失败写入 agent_run.reason_code 的取值。
	reasonAgentError = "AGENT_ERROR"
	reasonAgentPanic = "AGENT_PANIC"
)

var (
	// ErrBadInput 表示请求参数不合法（→ 400）。
	ErrBadInput = errors.New("bad input")
	// ErrRateLimited 表示农场提问超过限流（→ 429）。
	ErrRateLimited = errors.New("rate limited")
	// ErrAdviceDisabled 表示本农场 enable_advice 未生效，不生成新建议（→ 403 ADVICE_DISABLED）；已有作业单流程不受影响。
	ErrAdviceDisabled = errors.New("ADVICE_DISABLED")
)

// FlagReader 是业务侧读开关的统一接口（30s 缓存），FlagService 实现。
type FlagReader interface {
	Bool(ctx context.Context, key model.FlagKey, scope model.Scope) (bool, error)
}

// FieldScopeChecker 校验 field_id 属于 Scope，repository.FieldScope 实现。
type FieldScopeChecker interface {
	Owns(ctx context.Context, scope model.Scope, fieldID uint64) (bool, error)
}

// RateLimiter 是农场维度的提问限流，repository.AskLimiter 实现。
type RateLimiter interface {
	Allow(ctx context.Context, farmID uint64) (bool, error)
}

// RunStore 是 AskService 需要的 run 读写，repository.RunRepo 实现。
type RunStore interface {
	CreateRun(ctx context.Context, s model.RunState, userID, fieldID uint64, meta agent.RunMeta) (uint64, error)
	FinishRun(ctx context.Context, scope model.Scope, runID uint64, status model.RunStatus, reasonCode string) error
	Summary(ctx context.Context, scope model.Scope, runID uint64) (model.RunSummary, error)
}

// AdviceReader 按 run 读回建议卡，repository.AdviceRepo 实现；无卡返回 model.ErrNotFound。
type AdviceReader interface {
	GetByRun(ctx context.Context, scope model.Scope, runID uint64) (model.AdviceCard, error)
}

// AskInput 是一次提问；farm_id 只认 Scope。
type AskInput struct {
	FieldID uint64
	Text    string
}

// RunView 是 GET /runs/{run_id} 的结果：run 状态 + 建议卡（尚未产出时为 nil）。
type RunView struct {
	Run  model.RunSummary  `json:"run"`
	Card *model.AdviceCard `json:"card,omitempty"`
}

// AskDeps 是 AskService 的依赖；Flags 必填（enable_advice 判定，P1 L0）。
type AskDeps struct {
	Fields  FieldScopeChecker
	Limiter RateLimiter
	Runs    RunStore
	Advice  AdviceReader
	Agent   agent.Agent
	Flags   FlagReader
	Meta    agent.RunMeta
	Logger  *slog.Logger
}

// AskService 是 ask 入口：校验 → enable_advice → 限流 → 建 run(RUNNING) → 异步跑主链路，立即返回 run_id。
type AskService struct {
	d  AskDeps
	wg sync.WaitGroup
}

// NewAskService 构造 AskService。
func NewAskService(d AskDeps) *AskService { return &AskService{d: d} }

// Ask 建 run 并异步执行；主链路错误或 panic 时把 run 置 FAILED。
func (s *AskService) Ask(ctx context.Context, scope model.Scope, in AskInput) (uint64, error) {
	text := strings.TrimSpace(in.Text)
	if in.FieldID == 0 || text == "" || utf8.RuneCountInString(text) > MaxQuestionRunes {
		return 0, fmt.Errorf("%w: field_id required, text 1-%d chars", ErrBadInput, MaxQuestionRunes)
	}
	ok, err := s.d.Fields.Owns(ctx, scope, in.FieldID)
	if err != nil {
		return 0, fmt.Errorf("service: ask: %w", err)
	}
	if !ok {
		return 0, fmt.Errorf("service: field %d: %w", in.FieldID, ErrNotInScope)
	}
	if err := s.adviceEnabled(ctx, scope); err != nil {
		return 0, err
	}
	allowed, err := s.d.Limiter.Allow(ctx, scope.FarmID)
	if err != nil {
		return 0, fmt.Errorf("service: ask: %w", err)
	}
	if !allowed {
		return 0, fmt.Errorf("service: farm %d: %w", scope.FarmID, ErrRateLimited)
	}
	runID, err := s.d.Runs.CreateRun(ctx, model.RunState{TenantID: scope.TenantID, FarmID: scope.FarmID},
		scope.UserID, in.FieldID, s.d.Meta)
	if err != nil {
		return 0, fmt.Errorf("service: ask: %w", err)
	}
	q := agent.Question{Text: text, Scope: scope, FieldID: in.FieldID, RunID: runID}
	// 独立 ctx：请求返回后主链路继续执行，但保留 ctx 中的值（request id 等）。
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), askRunTimeout)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.run(runCtx, q)
	}()
	return runID, nil
}

// adviceEnabled 判定本农场 enable_advice；未装配开关或读取失败都不放行（失败关闭）。
func (s *AskService) adviceEnabled(ctx context.Context, scope model.Scope) error {
	if s.d.Flags == nil {
		return errors.New("service: ask: flag reader not configured")
	}
	on, err := s.d.Flags.Bool(ctx, model.FlagEnableAdvice, scope)
	if err != nil {
		return fmt.Errorf("service: ask: %w", err)
	}
	if !on {
		return fmt.Errorf("service: farm %d: %w", scope.FarmID, ErrAdviceDisabled)
	}
	return nil
}

// run 执行主链路；panic 与错误都落 FAILED（执行权丢失除外）。主链路已自行收尾（run 非 RUNNING）时 FinishRun 的非法迁移只记日志。
func (s *AskService) run(ctx context.Context, q agent.Question) {
	reason := ""
	log := s.d.Logger.With("run_id", q.RunID, "request_id", RequestIDFrom(ctx))
	defer func() {
		if p := recover(); p != nil {
			log.ErrorContext(ctx, "ask: agent panic", "panic", fmt.Sprint(p))
			reason = reasonAgentPanic
		}
		if reason == "" {
			return
		}
		if err := s.d.Runs.FinishRun(ctx, q.Scope, q.RunID, model.RunFailed, reason); err != nil {
			log.WarnContext(ctx, "ask: finish run failed", "err", err)
		}
	}()
	if _, err := s.d.Agent.Ask(ctx, q); err != nil {
		log.ErrorContext(ctx, "ask: agent error", "err", err)
		// 执行权已丢失：run 可能已被 stuck_run 接管（SYSTEM_ERROR 重入），不得再写 FAILED
		if !errors.Is(err, runner.ErrLeaseLost) {
			reason = reasonAgentError
		}
	}
}

// Wait 等待已启动的异步主链路结束（优雅退出与测试用）。
func (s *AskService) Wait() { s.wg.Wait() }

type requestIDKey struct{}

// WithRequestID 把 X-Request-Id 放入 ctx（controller 调用），异步主链路日志带上它。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom 取 ctx 中的 request id，没有时为空串。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// GetRun 返回 run 状态与建议卡；run 不在 Scope 内返回 model.ErrNotFound。
func (s *AskService) GetRun(ctx context.Context, scope model.Scope, runID uint64) (RunView, error) {
	sum, err := s.d.Runs.Summary(ctx, scope, runID)
	if err != nil {
		return RunView{}, fmt.Errorf("service: get run %d: %w", runID, err)
	}
	view := RunView{Run: sum}
	card, err := s.d.Advice.GetByRun(ctx, scope, runID)
	switch {
	case errors.Is(err, model.ErrNotFound):
	case err != nil:
		return RunView{}, fmt.Errorf("service: get run %d card: %w", runID, err)
	default:
		view.Card = &card
	}
	return view, nil
}
