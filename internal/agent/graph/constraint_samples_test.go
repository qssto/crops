package graph_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/grader/code"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const suiteConstraint = "constraint"

// reviewGate 是带真实约束复核的 ApprovalGate（同 service.ApprovalService.Open 的拦截判定，不落库）：
// 拦截返回 *agent.ConstraintBlockedError，否则开一张 PREPARING 票。
type reviewGate struct{ rv *constraint.Reviewer }

func (g reviewGate) Open(_ context.Context, _ model.Scope, req agent.ApprovalRequest) (model.ApprovalTicket, error) {
	if _, blocked := g.rv.Review(req.Order); len(blocked) > 0 {
		return model.ApprovalTicket{}, &agent.ConstraintBlockedError{OrderID: req.Order.OrderID, RuleIDs: blocked}
	}
	return model.ApprovalTicket{TicketID: req.Order.OrderID, OrderID: req.Order.OrderID, Status: model.TicketPreparing}, nil
}

func (reviewGate) Activate(context.Context, model.Scope, uint64) (model.ApprovalTicket, error) {
	return model.ApprovalTicket{}, agent.ErrNotActivatable
}

// newReviewer 用 harness 的注册表实例构造 Reviewer（主链路生成校验行与开票前复核同一注册表）。
func (h *harness) newReviewer(t *testing.T) *constraint.Reviewer {
	t.Helper()
	b, err := os.ReadFile(repoRoot + "/policy/regulation_map.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg := h.reg
	regs, err := constraint.ParseRegulationMap(b, reg)
	if err != nil {
		t.Fatal(err)
	}
	return constraint.NewReviewer(reg, regs)
}

// constraintRun 是一个样本经 reviewGate 运行的结果。
type constraintRun struct {
	ans   agent.Answer
	tools *env.FixtureMCP
}

// runWithReview 用 reviewGate + 样本 fixtures.local_orders（未声明为空：新 trial 库无本地单）运行样本。
func (h *harness) runWithReview(t *testing.T, s *sample.Sample, gate reviewGate) constraintRun {
	t.Helper()
	clk := sampleClock(t, s)
	tools := fixtureTools(t, s, clk)
	local, err := env.NewFixtureLocalOrders(s.Fixtures.LocalOrders)
	if err != nil {
		t.Fatalf("%s: %v", s.ID, err)
	}
	a := h.newAgent(t, clk, deps{tools: tools, store: newMemStore(), cfg: func(c *graph.Config) {
		c.Gate, c.LocalOrders = gate, local
	}})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: ask: %v", s.ID, err)
	}
	return constraintRun{ans: ans, tools: tools}
}

// approvalOf 由回答推出审批观测（无库）：开了票即 PENDING_APPROVAL，否则作业单留 DRAFT。
func approvalOf(ans agent.Answer) *grader.ApprovalState {
	st := &grader.ApprovalState{Ticket: ans.Ticket}
	switch {
	case ans.Ticket != nil:
		st.OrderStatus = model.OrderPendingApproval
	case ans.Order != nil:
		st.OrderStatus = model.OrderDraft
	}
	return st
}

// failedChecks 列出作业单上未通过的约束行（rule_id: observed），供失败输出定位。
func failedChecks(ans agent.Answer) string {
	if ans.Order == nil {
		return fmt.Sprintf("no order (outcome %s, reason %s)", ans.Outcome, ans.Card.RefuseReason)
	}
	var b strings.Builder
	for _, c := range ans.Order.ConstraintChecks {
		if !c.Passed {
			fmt.Fprintf(&b, "%s: %s; ", c.RuleID, c.Observed)
		}
	}
	return b.String()
}

// TestConstraint_Samples（P1 §6.4 功能任务 7）：全部样本经真实 Reviewer 判定，并由 constraint_blocked 打分——
// 断言拦截的样本须 CONSTRAINT_BLOCKED + rule_id、无票、单留 DRAFT、设备零调用；未断言 expect.constraint 的样本
// 一律不得被拦截（P1-L3 审查 7：不论是否断言了审批）。constraint 套件另判 outcome，断言了作业单的须开票。
func TestConstraint_Samples(t *testing.T) {
	h := newHarness(t)
	gate := reviewGate{rv: h.newReviewer(t)}
	var cs, unexpected int
	for _, s := range loadSamples(t) {
		r := h.runWithReview(t, s, gate)
		if s.Expect.Constraint == nil && len(r.ans.ConstraintBlocked) > 0 {
			unexpected++
		}
		gradeConstraint(t, s, r)
		if s.Suite != suiteConstraint {
			continue
		}
		cs++
		checkConstraintSample(t, s, r)
	}
	if cs < 20 {
		t.Errorf("constraint suite has %d samples, want >= 20", cs)
	}
	t.Logf("constraint samples=%d unexpected blocks=%d", cs, unexpected)
}

// gradeConstraint 用 constraint_blocked 打分（未断言的样本被拦截即不通过）。
func gradeConstraint(t *testing.T, s *sample.Sample, r constraintRun) {
	t.Helper()
	res, err := code.ConstraintBlocked{}.Grade(context.Background(), &grader.Trial{Sample: s, Approval: approvalOf(r.ans),
		DeviceCalls: []string{}, ToolCalls: r.tools.CallCounts(), ConstraintBlocked: r.ans.ConstraintBlocked})
	if err != nil || !res.Pass {
		t.Errorf("%s: constraint_blocked: %v %s [%s]", s.ID, err, res.Evidence, failedChecks(r.ans))
	}
}

// checkConstraintSample 判一个 constraint 套件样本：outcome 一致；未断言拦截且断言了作业单的须开票。
func checkConstraintSample(t *testing.T, s *sample.Sample, r constraintRun) {
	t.Helper()
	if got := r.ans.Outcome; got != s.Expect.Outcome {
		t.Errorf("%s: outcome %s, want %s [%s]", s.ID, got, s.Expect.Outcome, failedChecks(r.ans))
	}
	if s.Expect.Constraint == nil && s.Expect.WorkOrder != nil && r.ans.Ticket == nil {
		t.Errorf("%s: want ticket, ticket <nil> [%s]", s.ID, failedChecks(r.ans))
	}
}

// TestConstraint_SampleRuleIDsRegistered：样本断言的 rule_id 均已注册（或为 reg: 前缀的法规条目）。
func TestConstraint_SampleRuleIDsRegistered(t *testing.T) {
	reg := constraint.Default()
	for _, s := range loadSamples(t) {
		if s.Expect.Constraint == nil {
			continue
		}
		for _, id := range s.Expect.Constraint.RuleIDs {
			if !reg.Has(id) && !strings.HasPrefix(id, constraint.UnmappedPrefix) && !slices.Contains(
				[]string{constraint.RuleRegistered, constraint.RuleNoPlan}, id) {
				t.Errorf("%s: rule_id %s not registered", s.ID, id)
			}
		}
	}
}
