package sample

import (
	"strings"
	"testing"
)

// TestCheckApproval（P1 L5）：expect.approval 须配 ADVICE + work_order、至少断言一项；denied 与反例脚本对应；
// ticket_status / risk_level / risk_reasons 取已知值，有原因时不得为 LOW。
func TestCheckApproval(t *testing.T) {
	approvalSample := func(approver string, a ApprovalExpect, mut func(s *Sample)) func(s []*Sample) []*Sample {
		return func(s []*Sample) []*Sample {
			s[1].Fixtures.Approver = approver
			s[1].Expect.WorkOrder = &WorkOrderExpect{FinalStatus: "PENDING_APPROVAL"}
			s[1].Expect.Approval = &a
			if mut != nil {
				mut(s[1])
			}
			return s
		}
	}
	roleDenied := ApprovalExpect{Denied: DeniedRoleInsufficient, TicketStatus: "OPEN", RiskLevel: "HIGH",
		RiskReasons: []string{"PESTICIDE"}}
	tests := []struct {
		name    string
		mutate  func(s []*Sample) []*Sample
		wantErr string // 空表示应通过
	}{
		{name: "role denied ok", mutate: approvalSample("approve_as:farm_manager", roleDenied, nil)},
		{name: "same approver ok", mutate: approvalSample("approve_same_twice",
			ApprovalExpect{Denied: DeniedSameApprover, TicketStatus: "OPEN"}, nil)},
		{name: "reject without reason ok", mutate: approvalSample("reject_no_reason",
			ApprovalExpect{Denied: DeniedRejectReasonInvalid}, nil)},
		{name: "risk reasons ok", mutate: approvalSample("", ApprovalExpect{RiskLevel: "HIGH",
			RiskReasons: []string{"PESTICIDE", "LARGE_AREA", "NEAR_THRESHOLD:wind_speed_max"}}, nil)},
		{name: "unknown area reason ok", mutate: approvalSample("", ApprovalExpect{RiskLevel: "HIGH",
			RiskReasons: []string{"LARGE_AREA:unknown"}}, nil)},
		{name: "exact reasons ok", mutate: approvalSample("", ApprovalExpect{RiskLevel: "HIGH",
			RiskReasons: []string{"LARGE_AREA"}, RiskReasonsExact: true}, nil)},
		{name: "exact no reasons with low ok", mutate: approvalSample("", ApprovalExpect{RiskLevel: "LOW", RiskReasonsExact: true}, nil)},
		{name: "exact alone is an assertion", mutate: approvalSample("", ApprovalExpect{RiskReasonsExact: true}, nil)},
		// 应失败
		{name: "exact no reasons with high", wantErr: "risk_reasons_exact with no reasons requires risk_level LOW",
			mutate: approvalSample("approve", ApprovalExpect{RiskLevel: "HIGH", RiskReasonsExact: true}, nil)},
		{name: "denied without matching script", wantErr: "does not match fixtures.approver",
			mutate: approvalSample("approve", roleDenied, nil)},
		{name: "same approver code with role script", wantErr: "does not match fixtures.approver",
			mutate: approvalSample("approve_as:farm_manager", ApprovalExpect{Denied: DeniedSameApprover}, nil)},
		{name: "unknown denied code", wantErr: "denied \"FORBIDDEN\"",
			mutate: approvalSample("approve", ApprovalExpect{Denied: "FORBIDDEN"}, nil)},
		{name: "empty assertion", wantErr: "asserts nothing", mutate: approvalSample("approve", ApprovalExpect{}, nil)},
		{name: "unknown ticket status", wantErr: "ticket_status \"DONE\"",
			mutate: approvalSample("approve", ApprovalExpect{TicketStatus: "DONE"}, nil)},
		{name: "unknown risk level", wantErr: "risk_level \"MEDIUM\"",
			mutate: approvalSample("approve", ApprovalExpect{RiskLevel: "MEDIUM"}, nil)},
		{name: "unknown risk reason", wantErr: "risk_reasons[0] \"WINDY\"",
			mutate: approvalSample("approve", ApprovalExpect{RiskReasons: []string{"WINDY"}}, nil)},
		{name: "near threshold without rule", wantErr: "risk_reasons[0] \"NEAR_THRESHOLD:\"",
			mutate: approvalSample("approve", ApprovalExpect{RiskReasons: []string{"NEAR_THRESHOLD:"}}, nil)},
		{name: "reasons with low", wantErr: "requires risk_level HIGH",
			mutate: approvalSample("approve", ApprovalExpect{RiskLevel: "LOW", RiskReasons: []string{"PESTICIDE"}}, nil)},
		{name: "without work order", wantErr: "requires outcome=ADVICE",
			mutate: approvalSample("approve_as:farm_manager", roleDenied, func(s *Sample) { s.Expect.WorkOrder = nil })},
	}
	v := NewValidator(DefaultRules(mustTaxonomy(t), repoKB(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := v.Validate(tt.mutate(validSet()))
			if tt.wantErr == "" {
				if !report.OK() {
					t.Fatalf("want ok, got %v", report.Violations)
				}
				return
			}
			for _, viol := range report.Violations {
				if viol.Rule == RuleApproval && viol.Subject() == "s-1" && strings.Contains(viol.Msg, tt.wantErr) {
					return
				}
			}
			t.Fatalf("want %s violation containing %q, got %v", RuleApproval, tt.wantErr, report.Violations)
		})
	}
}

// TestValidApprover_L5：P1 L5 新增的审批脚本取值（与 env.ParseApproverScript 一致）。
func TestValidApprover_L5(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"approve_once", true}, {"approve_same_twice", true}, {"reject_no_reason", true},
		{"approve_as:farm_manager", true}, {"approve_then_wait:3h", true},
		// 应失败
		{"approve_as:platform_admin", false}, {"approve_as:", false}, {"approve_then_wait:0s", false},
		{"approve_then_wait:soon", false},
	}
	for _, tt := range tests {
		if got := validApprover(tt.in); got != tt.want {
			t.Errorf("validApprover(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
