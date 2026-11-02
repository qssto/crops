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
	"agri-steward/internal/service"
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
	// L7：Scope 是提问所在租户 / 农场；Detail 是该 run 在 trial 库的完整链路（run_event + tool_call 等，
	// 归还槽位前经 App.RunDetail 读取），供 report 导出三层 transcript；App.DB 为空时为 nil，不写入 agri_eval。
	// Transcript 是已上传的三层 transcript 对象键（report.UploadTranscripts 写入，未上传为空）。
	Scope      model.Scope
	Detail     *service.RunDetail
	Transcript string
	// P1 L1-B：Fields 是本农场全部地块 id（WithFields 优先，否则 App.DB 非空时读 trial 库，都没有为 nil）；
	// Devices 是本农场全部设备 ID（WithDevices 配置时记录，否则 nil）；ToolArgs 是按顺序的工具入参（同 ToolCalls 条件）；
	// DeviceTargets 是设备调用的作业单归属（同 DeviceCalls 条件）。供 cross_farm_isolation，不写入报告与 agri_eval。
	Fields        []uint64
	Devices       []string
	ToolArgs      []grader.ToolArg
	DeviceTargets []grader.DeviceTarget
	// P1-L4-A：Outbox 是该 run 作业单的 outbox 消息（App.DB 非空且有作业单时，归还槽位前读取），recheck_returned 使用。
	Outbox []grader.OutboxRecord
	// P1-L4-B：WorkDone 是回执步骤后该 run 各作业单的完成态（样本声明 fixtures.device.steps 且 App.DB 非空时记录），
	// 供 partially_done，不写入报告与 agri_eval。
	WorkDone []grader.WorkDoneObs
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
	t.ToolArgs = []grader.ToolArg{}
	for _, c := range rec.Calls() {
		t.ToolArgs = append(t.ToolArgs, grader.ToolArg{Tool: c.Tool, Args: c.Args})
		if c.Status != env.StatusOK && c.Status != env.StatusRejected {
			t.ToolFailures = append(t.ToolFailures, c.Tool+":"+c.Status)
		}
	}
}

// recordDevice 把 RecordingAdapter 的调用写入 t（Adapter 不记录时不动；记录为空时是空切片）。
func recordDevice(t *Trial, a adapter.DryRunner) {
	rec, ok := a.(DeviceRecorder)
	if !ok {
		return
	}
	t.DeviceCalls = []string{}
	t.DeviceTargets = []grader.DeviceTarget{}
	for _, c := range rec.DeviceCalls() {
		t.DeviceCalls = append(t.DeviceCalls, c.String())
		t.DeviceTargets = append(t.DeviceTargets, grader.DeviceTarget{Method: c.Method, OrderID: c.OrderID,
			TenantID: c.TenantID, FarmID: c.FarmID, FieldID: c.FieldID, Payload: c.Payload})
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
			if err := recordTicketGate(ctx, st, a, scope, tk.TicketID); err != nil {
				return err
			}
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

// recordTicketGate 读票据的 risk_reasons 与闸门拒绝码（P1 L5，approval_gate 打分器使用）。
func recordTicketGate(ctx context.Context, st *grader.ApprovalState, a *app.App, scope model.Scope, ticketID uint64) error {
	repo := repository.ApprovalRepo{}
	reasons, err := repo.TicketRiskReasons(ctx, a.DB, scope, ticketID)
	if err != nil {
		return fmt.Errorf("read risk reasons: %w", err)
	}
	denied, err := repo.TicketDeniedCodes(ctx, a.DB, scope, ticketID)
	if err != nil {
		return fmt.Errorf("read denied audits: %w", err)
	}
	st.RiskReasons, st.Denied = reasons, denied
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
	// Samples 是实际运行的样本数（不含隔离样本）。
	Samples int
	Trials  []Trial
	// Quarantined 是声明 quarantine 而未运行的样本（P1-L4 审查修复）：无 trial，不计入 N / 通过 / INFRA_ERROR，
	// Gate 0 与红线统计只看 Trials，因此不会把它们当通过；报告单列「隔离样本」。
	Quarantined []QuarantinedSample
	// Snapshot 是配置快照（L8，cmd/eval 填写；单测与 Runner 不填为零值，报告不输出快照节）。
	Snapshot Snapshot
}

// QuarantinedSample 是一个被隔离（跳过）的样本及其理由。
type QuarantinedSample struct {
	ID     string
	Suite  string
	Reason string
}

// splitQuarantined 把样本分成可运行与隔离两组，保持原顺序。
func splitQuarantined(samples []*sample.Sample) (run []*sample.Sample, quarantined []QuarantinedSample) {
	run = make([]*sample.Sample, 0, len(samples))
	for _, s := range samples {
		if reason, ok := s.QuarantineReason(); ok {
			quarantined = append(quarantined, QuarantinedSample{ID: s.ID, Suite: s.Suite, Reason: reason})
			continue
		}
		run = append(run, s)
	}
	return run, quarantined
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
	fields   FieldLister
	devices  DeviceLister
	receipts *ReceiptSteps
}

// FieldLister 列出农场全部地块 id（cross_farm_isolation 判 field_id 归属）。
type FieldLister interface {
	FieldIDs(ctx context.Context, scope model.Scope) ([]uint64, error)
}

// DeviceLister 列出农场全部设备 ID（cross_farm_isolation 判 device_id 归属）；device.Bindings 实现。
type DeviceLister interface {
	DeviceIDs(ctx context.Context, scope model.Scope) ([]string, error)
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

// WithFields 设置地块清单来源，优先于 trial 库（无库的单测 harness 用）。
func WithFields(l FieldLister) RunnerOption {
	return func(r *Runner) { r.fields = l }
}

// WithDevices 设置设备清单来源（评测与 gateway 共用 configs/mock/device.yaml 的显式绑定）。
func WithDevices(l DeviceLister) RunnerOption {
	return func(r *Runner) { r.devices = l }
}

// WithApprover 设置脚本审批人（L6）：在 Ask 返回后、读迁移与打分前执行（见 ScriptApprover）。
func WithApprover(fn ApproveFunc) RunnerOption {
	return func(r *Runner) { r.approve = fn }
}

// WithReceiptSteps 设置回执投递步骤（P1-L4 合并统一）：样本声明 fixtures.device.steps 时，在审批脚本之后
// 先排空 outbox（steps.Drain，通常为 DrainStep），再按样本推进时钟 / 投递签名回执 / 扫描（每步后再排空）。
// steps.Secret 须与装配 App 时的 Deps.ReceiptSecret 相同；未配置时回执步骤不执行（无库单测 harness）。
func WithReceiptSteps(steps ReceiptSteps) RunnerOption {
	return func(r *Runner) { r.receipts = &steps }
}

// DrainStep 是 ReceiptSteps.Drain 的标准实现：与 fixtures.dispatch 同一个排空 outbox 步骤（DrainOutbox）。
func DrainStep(ctx context.Context, a *app.App) error {
	_, err := DrainOutbox(ctx, a, drainMaxRounds)
	return err
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
// 不中断评测；ctx 取消时停止并返回已完成部分与错误。声明 quarantine 的样本不运行，记入 Experiment.Quarantined。
func (r *Runner) Run(ctx context.Context, samples []*sample.Sample, opts Options) (*Experiment, error) {
	if opts.N < 1 {
		return nil, fmt.Errorf("evalkit: n must be >= 1, got %d", opts.N)
	}
	samples, quarantined := splitQuarantined(samples)
	exp := &Experiment{
		ID:          opts.ExperimentID,
		Suites:      opts.Suites,
		Tags:        opts.Tags,
		N:           opts.N,
		StartedAt:   r.now(),
		Samples:     len(samples),
		Trials:      make([]Trial, 0, len(samples)*opts.N),
		Quarantined: quarantined,
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
	t.Scope = q.Scope
	if err == nil && r.approve != nil {
		if err = r.approve(ctx, a, s, q, ans); err != nil {
			err = fmt.Errorf("approver: %w", err)
		}
	}
	if err == nil {
		if err = r.runReceipts(ctx, t, a, s, q.Scope, ans.RunID); err != nil {
			err = fmt.Errorf("receipt steps: %w", err)
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
	if err := recordTransitions(ctx, t, a, q.Scope, ans.RunID); err != nil {
		return ans, err
	}
	if err := recordOutbox(ctx, t, a, q.Scope, ans); err != nil {
		return ans, err
	}
	if err := r.recordFields(ctx, t, a, q.Scope); err != nil {
		return ans, err
	}
	if err := r.recordDevices(ctx, t, q.Scope); err != nil {
		return ans, err
	}
	return ans, recordDetail(ctx, t, a, q.Scope, ans.RunID)
}

// runReceipts 执行样本的回执步骤（未配置 WithReceiptSteps 或样本未声明 fixtures.device.steps 时不动）：
// 先排空 outbox 让审批后的 order.dispatch 落地（审批脚本未排空时，如样本无 fixtures.dispatch），再 ReceiptSteps.Run；
// 须在 recordDevice / recordTransitions 之前，回执步骤里的 Cancel / QueryStatus 与状态迁移才进入 trial 观测。
func (r *Runner) runReceipts(ctx context.Context, t *Trial, a *app.App, s *sample.Sample, scope model.Scope, runID uint64) error {
	if r.receipts == nil || s.Fixtures.Device == nil || len(s.Fixtures.Device.Steps) == 0 {
		return nil
	}
	if r.receipts.Drain != nil {
		if err := r.receipts.Drain(ctx, a); err != nil {
			return fmt.Errorf("drain before steps: %w", err)
		}
	}
	return r.receipts.Run(ctx, t, a, s, scope, runID)
}

// recordDetail 从 trial 库读该 run 的完整链路（App.DB / App.RunDetail 为空或无 run 时不动）。
// 读的是槽位注入的 trial 库（agri_eval_t_{n}），与生产库隔离。
func recordDetail(ctx context.Context, t *Trial, a *app.App, scope model.Scope, runID uint64) error {
	if a.DB == nil || a.RunDetail == nil || runID == 0 {
		return nil
	}
	d, err := a.RunDetail.Get(ctx, scope, runID)
	if err != nil {
		return fmt.Errorf("record run detail: %w", err)
	}
	t.Detail = &d
	return nil
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

// recordOutbox（P1-L4-A）读该 run 作业单的全部 outbox 消息（App.DB 为空或无作业单时不动；有单无消息时是空切片）。
// 作业单取 Answer.Order，无单时取审批票据指向的单（同 recordApproval）。
func recordOutbox(ctx context.Context, t *Trial, a *app.App, scope model.Scope, ans agent.Answer) error {
	if a.DB == nil {
		return nil
	}
	var orderID uint64
	switch {
	case ans.Order != nil:
		orderID = ans.Order.OrderID
	case t.Approval != nil && t.Approval.Ticket != nil:
		orderID = t.Approval.Ticket.OrderID
	}
	if orderID == 0 {
		return nil
	}
	rows, err := repository.OutboxRepo{}.OutboxByAggregate(ctx, a.DB, scope, orderID)
	if err != nil {
		return fmt.Errorf("record outbox: %w", err)
	}
	t.Outbox = make([]grader.OutboxRecord, 0, len(rows))
	for _, r := range rows {
		t.Outbox = append(t.Outbox, grader.OutboxRecord{Topic: r.Topic, DedupKey: r.DedupKey, Status: r.Status})
	}
	return nil
}

// recordFields 读提问农场的全部地块 id：WithFields 优先，否则读 trial 库（都没有时不动）。
func (r *Runner) recordFields(ctx context.Context, t *Trial, a *app.App, scope model.Scope) error {
	src := r.fields
	if src == nil && a.DB != nil {
		src = repository.FieldDirectory{DB: a.DB}
	}
	if src == nil {
		return nil
	}
	ids, err := src.FieldIDs(ctx, scope)
	if err != nil {
		return fmt.Errorf("record fields: %w", err)
	}
	t.Fields = ids
	return nil
}

// recordDevices 读提问农场的全部设备 ID（未配置 WithDevices 时不动）。
func (r *Runner) recordDevices(ctx context.Context, t *Trial, scope model.Scope) error {
	if r.devices == nil {
		return nil
	}
	ids, err := r.devices.DeviceIDs(ctx, scope)
	if err != nil {
		return fmt.Errorf("record devices: %w", err)
	}
	t.Devices = ids
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
