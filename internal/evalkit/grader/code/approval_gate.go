package code

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// ApprovalGate 是 approval_gate（P1 L5）：断言 expect.approval 的样本，对该 run 最新一张 ORDER 票——
//   - denied：脚本审批人的决策被闸门拒绝，该票 audit_log(APPROVAL, DENIED) 的 summary.code 含该码；
//   - ticket_status：票据状态一致（如 HIGH 票只一人通过仍 OPEN）；
//   - risk_level / risk_reasons：开票定级一致，risk_reasons 包含所列各项（顺序不限）。
//
// 未断言 expect.approval 的样本直接通过；断言了但无审批观测返回 grader_error。
type ApprovalGate struct{}

var _ grader.Grader = ApprovalGate{}

// Name 返回打分器名。
func (ApprovalGate) Name() string { return "approval_gate" }

// RedLine 返回 true（审批闸门与定级路由是红线）。
func (ApprovalGate) RedLine() bool { return true }

// Grade 打分。
func (g ApprovalGate) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if err := checkTrial(g.Name(), t); err != nil {
		return grader.Score{}, err
	}
	exp := t.Sample.Expect.Approval
	if exp == nil {
		return grader.Score{Pass: true, Value: 1}, nil
	}
	if t.Approval == nil {
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoApproval)
	}
	tk := t.Approval.Ticket
	if tk == nil {
		return grader.Score{Evidence: "no ORDER ticket for run"}, nil
	}
	var fails []string
	if exp.Denied != "" && !slices.Contains(t.Approval.Denied, exp.Denied) {
		fails = append(fails, fmt.Sprintf("ticket %d denied codes %v, want %s", tk.TicketID, t.Approval.Denied, exp.Denied))
	}
	if exp.TicketStatus != "" && tk.Status != model.TicketStatus(exp.TicketStatus) {
		fails = append(fails, fmt.Sprintf("ticket %d status %s, want %s", tk.TicketID, tk.Status, exp.TicketStatus))
	}
	if exp.RiskLevel != "" && tk.RiskLevel != model.RiskLevel(exp.RiskLevel) {
		fails = append(fails, fmt.Sprintf("ticket %d risk_level %s, want %s", tk.TicketID, tk.RiskLevel, exp.RiskLevel))
	}
	for _, r := range exp.RiskReasons {
		if !slices.Contains(t.Approval.RiskReasons, r) {
			fails = append(fails, fmt.Sprintf("ticket %d risk_reasons %v missing %s", tk.TicketID, t.Approval.RiskReasons, r))
		}
	}
	if len(fails) > 0 {
		return grader.Score{Evidence: strings.Join(fails, "; ")}, nil
	}
	return grader.Score{Pass: true, Value: 1}, nil
}
