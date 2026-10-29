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
	"agri-steward/internal/service"
)

// ApproveFunc 在 Ask 返回后对该 run 的票据执行样本 fixtures.approver（L6）。
type ApproveFunc func(ctx context.Context, a *app.App, s *sample.Sample, q agent.Question, ans agent.Answer) error

// ScriptApprover 返回按 fixtures.approver 驱动 DecisionService / Scanner 的 ApproveFunc（P0 §6.7 功能任务 7）：
//  1. 样本无 approver、断言 stop_at_approval（观测审批点，L5）或 run 未开票时不动作；
//  2. 票据 PREPARING 时先跑一轮扫描（ticket_activate）转 OPEN；
//  3. env.ScriptedApprover 执行通过 / 驳回 / 超时 / approve_after，HIGH 票由 second 二次确认；
//  4. 通过后等异步 Resume 收尾，再按下发前复检（RecheckService.PreDispatch，epoch 由 RecheckEpoch 分配）处置：
//     条件变化时原单 CANCELED(CONDITION_CHANGED)，并等新建 run 结束。复检无结论不算错误（作业单保持 APPROVED）。
//
// App.Clock 须为 *env.FrozenClock（PoolEnv 槽位时钟）。
func ScriptApprover(first, second env.Identity) ApproveFunc {
	return func(ctx context.Context, a *app.App, s *sample.Sample, q agent.Question, ans agent.Answer) error {
		if s.Fixtures.Approver == "" || s.Expect.Trajectory.StopAtApproval || ans.Ticket == nil || a.HITL == nil {
			return nil
		}
		clk, ok := a.Clock.(*env.FrozenClock)
		if !ok {
			return errors.New("evalkit: approver needs env.FrozenClock")
		}
		script, err := env.ParseApproverScript(s.Fixtures.Approver)
		if err != nil {
			return err
		}
		approver, err := env.NewScriptedApprover(script, env.ApproverDeps{Clock: clk, Decider: a.HITL.Decisions,
			Sweeper: a.HITL.Scanner, First: first, Second: second})
		if err != nil {
			return err
		}
		tk, err := openTicket(ctx, a, q.Scope, ans.Ticket.TicketID)
		if err != nil {
			return err
		}
		approved, err := approver.Act(ctx, q.Scope, tk)
		a.HITL.Decisions.Wait()
		if err != nil || !approved {
			return err
		}
		return recheckBeforeDispatch(ctx, a, q, ans)
	}
}

// openTicket 读票据；PREPARING 时跑一轮扫描激活后重读，仍非 OPEN 返回错误。
func openTicket(ctx context.Context, a *app.App, scope model.Scope, ticketID uint64) (env.Ticket, error) {
	repo := repository.NewDecisionRepo(a.DB, a.Clock)
	t, err := repo.TicketTx(ctx, nil, scope, ticketID)
	if err != nil {
		return env.Ticket{}, fmt.Errorf("evalkit: read ticket: %w", err)
	}
	if model.TicketStatus(t.Status) == model.TicketPreparing {
		if err := a.HITL.Scanner.RunOnce(ctx); err != nil {
			return env.Ticket{}, fmt.Errorf("evalkit: activate ticket %d: %w", ticketID, err)
		}
		if t, err = repo.TicketTx(ctx, nil, scope, ticketID); err != nil {
			return env.Ticket{}, fmt.Errorf("evalkit: read ticket: %w", err)
		}
	}
	if model.TicketStatus(t.Status) != model.TicketOpen || t.ExpiresAt == nil {
		return env.Ticket{}, fmt.Errorf("evalkit: ticket %d is %s, want OPEN", ticketID, t.Status)
	}
	return env.Ticket{ID: t.ID, Digest: t.SnapshotDigest, ExpiresAt: *t.ExpiresAt}, nil
}

// recheckBeforeDispatch 模拟 L8 order.dispatch worker 下发前的复检；主链路不支持复检时跳过。
func recheckBeforeDispatch(ctx context.Context, a *app.App, q agent.Question, ans agent.Answer) error {
	if a.HITL.Recheck == nil || ans.Card == nil || ans.Order == nil {
		return nil
	}
	epoch, err := a.Runs.RecheckEpoch(ctx, q.Scope, ans.RunID)
	if err != nil {
		return err
	}
	q.RunID = ans.RunID
	res, err := a.HITL.Recheck.PreDispatch(ctx, service.PreDispatchInput{Card: *ans.Card, Question: q, Epoch: epoch,
		Order: model.OrderRef{TenantID: q.Scope.TenantID, FarmID: q.Scope.FarmID, OrderID: ans.Order.OrderID}})
	if errors.Is(err, agent.ErrRecheckInconclusive) {
		return nil
	}
	if res.Changed {
		a.Ask.Wait() // 新建 run 异步执行，等其结束再归还槽位
	}
	return err
}
