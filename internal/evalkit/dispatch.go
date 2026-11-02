package evalkit

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// drainMaxRounds 是一次排空最多跑的 RunOnce 轮数（每轮至多 50 条；超过即视为消息在自我重排，报错）。
const drainMaxRounds = 10

// errDrainNotSettled 表示排空达到轮数上限仍有到期消息。
var errDrainNotSettled = errors.New("evalkit: outbox not drained")

// DrainOutbox 按 cmd/worker 同一张处理器表（App.NewOutboxPoller）在 trial 库上反复 RunOnce，直到一轮领取 0 条；
// 处理中写入的到期消息（如复检退回的 notify.send）在下一轮领取。失败重试的消息 next_run_at 在 FrozenClock 之后，不再领取。
// 返回处理的消息总数；随后等异步 run 与审批收尾，再归还槽位。
func DrainOutbox(ctx context.Context, a *app.App, maxRounds int) (int, error) {
	if a.DB == nil {
		return 0, errors.New("evalkit: drain outbox needs App.DB")
	}
	p, err := a.NewOutboxPoller()
	if err != nil {
		return 0, fmt.Errorf("evalkit: outbox poller: %w", err)
	}
	total := 0
	for range maxRounds {
		n, err := p.RunOnce(ctx)
		total += n
		if err != nil {
			return total, fmt.Errorf("evalkit: drain outbox: %w", err)
		}
		if n == 0 {
			waitAsync(a)
			return total, nil
		}
	}
	waitAsync(a)
	return total, fmt.Errorf("%w after %d rounds (%d messages)", errDrainNotSettled, maxRounds, total)
}

func waitAsync(a *app.App) {
	if a.Ask != nil {
		a.Ask.Wait()
	}
	if a.HITL != nil {
		a.HITL.Decisions.Wait()
	}
}

// runDispatch 执行样本 fixtures.dispatch（P1-L4-A，在首次审批脚本之后，含审批未通过）：
//  1. 排空 outbox（order.dispatch → 下发器 ①-⑦；复检退回时 notify.send）；
//  2. reapprove 非空且该 run 有退回后的新票（ticket_seq > 1）时，对新票执行该脚本并再排空；
//  3. redeliver 次：把已 DONE 的 order.dispatch 放回 PENDING（模拟重复投递）并排空。
func runDispatch(ctx context.Context, a *app.App, s *sample.Sample, scope model.Scope, ans agent.Answer,
	act func(script string, ticketID uint64) error) error {
	fx := s.Fixtures.Dispatch
	if _, err := DrainOutbox(ctx, a, drainMaxRounds); err != nil {
		return err
	}
	if fx.Reapprove != "" && ans.RunID != 0 {
		tk, ok, err := (repository.ApprovalRepo{}).TicketByRun(ctx, a.DB, scope, ans.RunID)
		if err != nil {
			return fmt.Errorf("evalkit: read reopened ticket: %w", err)
		}
		if ok && tk.TicketSeq > 1 {
			if err := act(fx.Reapprove, tk.TicketID); err != nil {
				return fmt.Errorf("evalkit: reapprove: %w", err)
			}
			if _, err := DrainOutbox(ctx, a, drainMaxRounds); err != nil {
				return err
			}
		}
	}
	for i := range fx.Redeliver {
		if ans.Order == nil {
			return errors.New("evalkit: redeliver needs a work order")
		}
		n, err := (repository.OutboxRepo{}).RedeliverDone(ctx, a.DB, scope, ans.Order.OrderID,
			repository.TopicOrderDispatch, a.Clock.Now())
		if err != nil {
			return fmt.Errorf("evalkit: redeliver #%d: %w", i+1, err)
		}
		if n == 0 {
			return fmt.Errorf("evalkit: redeliver #%d: no DONE %s for order %d", i+1, repository.TopicOrderDispatch, ans.Order.OrderID)
		}
		if _, err := DrainOutbox(ctx, a, drainMaxRounds); err != nil {
			return err
		}
	}
	return nil
}

// newApprover 按脚本串构造 ScriptedApprover（首审与重审共用）。
func newApprover(a *app.App, clk *env.FrozenClock, script string, first, second env.Identity) (*env.ScriptedApprover, error) {
	sc, err := env.ParseApproverScript(script)
	if err != nil {
		return nil, err
	}
	return env.NewScriptedApprover(sc, env.ApproverDeps{Clock: clk, Decider: a.HITL.Decisions,
		Sweeper: a.HITL.Scanner, First: first, Second: second})
}
