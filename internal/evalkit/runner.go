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
	"agri-steward/internal/evalkit/sample"
)

// DefaultTrials 是每个样本默认运行次数（P0 §6.1 技术实现 3）。
const DefaultTrials = 3

// Trial 是一个样本的一次运行结果。L4 起补充 run_id、transcript 与打分明细。
type Trial struct {
	SampleID     string
	Tags         []string
	TrialIndex   int
	Expected     agent.Outcome
	Outcome      agent.Outcome
	RefuseReason string
	Latency      time.Duration
	Error        string
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

// Runner 调度样本运行。now 可注入，便于测试延迟统计。
type Runner struct {
	app *app.App
	now func() time.Time
}

// NewRunner 构造 Runner；a 必须由 app.Build 装配。
func NewRunner(a *app.App, now func() time.Time) (*Runner, error) {
	if a == nil {
		return nil, errors.New("evalkit: app is nil")
	}
	if now == nil {
		now = time.Now
	}
	return &Runner{app: a, now: now}, nil
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
		Tags:       s.Tags,
		TrialIndex: index,
		Expected:   s.Expect.Outcome,
	}
	start := r.now()
	ans, err := r.app.Agent.Ask(ctx, questionOf(s))
	t.Latency = r.now().Sub(start)
	if err != nil {
		t.Error = fmt.Errorf("trial %s#%d: %w", s.ID, index, err).Error()
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
