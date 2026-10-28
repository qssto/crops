// Package graph 是建议主链路：Eino compose 线性图
// classify → enrich → load_knowledge → candidates → choose → compute → explain → assemble → validate → persist → to_order。
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
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// GraphVersion 是主链路版本（agent_run.graph_version）。
const GraphVersion = "l4a-v1"

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

var _ agent.Agent = (*Agent)(nil)

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
		RenewEvery: cfg.RenewEvery, Logger: a.log})
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

// compile 按 nodeOrder 把节点串成线性图。
func (a *Agent) compile(ctx context.Context) (compose.Runnable[*state, *state], error) {
	fns := map[string]node{
		NodeClassify: a.classify, NodeEnrich: a.enrich, NodeKnowledge: a.loadKnowledge, NodeCands: a.candidates,
		NodeChoose: a.choose, NodeCompute: a.compute, NodeExplain: a.explain, NodeAssemble: a.assemble,
		NodeValidate: a.validate, NodePersist: a.persist, NodeToOrder: a.toOrder,
	}
	g := compose.NewGraph[*state, *state]()
	prev := compose.START
	for _, name := range nodeOrder {
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
	return g.Compile(ctx, compose.WithGraphName("advice"))
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
	_, err := a.run.Run(ctx, q.Scope, runID, func(rctx context.Context, cb callbacks.Handler) (out runner.Outcome, err error) {
		defer func() {
			if p := recover(); p != nil {
				out, err = runner.Outcome{ReasonCode: stepGraphError}, fmt.Errorf("graph panic: %v", p)
			}
		}()
		s, err := a.newState(rctx, q, runID)
		if err != nil {
			return runner.Outcome{ReasonCode: stepGraphError}, err
		}
		if final, err = a.graph.Invoke(rctx, s, compose.WithCallbacks(cb)); err != nil {
			return runner.Outcome{ReasonCode: stepGraphError}, err
		}
		if final.refuse != "" {
			a.log.InfoContext(rctx, "advice refused", "run_id", runID, "reason", final.refuse, "detail", final.detail)
		}
		return runner.Outcome{Status: model.RunCompleted, ReasonCode: final.refuse}, nil
	})
	if err != nil {
		return agent.Answer{RunID: runID}, fmt.Errorf("graph: run %d: %w", runID, err)
	}
	if final == nil || final.card == nil {
		return agent.Answer{RunID: runID}, errors.New("graph: run finished without card")
	}
	return agent.Answer{Outcome: final.card.Outcome, RefuseReason: final.card.RefuseReason, RunID: runID,
		Card: final.card, Order: final.order}, nil
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
