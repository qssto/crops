package graph

import (
	"context"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
)

// openTicket：作业单草稿落库后经 ApprovalGate 开票（PREPARING，按业务唯一键幂等）；Gate 为空时不开票。
// 发起人取 agent_run.user_id（= Question.Scope.UserID）。
func (a *Agent) openTicket(ctx context.Context, s *state) error {
	if a.cfg.Gate == nil || s.order == nil {
		return nil
	}
	if err := runner.Alive(ctx); err != nil {
		return err
	}
	t, err := a.cfg.Gate.Open(ctx, s.q.Scope, agent.ApprovalRequest{Order: *s.order, Card: *s.card,
		InitiatorID: s.q.Scope.UserID})
	if err != nil {
		return fmt.Errorf("open ticket: %w", err)
	}
	s.ticket = &t
	return nil
}
