// Package evalkit 是评测 harness：按 (sample, trial) 调度被测 agent 并记录结果。
// 被测 agent 一律经 app.Build 装配，与生产共用装配逻辑；生产代码不得 import 本包。
package evalkit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// DefaultTrials 是每个样本默认运行次数（P0 §6.1 技术实现 3）。
const DefaultTrials = 3

// Trial 是一个样本的一次运行结果。L4 起补充 run_id、transcript 与打分明细。
type Trial struct {
	SampleID     string
	Suite        string
	Tags         []string
	TrialIndex   int
	Expected     model.Outcome
	Outcome      model.Outcome
	RefuseReason string
	Latency      time.Duration
	Error        string
	// ToolCalls 是工具名 → 调用次数；UnexpectedCalls 是未在 fixtures.mcp 声明的调用（按顺序的工具名）。
	// 仅当 App.Tools 实现 ToolRecorder（FixtureMCP）时填充。
	ToolCalls       map[string]int
	UnexpectedCalls []string
}

// ToolRecorder 是评测工具接缝的调用记录（env.FixtureMCP 实现）。
type ToolRecorder interface {
	CallCounts() map[string]int
	UnexpectedCalls() []env.ToolCallRecord
}

// recordTools 把 trial 的工具调用记录写入 t（Tools 不记录时不动）。
func recordTools(t *Trial, tools agent.ToolProvider) {
	rec, ok := tools.(ToolRecorder)
	if !ok {
		return
	}
	t.ToolCalls = rec.CallCounts()
	for _, c := range rec.UnexpectedCalls() {
		t.UnexpectedCalls = append(t.UnexpectedCalls, c.Tool)
	}
}

// Experiment 是一次评测的全部 trial。
type Experiment struct {
	ID         string
	Suites     []string
	Tags       []string
	N          int
	StartedAt  time.Time
	FinishedAt time.Time
	Samples    int
	Trials     []Trial
}

// Options 是一次评测的参数。
type Options struct {
	ExperimentID string
	Suites       []string
	Tags         []string
	N            int
}

// EnvFunc 为一个 (sample, trial) 提供经 app.Build 装配的 App 与归还函数。
// 评测用 env.Pool（agri_eval_t_{n} + as:eval-t{n}: + FrozenClock）实现；单测可用 StaticEnv。
type EnvFunc func(ctx context.Context, s *sample.Sample, index int) (a *app.App, release func() error, err error)

// StaticEnv 让所有 trial 共用同一个 App（无需隔离的单测）。
func StaticEnv(a *app.App) EnvFunc {
	return func(context.Context, *sample.Sample, int) (*app.App, func() error, error) {
		return a, func() error { return nil }, nil
	}
}

// Runner 调度样本运行。now 是墙钟（统计延迟用），与被测 App 内的 FrozenClock 无关。
type Runner struct {
	env EnvFunc
	now func() time.Time
}

// NewRunner 构造 Runner；now 为 nil 时用系统时钟。
func NewRunner(env EnvFunc, now func() time.Time) (*Runner, error) {
	if env == nil {
		return nil, errors.New("evalkit: env is nil")
	}
	if now == nil {
		now = clock.System{}.Now
	}
	return &Runner{env: env, now: now}, nil
}

// Run 对 samples 中每个样本顺序跑 opts.N 次。单个 trial 的 agent 错误记入 Trial.Error，
// 不中断评测；ctx 取消时停止并返回已完成部分与错误。
func (r *Runner) Run(ctx context.Context, samples []*sample.Sample, opts Options) (*Experiment, error) {
	if opts.N < 1 {
		return nil, fmt.Errorf("evalkit: n must be >= 1, got %d", opts.N)
	}
	exp := &Experiment{
		ID:        opts.ExperimentID,
		Suites:    opts.Suites,
		Tags:      opts.Tags,
		N:         opts.N,
		StartedAt: r.now(),
		Samples:   len(samples),
		Trials:    make([]Trial, 0, len(samples)*opts.N),
	}
	defer func() { exp.FinishedAt = r.now() }()

	for _, s := range samples {
		for i := 0; i < opts.N; i++ {
			if err := ctx.Err(); err != nil {
				return exp, fmt.Errorf("evalkit: stopped before %s#%d: %w", s.ID, i, err)
			}
			exp.Trials = append(exp.Trials, r.runTrial(ctx, s, i))
		}
	}
	return exp, nil
}

func (r *Runner) runTrial(ctx context.Context, s *sample.Sample, index int) Trial {
	t := Trial{
		SampleID:   s.ID,
		Suite:      s.Suite,
		Tags:       s.Tags,
		TrialIndex: index,
		Expected:   s.Expect.Outcome,
	}
	a, release, err := r.env(ctx, s, index)
	if err != nil {
		t.Error = fmt.Errorf("trial %s#%d: env: %w", s.ID, index, err).Error()
		return t
	}
	start := r.now()
	ans, askErr := a.Agent.Ask(ctx, questionOf(s))
	t.Latency = r.now().Sub(start)
	recordTools(&t, a.Tools)
	if relErr := release(); relErr != nil {
		askErr = errors.Join(askErr, fmt.Errorf("release env: %w", relErr))
	}
	if askErr != nil {
		t.Error = fmt.Errorf("trial %s#%d: %w", s.ID, index, askErr).Error()
		return t
	}
	t.Outcome = ans.Outcome
	t.RefuseReason = ans.RefuseReason
	return t
}

func questionOf(s *sample.Sample) agent.Question {
	return agent.Question{
		Text:      s.Question,
		FarmCode:  s.Context.Farm,
		FieldCode: s.Context.Field,
		Role:      s.Context.Role,
		UserID:    s.Context.User,
	}
}

// Select 按套件与 tag 过滤样本：suites 为空不限套件；tags 为空不限 tag，否则命中任一即保留。
func Select(samples []*sample.Sample, suites, tags []string) []*sample.Sample {
	suiteSet := make(map[string]bool, len(suites))
	for _, s := range suites {
		suiteSet[s] = true
	}
	out := make([]*sample.Sample, 0, len(samples))
	for _, s := range samples {
		if len(suiteSet) > 0 && !suiteSet[s.Suite] {
			continue
		}
		if s.HasAnyTag(tags) {
			out = append(out, s)
		}
	}
	return out
}
