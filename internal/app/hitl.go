package app

import (
	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/harness/memory"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// HITLDeps 是 L6（HITL 中断恢复 + 记忆）的可选依赖。Objects 为空时不启用检查点：run 不在审批点中断、Resume 不可用；
// Memory.Enabled 为 false 时记忆注入关闭（验收 4：关闭后链路正常）；MemorySource 为空时读业务库（评测注入 fixture）。
type HITLDeps struct {
	Objects      repository.ObjectGetter // 超限检查点从 raw_store 读回（生产：repository.MinIOGetter）
	RawBucket    string                  // 与 RawStore 同一 bucket
	Memory       memory.Options
	MemorySource memory.Source
}

// HITL 是 L6 装配结果。Checkpoints 可空；Decisions / Scanner 始终装配（无票据时扫描为空转）；
// Recheck 在主链路实现 agent.Rechecker 时装配（下发前复检，L8 的 order.dispatch worker 调用）。
type HITL struct {
	Checkpoints *repository.CheckpointRepo
	Memory      *memory.Store
	Decisions   *service.DecisionService
	Scanner     *service.Scanner
	Recheck     *service.RecheckService
	decisions   *repository.DecisionRepo
}

// buildHITLStores 装配主链路需要的检查点与记忆存储（须在 NewAgent 之前）。
func buildHITLStores(deps Deps) *HITL {
	src := deps.HITL.MemorySource
	if src == nil {
		src = repository.NewMemoryRepo(deps.DB)
	}
	h := &HITL{
		Memory:    memory.New(src, deps.HITL.Memory),
		decisions: repository.NewDecisionRepo(deps.DB, deps.Clock),
	}
	if deps.HITL.Objects != nil {
		h.Checkpoints = repository.NewCheckpointRepo(deps.DB, deps.Clock, deps.RawStore, deps.HITL.Objects, deps.HITL.RawBucket)
	}
	return h
}

// buildHITLServices 在主链路就绪后装配审批决策与扫描任务；主链路实现 agent.Resumer 时作为续跑入口。
// 扫描的 Activate（ticket_activate）走 L5 的 ApprovalService。
func (a *App) buildHITLServices() {
	resumer, _ := a.Agent.(agent.Resumer)
	idem := repository.NewRedisIdem(a.Redis)
	a.HITL.Decisions = service.NewDecisionService(service.DecisionDeps{Store: a.HITL.decisions, Orders: a.WorkOrders,
		Events: a.eventRepo, Idem: idem, Locks: a.Locker, Keys: a.Keys, Resumer: resumer,
		Clock: a.Clock, Logger: a.Logger})
	a.HITL.Scanner = service.NewScanner(service.ScanDeps{Tickets: a.HITL.decisions, Runs: a.Runs,
		Expirer: a.HITL.Decisions, Orders: a.WorkOrders, Resumer: resumer, Gate: a.Approvals, Locks: a.Locker,
		Keys: a.Keys, Clock: a.Clock, Logger: a.Logger})
	if rc, ok := a.Agent.(agent.Rechecker); ok {
		a.HITL.Recheck = service.NewRecheckService(service.RecheckDeps{Rechecker: rc, Tx: a.HITL.decisions,
			Orders: a.WorkOrders, Starter: a.Ask, Locks: a.Locker, Keys: a.Keys, Logger: a.Logger})
	}
}

// withHITL 把 L6 存储注入主链路配置；未启用检查点时保持 Checkpoints / Resumes 为空（避免带类型的 nil 接口）。
func (h *HITL) withHITL(cfg *graph.Config, runs *repository.RunRepo) {
	cfg.Memory = h.Memory
	if h.Checkpoints != nil {
		cfg.Checkpoints, cfg.Resumes = h.Checkpoints, runs
	}
}
