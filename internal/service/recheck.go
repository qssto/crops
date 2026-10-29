package service

import (
	"context"
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
}

// PreDispatchResult 是复检结论：Changed 时原单已取消，NewRunID 为新建的 run；Checks 为复检判定的 PRE_DISPATCH 约束。
type PreDispatchResult struct {
	Changed  bool
	Detail   string
	NewRunID uint64
	Checks   []model.ConstraintCheck
}

// PreDispatch 复检并按结论处置。结论未变返回 Changed=false，调用方继续下发；
// 复检无结论（agent.ErrRecheckInconclusive）原样返回，作业单不动，由调用方稍后重试。
// 取消与新建 run 不在同一事务：取消提交后新建失败只返回错误（单已终态，重试 PreDispatch 会因非法迁移失败），由人工重新提问。
func (s *RecheckService) PreDispatch(ctx context.Context, in PreDispatchInput) (PreDispatchResult, error) {
	res, err := s.d.Rechecker.Recheck(ctx, agent.RecheckRequest{Question: in.Question, Card: in.Card, Epoch: in.Epoch})
	if err != nil {
		return PreDispatchResult{}, fmt.Errorf("service: recheck order %d: %w", in.Order.OrderID, err)
	}
	out := PreDispatchResult{Changed: res.Changed, Detail: res.Detail, Checks: res.Checks}
	if !res.Changed {
		return out, nil
	}
	if err := s.cancel(ctx, in.Order); err != nil {
		return out, err
	}
	s.d.Logger.InfoContext(ctx, "order canceled by recheck", "order_id", in.Order.OrderID, "run_id", in.Question.RunID,
		"detail", res.Detail)
	runID, err := s.d.Starter.Ask(ctx, in.Question.Scope, AskInput{FieldID: in.Question.FieldID, Text: in.Question.Text})
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
