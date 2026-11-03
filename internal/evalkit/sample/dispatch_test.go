package sample

import (
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

// TestCheckDispatch（P1-L4-A）：fixtures.dispatch 须配审批脚本、不与 stop_at_approval 同用；expect.dispatch 须配 fixtures.dispatch。
func TestCheckDispatch(t *testing.T) {
	dispatchSample := func(mut func(s *Sample)) func(s []*Sample) []*Sample {
		return func(s []*Sample) []*Sample {
			s[1].Fixtures.Approver = "approve"
			s[1].Fixtures.Dispatch = &DispatchFixture{}
			s[1].Expect.Dispatch = &DispatchExpect{RecheckReturned: true, DeviceDispatches: intp(0)}
			s[1].Expect.WorkOrder = &WorkOrderExpect{FinalStatus: "PENDING_APPROVAL"}
			if mut != nil {
				mut(s[1])
			}
			return s
		}
	}
	tests := []struct {
		name    string
		mutate  func(s []*Sample) []*Sample
		wantErr string
	}{
		{name: "returned ok", mutate: dispatchSample(nil)},
		{name: "reapprove and redeliver ok", mutate: dispatchSample(func(s *Sample) {
			s.Fixtures.Dispatch = &DispatchFixture{Reapprove: "approve_after:10m", Redeliver: 2}
			s.Expect.WorkOrder.FinalStatus = "DONE"
		})},
		{name: "no approver", wantErr: "requires fixtures.approver",
			mutate: dispatchSample(func(s *Sample) { s.Fixtures.Approver = "" })},
		{name: "stop at approval", wantErr: "conflicts with stop_at_approval",
			mutate: dispatchSample(func(s *Sample) { s.Expect.Trajectory.StopAtApproval = true })},
		{name: "bad reapprove", wantErr: "reapprove",
			mutate: dispatchSample(func(s *Sample) { s.Fixtures.Dispatch.Reapprove = "maybe" })},
		{name: "redeliver too many", wantErr: "redeliver 4",
			mutate: dispatchSample(func(s *Sample) { s.Fixtures.Dispatch.Redeliver = 4 })},
		{name: "negative redeliver", wantErr: "redeliver -1",
			mutate: dispatchSample(func(s *Sample) { s.Fixtures.Dispatch.Redeliver = -1 })},
		{name: "expect without fixture", wantErr: "requires fixtures.dispatch",
			mutate: dispatchSample(func(s *Sample) { s.Fixtures.Dispatch = nil })},
		{name: "negative device dispatches", wantErr: "device_dispatches -1",
			mutate: dispatchSample(func(s *Sample) { s.Expect.Dispatch.DeviceDispatches = intp(-1) })},
		{name: "returned but final DONE", wantErr: "final_status=PENDING_APPROVAL",
			mutate: dispatchSample(func(s *Sample) { s.Expect.WorkOrder.FinalStatus = "DONE" })},
		{name: "recheck failed rules ok", mutate: dispatchSample(func(s *Sample) {
			s.Expect.Dispatch.RecheckFailedRules = []string{"device_battery"}
		})},
		{name: "failed rules without return", wantErr: "requires recheck_returned=true",
			mutate: dispatchSample(func(s *Sample) {
				s.Expect.Dispatch.RecheckReturned = false
				s.Expect.Dispatch.RecheckFailedRules = []string{"device_battery"}
			})},
		{name: "empty failed rule id", wantErr: "recheck_failed_rules[0]",
			mutate: dispatchSample(func(s *Sample) { s.Expect.Dispatch.RecheckFailedRules = []string{""} })},
		{name: "too long failed rule id", wantErr: "recheck_failed_rules[0]",
			mutate: dispatchSample(func(s *Sample) {
				s.Expect.Dispatch.RecheckFailedRules = []string{strings.Repeat("x", 49)}
			})},
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
				if viol.Rule == RuleDispatch && viol.Subject() == "s-1" && strings.Contains(viol.Msg, tt.wantErr) {
					return
				}
			}
			t.Fatalf("want %s violation containing %q, got %v", RuleDispatch, tt.wantErr, report.Violations)
		})
	}
}
