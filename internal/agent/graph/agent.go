// Package graph 是建议主链路：Eino compose 线性图
// classify → enrich → load_knowledge → candidates → choose → compute → explain → assemble → validate → persist → to_order
// → await_approval（L6：有票据即 WAIT_APPROVAL 中断）→ finalize（收尾）。
// 数值只来自规则层 calc / 知识库参数，模型只做分类、在候选内选药与组织文字。
package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/calc"
	harnesscb "agri-steward/internal/agent/harness/callbacks"
	"agri-steward/internal/agent/harness/checkpoint"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// GraphVersion 是主链路版本（agent_run.graph_version）；检查点结构随之变化，Resume 要求一致。
const GraphVersion = "l6-v1"

// 默认值。
const (
	defaultOrderWindow = 24 * time.Hour
	chinaOffsetSec     = 8 * 3600
)

// Config 是主链路依赖。LLM、Tools、Runs、Advice、Drafts、Locker、Clock、KB、Taxonomy 必填。
type Config struct {
	LLM      *llm.Client
	Tier     llm.Tier
	Tools    agent.ToolProvider
	Runs     agent.RunStateStore
	Advice   agent.AdviceStore
	Drafts   agent.OrderDraftStore
	Locker   agent.RunLocker
	Clock    clock.Clock
	KB       *knowledge.Store
	KBRef    string // 知识版本，空取 knowledge.RefWorktree
	Taxonomy *knowledge.Taxonomy
	Crop     string // 本农场作物 code（rice）
	Region   string // 区域 code（jiangsu）
	Loc      *time.Location
	Logger   *slog.Logger

	// L5 / L6 接缝，均可空（空时退回 L4 行为）。
	Gate        agent.ApprovalGate    // L5：to_order 写 DRAFT 后开票；空则不开票
	Events      agent.EventSink       // L5：VALIDATION_FAILED 等图内事件；空则只记日志
	Checkpoints agent.CheckpointStore // L6：compose.WithCheckPointStore，只在中断时写；与 Gate 同时非空才在审批点中断
	Resumes     agent.RunResumeStore  // L6：Resume 的状态守卫；空则 Resume 不可用
	Memory      agent.MemoryStore     // L6：记忆注入；空即关闭
	Trace       callbacks.Handler     // L7：附加 Eino 回调（harness/callbacks 的 STEP_* 事件）；空则不挂

	RenewEvery  time.Duration // 租约续期间隔，0 取 runner 默认
	OrderWindow time.Duration // 建议作业窗口长度，0 取 24h
}

// Agent 是主链路实现（agent.Agent）。
type Agent struct {
	cfg   Config
	loc   *time.Location
	log   *slog.Logger
	run   *runner.Runner
	graph compose.Runnable[*state, *state]
}

var (
	_ agent.Agent   = (*Agent)(nil)
	_ agent.Resumer = (*Agent)(nil)
)

// ErrResumeUnsupported 表示未配置 Checkpoints / Resumes 却调用 Resume；包装 agent.ErrNotResumable，扫描据此收尾而非无限重试。
var ErrResumeUnsupported = fmt.Errorf("graph: resume not configured: %w", agent.ErrNotResumable)

// New 校验依赖并编译图。
func New(ctx context.Context, cfg Config) (*Agent, error) {
	var missing []string
	for name, nilDep := range map[string]bool{
		"LLM": cfg.LLM == nil, "Tools": cfg.Tools == nil, "Runs": cfg.Runs == nil, "Advice": cfg.Advice == nil,
		"Drafts": cfg.Drafts == nil, "Locker": cfg.Locker == nil, "Clock": cfg.Clock == nil, "KB": cfg.KB == nil,
		"Taxonomy": cfg.Taxonomy == nil, "Crop": cfg.Crop == "", "Region": cfg.Region == "",
	} {
		if nilDep {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("graph: missing config %v", missing)
	}
	if !cfg.LLM.HasTier(cfg.Tier) {
		return nil, fmt.Errorf("graph: %w: %q", llm.ErrUnknownTier, cfg.Tier)
	}
	if cfg.KBRef == "" {
		cfg.KBRef = knowledge.RefWorktree
	}
	if cfg.OrderWindow <= 0 {
		cfg.OrderWindow = defaultOrderWindow
	}
	a := &Agent{cfg: cfg, loc: cfg.Loc, log: cfg.Logger}
	if a.loc == nil {
		a.loc = time.FixedZone("CST", chinaOffsetSec)
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	r, err := runner.New(runner.Config{Locker: cfg.Locker, Runs: cfg.Runs, Clock: cfg.Clock,
		RenewEvery: cfg.RenewEvery, Logger: a.log, Gate: cfg.Gate, Resumes: cfg.Resumes, Events: cfg.Events})
	if err != nil {
		return nil, fmt.Errorf("graph: %w", err)
	}
	a.run = r
	if a.graph, err = a.compile(ctx); err != nil {
		return nil, fmt.Errorf("graph: compile: %w", err)
	}
	return a, nil
}

// node 是一个图节点。
type node = func(context.Context, *state) (*state, error)

// compile 按 graphNodes 把节点串成线性图；配置了 Checkpoints 时挂检查点存储（只在中断时写）。
func (a *Agent) compile(ctx context.Context) (compose.Runnable[*state, *state], error) {
	fns := map[string]node{
		NodeClassify: a.classify, NodeEnrich: a.enrich, NodeKnowledge: a.loadKnowledge, NodeCands: a.candidates,
		NodeChoose: a.choose, NodeCompute: a.compute, NodeExplain: a.explain, NodeAssemble: a.assemble,
		NodeValidate: a.validate, NodePersist: a.persist, NodeToOrder: a.toOrder,
		NodeAwaitApproval: a.awaitApproval, NodeFinalize: a.finalize,
	}
	g := compose.NewGraph[*state, *state]()
	prev := compose.START
	for _, name := range graphNodes() {
		if err := g.AddLambdaNode(name, compose.InvokableLambda(fns[name]), compose.WithNodeName(name)); err != nil {
			return nil, fmt.Errorf("add node %s: %w", name, err)
		}
		if err := g.AddEdge(prev, name); err != nil {
			return nil, fmt.Errorf("edge %s->%s: %w", prev, name, err)
		}
		prev = name
	}
	if err := g.AddEdge(prev, compose.END); err != nil {
		return nil, fmt.Errorf("edge %s->end: %w", prev, err)
	}
	opts := []compose.GraphCompileOption{compose.WithGraphName("advice")}
	if a.cfg.Checkpoints != nil {
		opts = append(opts, compose.WithCheckPointStore(checkpoint.New(a.cfg.Checkpoints)))
	}
	return g.Compile(ctx, opts...)
}

// invokeOpts 是一次 Invoke 的选项：runner 回调（+ L7 Trace）+ 配置了检查点时的 checkpoint_id（= run_id）。
func (a *Agent) invokeOpts(runID uint64, cb callbacks.Handler) []compose.Option {
	hs := []callbacks.Handler{cb}
	if a.cfg.Trace != nil {
		hs = append(hs, a.cfg.Trace)
	}
	opts := []compose.Option{compose.WithCallbacks(hs...)}
	if a.cfg.Checkpoints != nil {
		opts = append(opts, compose.WithCheckPointID(checkpoint.ID(runID)))
	}
	return opts
}

// Meta 是本主链路写入 agent_run 的版本信息（AskService 建 run 时同样使用）。
func (a *Agent) Meta() agent.RunMeta {
	return agent.RunMeta{GraphVersion: GraphVersion, PromptVersion: a.cfg.LLM.PromptVersion(),
		ModelTier: string(a.cfg.Tier), KnowledgeRef: a.cfg.KBRef}
}

// stepGraphError 是图运行错误时 run 的原因码。
const stepGraphError = "GRAPH_ERROR"

// Ask 跑一次主链路：RunID 为 0 时先建 run；在 runner 的执行权（租约 + 心跳）内运行图，
// 节点落 run_step；拒答也产出并落库拒答卡，run 以 COMPLETED 收尾。运行错误（含租约丢失）返回 error。
// L6：作业单开票后在 await_approval 中断：检查点落库同事务置 run INTERRUPTED(WAIT_APPROVAL)，runner 随后激活票据；
// 返回的 Answer 带 Card / Order / Ticket，run 对用户即「待审批」。
func (a *Agent) Ask(ctx context.Context, q agent.Question) (agent.Answer, error) {
	runID := q.RunID
	if runID == 0 {
		id, err := a.cfg.Runs.CreateRun(ctx, model.RunState{TenantID: q.Scope.TenantID, FarmID: q.Scope.FarmID},
			q.Scope.UserID, q.FieldID, a.Meta())
		if err != nil {
			return agent.Answer{}, fmt.Errorf("graph: create run: %w", err)
		}
		runID = id
	}
	var final *state
	out, err := a.run.Run(ctx, q.Scope, runID, func(rctx context.Context, cb callbacks.Handler) (out runner.Outcome, err error) {
		defer recoverOutcome(&out, &err)
		rctx = harnesscb.WithRun(rctx, harnesscb.Run{Scope: q.Scope, RunID: runID})
		s, err := a.newState(rctx, q, runID)
		if err != nil {
			return runner.Outcome{ReasonCode: stepGraphError}, err
		}
		final = s // 节点原地修改同一 *state：中断时 Invoke 无返回值，卡片 / 单 / 票据仍在 s 上
		a.emitRunStarted(rctx, s)
		tctx := agent.WithCheckpointTarget(rctx, agent.CheckpointTarget{Scope: q.Scope, RunID: runID,
			Interrupt: model.InterruptWaitApproval})
		res, err := a.graph.Invoke(tctx, s, a.invokeOpts(runID, cb)...)
		if _, ok := compose.ExtractInterruptInfo(err); ok {
			return runner.Outcome{Status: model.RunInterrupted, Interrupt: model.InterruptWaitApproval}, nil
		}
		if err != nil {
			return runner.Outcome{ReasonCode: stepGraphError}, err
		}
		final = res
		if final.refuse != "" {
			a.log.InfoContext(rctx, "advice refused", "run_id", runID, "reason", final.refuse, "detail", final.detail)
		}
		return runner.Outcome{Status: model.RunCompleted, ReasonCode: final.refuse}, nil
	})
	if err != nil {
		return agent.Answer{RunID: runID}, fmt.Errorf("graph: run %d: %w", runID, err)
	}
	ans, err := answerOf(runID, final)
	if err != nil {
		return ans, err
	}
	if out.Status == model.RunInterrupted && out.Ticket != nil {
		ans.Ticket = out.Ticket // 已激活（OPEN）；激活失败时保留开票结果（PREPARING），由 ticket_activate 补偿
	}
	return ans, nil
}

// Resume 从审批点续跑（agent.Resumer，P0 §6.7 流程 3）：先确认检查点存在（否则 ErrNotResumable，不整图重跑），
// 再经 runner.Resume 持锁、MarkResumed 守卫置 RUNNING 且 resume_epoch+1，Eino 按检查点恢复：
// classify … to_order 不重跑，await_approval 由 interrupt state 重建状态，finalize 收尾后 run 置 COMPLETED。
// 不调设备、不做复检；失败时 run 留在 RUNNING，由 stuck_run / run_finalize 扫描补偿。
func (a *Agent) Resume(ctx context.Context, scope model.Scope, runID uint64) (agent.Answer, error) {
	if a.cfg.Checkpoints == nil || a.cfg.Resumes == nil {
		return agent.Answer{RunID: runID}, ErrResumeUnsupported
	}
	// 只放归属、不放中断类型：CheckpointRepo.Get 据此按租户 / 农场取检查点；续跑若再次 Set 即被检查点适配器拒绝
	ctx = agent.WithCheckpointTarget(ctx, agent.CheckpointTarget{Scope: scope, RunID: runID})
	if _, ok, err := a.cfg.Checkpoints.Get(ctx, checkpoint.ID(runID)); err != nil {
		return agent.Answer{RunID: runID}, fmt.Errorf("graph: resume run %d: load checkpoint: %w", runID, err)
	} else if !ok {
		return agent.Answer{RunID: runID}, fmt.Errorf("graph: resume run %d: no checkpoint: %w", runID, agent.ErrNotResumable)
	}
	var final *state
	_, err := a.run.Resume(ctx, scope, agent.ResumeRequest{RunID: runID, GraphVersion: GraphVersion},
		func(rctx context.Context, cb callbacks.Handler) (out runner.Outcome, err error) {
			defer recoverOutcome(&out, &err)
			rctx = harnesscb.WithRun(rctx, harnesscb.Run{Scope: scope, RunID: runID})
			res, err := a.graph.Invoke(rctx, nil, a.invokeOpts(runID, cb)...)
			if err != nil {
				return runner.Outcome{ReasonCode: stepGraphError}, err
			}
			final = res
			return runner.Outcome{Status: model.RunCompleted}, nil
		})
	if err != nil {
		return agent.Answer{RunID: runID}, fmt.Errorf("graph: resume run %d: %w", runID, err)
	}
	return answerOf(runID, final)
}

// recoverOutcome 把执行体 panic 转为 GRAPH_ERROR 错误（run 记 FAILED 或由扫描补偿）。
func recoverOutcome(out *runner.Outcome, err *error) {
	if p := recover(); p != nil {
		*out, *err = runner.Outcome{ReasonCode: stepGraphError}, fmt.Errorf("graph panic: %v", p)
	}
}

// answerOf 由最终状态组装 Answer。
func answerOf(runID uint64, final *state) (agent.Answer, error) {
	if final == nil || final.card == nil {
		return agent.Answer{RunID: runID}, errors.New("graph: run finished without card")
	}
	return agent.Answer{Outcome: final.card.Outcome, RefuseReason: final.card.RefuseReason, RunID: runID,
		Card: final.card, Order: final.order, Ticket: final.ticket}, nil
}

// newState 打开知识版本并构造本次 run 的状态。
func (a *Agent) newState(ctx context.Context, q agent.Question, runID uint64) (*state, error) {
	snap, err := a.cfg.KB.Open(ctx, a.cfg.KBRef)
	if err != nil {
		return nil, fmt.Errorf("open knowledge %s: %w", a.cfg.KBRef, err)
	}
	c, err := calc.New(snap)
	if err != nil {
		return nil, fmt.Errorf("calc: %w", err)
	}
	e := &env{snap: snap, tx: a.cfg.Taxonomy, calc: c, loc: a.loc, crop: a.cfg.Crop, region: a.cfg.Region}
	return &state{q: q, runID: runID, now: a.cfg.Clock.Now(), env: e}, nil
}
