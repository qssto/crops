// Package evalkit 是评测 harness：按 (sample, trial) 调度被测 agent 并记录结果。
// 被测 agent 一律经 app.Build 装配，与生产共用装配逻辑；生产代码不得 import 本包。
package evalkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// DefaultTrials 是每个样本默认运行次数（P0 §6.1 技术实现 3）。
const DefaultTrials = 3

// Trial 是一个样本的一次运行结果（transcript 在 L5 补充）。
type Trial struct {
	SampleID     string
	Suite        string
	Tags         []string
	TrialIndex   int
	Expected     model.Outcome
	Outcome      model.Outcome
	RefuseReason string
	RunID        uint64
	Card         *model.AdviceCard
	Latency      time.Duration
	Error        string
	// Scores 按打分器顺序列出各维度结果；运行出错（Error 非空）时为空。
	Scores []Score
	// ToolCalls 是工具名 → 调用次数；UnexpectedCalls 是未在 fixtures.mcp 声明的调用（按顺序的工具名）；
	// ToolFailures 是返回故障状态的调用（"tool:STATUS"）。仅当 App.Tools 实现 ToolRecorder（FixtureMCP）时填充。
	ToolCalls       map[string]int
	UnexpectedCalls []string
	ToolFailures    []string
	// L5 护栏观测：Approval 取自 trial 库（App.DB 非空时）；DeviceCalls 仅当 App.Adapter 实现 DeviceRecorder；
	// Logs 仅当 App.Logger 的 Handler 链上有 LogRecorder（env.LogCapture），不写入报告与 agri_eval。
	Approval    *grader.ApprovalState
	DeviceCalls []string
	Logs        []byte
	// Stored 是该 run 写入 trial 库的 tool_call.result_summary 与 run_event.payload（App.DB 非空时，
	// 归还槽位前读取），供 pii_clean 扫描；不写入报告与 agri_eval。
	Stored []grader.StoredText
	// L6：OrderTransitions 是该 run 的作业单迁移（脚本审批人动作之后读 trial 库），App.DB 为空时为 nil。
	OrderTransitions []model.OrderTransition
}

// ToolRecorder 是评测工具接缝的调用记录（env.FixtureMCP 实现）。
type ToolRecorder interface {
	Calls() []env.ToolCallRecord
	CallCounts() map[string]int
	UnexpectedCalls() []env.ToolCallRecord
}

// DeviceRecorder 是评测设备接缝的调用记录（env.RecordingAdapter 实现）。
type DeviceRecorder interface {
	DeviceCalls() []env.DeviceCall
}

// LogRecorder 是 trial 应用日志捕获（env.LogCapture.Tap 返回的 Handler 实现）。
type LogRecorder interface {
	Logs() []byte
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
	t.ToolFailures = []string{}
	for _, c := range rec.Calls() {
		if c.Status != env.StatusOK && c.Status != env.StatusRejected {
			t.ToolFailures = append(t.ToolFailures, c.Tool+":"+c.Status)
		}
	}
}

// recordDevice 把 RecordingAdapter 的调用写入 t（Adapter 不记录时不动；记录为空时是空切片）。
func recordDevice(t *Trial, a adapter.DeviceAdapter) {
	rec, ok := a.(DeviceRecorder)
	if !ok {
		return
	}
	t.DeviceCalls = []string{}
	for _, c := range rec.DeviceCalls() {
		t.DeviceCalls = append(t.DeviceCalls, c.String())
	}
}

// recordLogs 沿 Handler 链（Unwrap）找 LogRecorder 并取日志；找不到时不动。
func recordLogs(t *Trial, l *slog.Logger) {
	if l == nil {
		return
	}
	for h := l.Handler(); h != nil; {
		if rec, ok := h.(LogRecorder); ok {
			t.Logs = rec.Logs()
			return
		}
		u, ok := h.(interface{ Unwrap() slog.Handler })
		if !ok {
			return
		}
		h = u.Unwrap()
	}
}

// recordApproval 从 trial 库读该 run 的状态、最新一张 ORDER 票与作业单当前状态（App.DB 为空时不动）。
// 作业单取 Answer.Order，无单时取票据指向的单；agent.Answer.Order.Status 是出单时的 DRAFT，不能代替库中状态。
func recordApproval(ctx context.Context, t *Trial, a *app.App, scope model.Scope, ans agent.Answer) error {
	if a.DB == nil {
		return nil
	}
	st := &grader.ApprovalState{}
	var ref model.OrderRef
	if o := ans.Order; o != nil {
		ref = model.OrderRef{TenantID: o.TenantID, FarmID: o.FarmID, OrderID: o.OrderID}
	}
	if ans.RunID != 0 {
		run, err := a.Runs.Summary(ctx, scope, ans.RunID)
		if err != nil {
			return fmt.Errorf("read run: %w", err)
		}
		st.RunStatus, st.Interrupt = run.Status, run.Interrupt
		tk, ok, err := (repository.ApprovalRepo{}).TicketByRun(ctx, a.DB, scope, ans.RunID)
		if err != nil {
			return fmt.Errorf("read approval: %w", err)
		}
		if ok {
			st.Ticket = &tk
			if ref.OrderID == 0 {
				ref = model.OrderRef{TenantID: tk.TenantID, FarmID: tk.FarmID, OrderID: tk.OrderID}
			}
		}
	}
	if ref.OrderID != 0 {
		cur, err := (repository.WorkOrderRepo{}).GetState(ctx, a.DB, ref)
		if err != nil {
			return fmt.Errorf("read work order: %w", err)
		}
		st.OrderStatus = cur.Status
	}
	t.Approval = st
	return nil
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

// QuestionFunc 由样本构造提问。评测环境据 slot 库把 farm / field 编码解析为 Scope / FieldID；
// 默认只填编码字段。
type QuestionFunc func(ctx context.Context, a *app.App, s *sample.Sample) (agent.Question, error)

// Runner 调度样本运行。now 是墙钟（统计延迟用），与被测 App 内的 FrozenClock 无关。
type Runner struct {
	env      EnvFunc
	now      func() time.Time
	question QuestionFunc
	approve  ApproveFunc
	grading  grading
}

// RunnerOption 配置 Runner。
type RunnerOption func(*Runner)

// WithGraders 设置打分器（通常为 code.All）与样本知识版本 opener（citation_valid 需要）。
func WithGraders(kb sample.KnowledgeOpener, graders ...grader.Grader) RunnerOption {
	return func(r *Runner) { r.grading = grading{graders: graders, kb: kb} }
}

// WithQuestion 设置提问构造函数。
func WithQuestion(fn QuestionFunc) RunnerOption {
	return func(r *Runner) { r.question = fn }
}

// WithApprover 设置脚本审批人（L6）：在 Ask 返回后、读迁移与打分前执行（见 ScriptApprover）。
func WithApprover(fn ApproveFunc) RunnerOption {
	return func(r *Runner) { r.approve = fn }
}

// NewRunner 构造 Runner；now 为 nil 时用系统时钟；未配置打分器时只跑 outcome_match。
func NewRunner(env EnvFunc, now func() time.Time, opts ...RunnerOption) (*Runner, error) {
	if env == nil {
		return nil, errors.New("evalkit: env is nil")
	}
	if now == nil {
		now = clock.System{}.Now
	}
	r := &Runner{env: env, now: now, question: defaultQuestion, grading: grading{graders: defaultGraders()}}
	for _, opt := range opts {
		opt(r)
	}
	if r.question == nil {
		return nil, errors.New("evalkit: question func is nil")
	}
	if len(r.grading.graders) == 0 {
		return nil, errors.New("evalkit: no grader")
	}
	return r, nil
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
		kb, kbErr := r.grading.openKB(ctx, s)
		for i := 0; i < opts.N; i++ {
			if err := ctx.Err(); err != nil {
				return exp, fmt.Errorf("evalkit: stopped before %s#%d: %w", s.ID, i, err)
			}
			if kbErr != nil {
				exp.Trials = append(exp.Trials, failedTrial(s, i, kbErr))
				continue
			}
			exp.Trials = append(exp.Trials, r.runTrial(ctx, s, i, kb))
		}
	}
	return exp, nil
}

func newTrial(s *sample.Sample, index int) Trial {
	return Trial{SampleID: s.ID, Suite: s.Suite, Tags: s.Tags, TrialIndex: index, Expected: s.Expect.Outcome}
}

// failedTrial 是未能运行的 trial（INFRA_ERROR）。
func failedTrial(s *sample.Sample, index int, err error) Trial {
	t := newTrial(s, index)
	t.Error = fmt.Errorf("trial %s#%d: %w", s.ID, index, err).Error()
	return t
}

func (r *Runner) runTrial(ctx context.Context, s *sample.Sample, index int, kb grader.KnowledgeIndex) Trial {
	a, release, err := r.env(ctx, s, index)
	if err != nil {
		return failedTrial(s, index, fmt.Errorf("env: %w", err))
	}
	t := newTrial(s, index)
	ans, askErr := r.ask(ctx, &t, a, s)
	if relErr := release(); relErr != nil {
		askErr = errors.Join(askErr, fmt.Errorf("release env: %w", relErr))
	}
	if askErr != nil {
		t.Error = fmt.Errorf("trial %s#%d: %w", s.ID, index, askErr).Error()
		return t
	}
	t.Outcome, t.RefuseReason, t.RunID, t.Card = ans.Outcome, ans.RefuseReason, ans.RunID, ans.Card
	r.grading.grade(ctx, &t, s, ans, kb)
	return t
}

// ask 构造提问并调用被测 agent，记录延迟、工具 / 设备调用、日志与审批状态（须在归还槽位前读库）。
func (r *Runner) ask(ctx context.Context, t *Trial, a *app.App, s *sample.Sample) (agent.Answer, error) {
	q, err := r.question(ctx, a, s)
	if err != nil {
		return agent.Answer{}, fmt.Errorf("question: %w", err)
	}
	start := r.now()
	ans, err := a.Agent.Ask(ctx, q)
	t.Latency = r.now().Sub(start)
	if err == nil && r.approve != nil {
		if err = r.approve(ctx, a, s, q, ans); err != nil {
			err = fmt.Errorf("approver: %w", err)
		}
	}
	recordTools(t, a.Tools)
	recordDevice(t, a.Adapter)
	recordLogs(t, a.Logger)
	if err != nil {
		return ans, err
	}
	if err := recordApproval(ctx, t, a, q.Scope, ans); err != nil {
		return ans, err
	}
	if err := recordStored(ctx, t, a, q.Scope, ans.RunID); err != nil {
		return ans, err
	}
	return ans, recordTransitions(ctx, t, a, q.Scope, ans.RunID)
}

// recordStored 从 trial 库读该 run 落库的摘要（App.DB 为空或无 run 时不动；有库无行时是空切片）。
func recordStored(ctx context.Context, t *Trial, a *app.App, scope model.Scope, runID uint64) error {
	if a.DB == nil || runID == 0 {
		return nil
	}
	rows, err := repository.SummaryRepo{}.ByRun(ctx, a.DB, scope, runID)
	if err != nil {
		return fmt.Errorf("record stored summaries: %w", err)
	}
	t.Stored = make([]grader.StoredText, 0, len(rows))
	for _, r := range rows {
		t.Stored = append(t.Stored, grader.StoredText{Source: fmt.Sprintf("%s#%d", r.Source, r.ID), Text: r.Text})
	}
	return nil
}

// recordTransitions 从 trial 库读该 run 的作业单迁移（App.DB 为空或无 run 时不动）。
func recordTransitions(ctx context.Context, t *Trial, a *app.App, scope model.Scope, runID uint64) error {
	if a.DB == nil || runID == 0 {
		return nil
	}
	ts, err := repository.WorkOrderRepo{}.TransitionsByRun(ctx, a.DB, scope, runID)
	if err != nil {
		return fmt.Errorf("record transitions: %w", err)
	}
	t.OrderTransitions = ts
	return nil
}

func defaultQuestion(_ context.Context, _ *app.App, s *sample.Sample) (agent.Question, error) {
	return QuestionOf(s), nil
}

// QuestionOf 由样本情境填编码字段（Scope / FieldID 由 QuestionFunc 补）。
func QuestionOf(s *sample.Sample) agent.Question {
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
