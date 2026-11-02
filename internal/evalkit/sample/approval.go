package sample

import (
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/model"
)

// RuleApproval 校验 P1 L5 审批样本：expect.approval 的结构与 fixtures.approver 反例脚本的对应。
const RuleApproval = "approval_shape"

// 审批闸门的拒绝码（与 service.ErrRoleInsufficient / ErrSameApprover / ErrRejectReasonInvalid 的文本一致；
// 样本层不 import service）。
const (
	DeniedRoleInsufficient    = "ROLE_INSUFFICIENT"
	DeniedSameApprover        = "SAME_APPROVER"
	DeniedRejectReasonInvalid = "REJECT_REASON_INVALID"
)

// ApprovalExpect 是审批期望（approval_gate 打分器使用）：
//   - Denied：脚本审批人的决策被闸门拒绝的错误码（该票须有 audit_log(APPROVAL, DENIED) 且 summary.code 一致）；
//   - TicketStatus：该 run 最新一张 ORDER 票的状态；
//   - RiskLevel / RiskReasons：开票定级（risk_reasons 须包含所列各项，顺序不限）。
type ApprovalExpect struct {
	Denied       string   `yaml:"denied,omitempty"`
	TicketStatus string   `yaml:"ticket_status,omitempty"`
	RiskLevel    string   `yaml:"risk_level,omitempty"`
	RiskReasons  []string `yaml:"risk_reasons,omitempty"`
}

// deniedScripts 是每个拒绝码对应的反例脚本判定（approve_as:<role> 以前缀判定）。
var deniedScripts = map[string]func(approver string) bool{
	DeniedRoleInsufficient:    func(v string) bool { return strings.HasPrefix(v, "approve_as:") },
	DeniedSameApprover:        func(v string) bool { return v == "approve_same_twice" },
	DeniedRejectReasonInvalid: func(v string) bool { return v == "reject_no_reason" },
}

var (
	knownTicketStatus = []model.TicketStatus{model.TicketPreparing, model.TicketOpen, model.TicketApproved,
		model.TicketRejected, model.TicketExpired, model.TicketSuperseded}
	knownRiskLevels = []model.RiskLevel{model.RiskLow, model.RiskHigh}
)

// checkApproval：expect.approval 须配 outcome=ADVICE 与 work_order、至少断言一项；denied 取值已知且与反例脚本对应
// （ROLE_INSUFFICIENT ↔ approve_as:<role>、SAME_APPROVER ↔ approve_same_twice、REJECT_REASON_INVALID ↔ reject_no_reason）；
// ticket_status / risk_level 取 model 枚举；risk_reasons 为 PESTICIDE / LARGE_AREA / NEAR_THRESHOLD:<rule_id>，
// 有原因时 risk_level 不得为 LOW（任一原因即 HIGH）。
func checkApproval(s *Sample) []string {
	a := s.Expect.Approval
	if a == nil {
		return nil
	}
	var msgs []string
	if s.Expect.Outcome != model.OutcomeAdvice || s.Expect.WorkOrder == nil {
		msgs = append(msgs, "expect.approval requires outcome=ADVICE and expect.work_order")
	}
	if a.Denied == "" && a.TicketStatus == "" && a.RiskLevel == "" && len(a.RiskReasons) == 0 {
		msgs = append(msgs, "expect.approval asserts nothing")
	}
	if a.Denied != "" {
		match, ok := deniedScripts[a.Denied]
		switch {
		case !ok:
			msgs = append(msgs, fmt.Sprintf("expect.approval.denied %q is not allowed", a.Denied))
		case !match(s.Fixtures.Approver):
			msgs = append(msgs, fmt.Sprintf("expect.approval.denied %s does not match fixtures.approver %q", a.Denied, s.Fixtures.Approver))
		}
	}
	if a.TicketStatus != "" && !slices.Contains(knownTicketStatus, model.TicketStatus(a.TicketStatus)) {
		msgs = append(msgs, fmt.Sprintf("expect.approval.ticket_status %q is not allowed", a.TicketStatus))
	}
	if a.RiskLevel != "" && !slices.Contains(knownRiskLevels, model.RiskLevel(a.RiskLevel)) {
		msgs = append(msgs, fmt.Sprintf("expect.approval.risk_level %q is not allowed", a.RiskLevel))
	}
	for i, r := range a.RiskReasons {
		rule, near := strings.CutPrefix(r, model.RiskReasonNearThresholdPrefix)
		if !(r == model.RiskReasonPesticide || r == model.RiskReasonLargeArea || (near && rule != "")) {
			msgs = append(msgs, fmt.Sprintf("expect.approval.risk_reasons[%d] %q is not allowed", i, r))
		}
	}
	if len(a.RiskReasons) > 0 && a.RiskLevel == string(model.RiskLow) {
		msgs = append(msgs, "expect.approval.risk_reasons requires risk_level HIGH")
	}
	return msgs
}
