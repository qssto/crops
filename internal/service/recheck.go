package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// TxRunner 开一个业务库事务，repository.DecisionRepo 实现。
type TxRunner interface {
	InTx(ctx context.Context, fn func(tx *repository.DB) error) error
}

// RunStarter 为同一问题新建 run 并异步执行，AskService 实现。
type RunStarter interface {
	Ask(ctx context.Context, scope model.Scope, in AskInput) (uint64, error)
}

// RecheckDeps 是 RecheckService 的依赖。
type RecheckDeps struct {
	Rechecker agent.Rechecker
	Tx        TxRunner
	Orders    OrderTransitioner
	Starter   RunStarter
	Locks     KeyLocker
	Keys      rediskey.Keys
	Logger    *slog.Logger
}

// RecheckService 是下发前复检（P0 §6.7 功能任务 3）：时效工具以新 epoch 重取后结论变化，
// 原作业单 CANCELED(CONDITION_CHANGED) 并为同一问题新建 run。由 L8 的 order.dispatch worker 在下发前调用。
type RecheckService struct{ d RecheckDeps }

// NewRecheckService 构造 RecheckService。
func NewRecheckService(d RecheckDeps) *RecheckService { return &RecheckService{d: d} }

// PreDispatchInput 是一次复检：Order 为待下发（APPROVED）的作业单，Question 带原 run_id 与原问题，
// Card 为原建议卡，Epoch 须大于原 run 已用过的 resume_epoch（由调用方经 RunRepo.RecheckEpoch 分配，每次复检一个新值）。
type PreDispatchInput struct {
	Order    model.OrderRef
	Question agent.Question
	Card     model.AdviceCard
	Epoch    int
	// Work 是作业单要素（order_id / field_id / 作业类型 / 设备 / 下发方式 / payload），P1 L3 复检约束取必检集合用。
	Work model.WorkOrder
	// Retry 为 true 表示 attempt > 1 的重试复检（P1 §6.5 第 8 步）：单已 DISPATCHED，不取消、不新建 run，
	// 只返回结论，由调用方 DISPATCHED → FAILED(RETRY_BLOCKED)。
	Retry bool
}

// PreDispatchResult 是复检结论：Changed 时原单已取消，NewRunID 为新建的 run（本农场 enable_advice 关时为 0）；Checks 为复检判定的 PRE_DISPATCH 约束。
// Blocked 为不通过的 PRE_DISPATCH 规则（P1 §6.4 第 4 条）：非空时拦截下发、不取消原单（由调用方走 Reopen 退回重审），
// 优先于 Changed（Changed 仍如实返回，Detail 记两者）。
type PreDispatchResult struct {
	Changed  bool
	Detail   string
	NewRunID uint64
	Checks   []model.ConstraintCheck
	Blocked  []string
}

// PreDispatch 复检并按结论处置。结论未变返回 Changed=false，调用方继续下发；
// 复检无结论（agent.ErrRecheckInconclusive）原样返回，作业单不动，由调用方稍后重试。
// 取消与新建 run 不在同一事务：取消提交后新建失败只返回错误（单已终态，重试 PreDispatch 会因非法迁移失败），由人工重新提问。
func (s *RecheckService) PreDispatch(ctx context.Context, in PreDispatchInput) (PreDispatchResult, error) {
	res, err := s.d.Rechecker.Recheck(ctx, agent.RecheckRequest{Question: in.Question, Card: in.Card, Epoch: in.Epoch,
		Order: in.Work})
	if err != nil {
		return PreDispatchResult{}, fmt.Errorf("service: recheck order %d: %w", in.Order.OrderID, err)
	}
	out := PreDispatchResult{Changed: res.Changed, Detail: res.Detail, Checks: res.Checks, Blocked: failedRules(res.Checks)}
	if len(out.Blocked) > 0 {
		s.d.Logger.WarnContext(ctx, "pre-dispatch constraints failed, dispatch blocked", "order_id", in.Order.OrderID,
			"rule_ids", out.Blocked, "verdict_changed", res.Changed, "retry", in.Retry)
		return out, nil
	}
	if !res.Changed || in.Retry {
		return out, nil
	}
	if err := s.cancel(ctx, in.Order); err != nil {
		return out, err
	}
	s.d.Logger.InfoContext(ctx, "order canceled by recheck", "order_id", in.Order.OrderID, "run_id", in.Question.RunID,
		"detail", res.Detail)
	// P2 L6：新 run 取代原 run（enable_compaction 开时接替其会话位置并复制 compaction_state，关时同 P1）
	runID, err := s.d.Starter.Ask(ctx, in.Question.Scope, AskInput{FieldID: in.Question.FieldID, Text: in.Question.Text,
		Supersedes: in.Question.RunID})
	if errors.Is(err, ErrAdviceDisabled) {
		// 本农场 enable_advice 已关：单照常取消，不生成新建议（P1 §8.1），不算失败。
		s.d.Logger.InfoContext(ctx, "recheck new run skipped, advice disabled", "order_id", in.Order.OrderID)
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("service: recheck order %d: new run: %w", in.Order.OrderID, err)
	}
	out.NewRunID = runID
	return out, nil
}

// cancel 持 lock:order 单事务把 APPROVED 作业单迁到 CANCELED(CONDITION_CHANGED)；已下发 / 执行中的单不取消（非法迁移）。
func (s *RecheckService) cancel(ctx context.Context, ref model.OrderRef) error {
	release, err := acquireOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, ref.OrderID)
	if err != nil {
		return err
	}
	defer release()
	err = s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		return s.d.Orders.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderCanceled,
			model.Actor{Type: model.ActorSystem}, model.ReasonConditionChanged)
	})
	if err != nil {
		return fmt.Errorf("service: recheck cancel order %d: %w", ref.OrderID, err)
	}
	return nil
}

// failedRules 返回不通过的约束规则 id（按出现顺序）。
func failedRules(checks []model.ConstraintCheck) []string {
	var out []string
	for _, c := range checks {
		if !c.Passed {
			out = append(out, c.RuleID)
		}
	}
	return out
}
