package env

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fakeDecider 模拟 DecisionService 的票据语义：confirm 票第一人通过后 AwaitingConfirm；过期返回 ErrTicketExpired；
// 审批人不含 agronomist 返回 ErrRoleInsufficient（用药类票）；驳回缺原因返回 ErrRejectReasonInvalid；fail 非空时一律返回 fail。
type fakeDecider struct {
	clk     *FrozenClock
	confirm bool
	calls   []string
	first   uint64
	fail    error
}

func (f *fakeDecider) Decide(_ context.Context, scope model.Scope, in service.DecideInput) (service.DecideResult, error) {
	f.calls = append(f.calls, in.Decision+"@"+in.RequestID)
	switch {
	case f.fail != nil:
		return service.DecideResult{}, f.fail
	case !time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC).After(f.clk.Now()):
		return service.DecideResult{}, service.ErrTicketExpired
	case !slices.Contains(scope.Roles, model.RoleAgronomist):
		return service.DecideResult{}, &service.RoleDeniedError{Op: service.OpApprovalDecidePesticide}
	case in.Decision == service.DecisionReject && in.ReasonCode == "":
		return service.DecideResult{}, service.ErrRejectReasonInvalid
	}
	if in.Decision == service.DecisionApprove && f.confirm && f.first == 0 {
		f.first = scope.UserID
		return service.DecideResult{TicketID: in.TicketID, Status: model.TicketOpen, AwaitingConfirm: true}, nil
	}
	if f.first == scope.UserID {
		return service.DecideResult{}, service.ErrSameApprover
	}
	if in.Decision == service.DecisionReject {
		return service.DecideResult{TicketID: in.TicketID, Status: model.TicketRejected}, nil
	}
	return service.DecideResult{TicketID: in.TicketID, Status: model.TicketApproved}, nil
}

type fakeSweeper struct {
	clk *FrozenClock
	at  []time.Time
}

func (f *fakeSweeper) RunOnce(context.Context) error { f.at = append(f.at, f.clk.Now()); return nil }

func TestParseApproverScript(t *testing.T) {
	tests := []struct {
		in      string
		want    ApproverScript
		wantErr bool
	}{
		{in: "approve", want: ApproverScript{Action: ApproverApprove}},
		{in: "reject", want: ApproverScript{Action: ApproverReject}},
		{in: "timeout", want: ApproverScript{Action: ApproverTimeout}},
		{in: "approve_after: 90m", want: ApproverScript{Action: ApproverApprove, After: 90 * time.Minute}},
		{in: "approve_after:-1m", wantErr: true},
		{in: "approve_after:soon", wantErr: true},
		{in: "approve_once", want: ApproverScript{Action: ApproverApproveOnce}},
		{in: "approve_same_twice", want: ApproverScript{Action: ApproverApproveSameTwice}},
		{in: "reject_no_reason", want: ApproverScript{Action: ApproverRejectNoReason}},
		{in: "approve_as:farm_manager", want: ApproverScript{Action: ApproverApproveAs, As: model.RoleFarmManager}},
		{in: "approve_then_wait:3h", want: ApproverScript{Action: ApproverApproveThenWait, Wait: 3 * time.Hour}},
		// 应失败：approve_as 只认农场审批相关角色；approve_then_wait 时长须为正
		{in: "approve_as:platform_admin", wantErr: true},
		{in: "approve_as:", wantErr: true},
		{in: "approve_then_wait:0s", wantErr: true},
		{in: "maybe", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseApproverScript(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("%q = %+v, %v", tt.in, got, err)
		}
		if err != nil && !errors.Is(err, errBadApprover) {
			t.Errorf("%q err = %v", tt.in, err)
		}
	}
}

func TestScriptedApprover_Act(t *testing.T) {
	start := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	farm := model.Scope{TenantID: 1, FarmID: 1}
	tk := Ticket{ID: 5, Digest: "d", ExpiresAt: expires}
	tests := []struct {
		name      string
		script    string
		confirm   bool
		wantCalls []string
		fail      error
		wantSweep bool
		wantOK    bool
		wantErr   bool
	}{
		{name: "approve low", script: "approve", wantOK: true, wantCalls: []string{"APPROVE@script-5-1002-APPROVE"}},
		{name: "approve high needs second approver", script: "approve", confirm: true, wantOK: true,
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE", "APPROVE@script-5-1004-APPROVE"}},
		{name: "reject", script: "reject", wantCalls: []string{"REJECT@script-5-1002-REJECT"}},
		{name: "timeout sweeps after expiry", script: "timeout", wantSweep: true},
		{name: "approve_after within window", script: "approve_after:30m", wantOK: true,
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE"}},
		{name: "approve_after past expiry ends expired", script: "approve_after:3h",
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE"}},
		// P1 L5 反例脚本：闸门拒绝不算错误，票据不通过
		{name: "approve_once high stays open", script: "approve_once", confirm: true,
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE"}},
		{name: "approve_same_twice denied", script: "approve_same_twice", confirm: true,
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE", "APPROVE@script-5-1002-APPROVE-again"}},
		{name: "reject_no_reason denied", script: "reject_no_reason", wantCalls: []string{"REJECT@script-5-1002-REJECT"}},
		{name: "approve_as farm_manager denied", script: "approve_as:farm_manager",
			wantCalls: []string{"APPROVE@script-5-1001-APPROVE"}},
		{name: "approve_as agronomist approves", script: "approve_as:agronomist", wantOK: true,
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE"}},
		{name: "approve_then_wait advances past window", script: "approve_then_wait:3h", confirm: true, wantOK: true,
			wantCalls: []string{"APPROVE@script-5-1002-APPROVE", "APPROVE@script-5-1004-APPROVE"}},
		// 应失败：非闸门拒绝的错误照常返回
		{name: "approve_as other error surfaces", script: "approve_as:farm_manager", fail: errors.New("db down"),
			wantErr: true, wantCalls: []string{"APPROVE@script-5-1001-APPROVE"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := NewFrozenClock(start)
			dec, sw := &fakeDecider{clk: clk, confirm: tt.confirm, fail: tt.fail}, &fakeSweeper{clk: clk}
			settled := 0
			script, err := ParseApproverScript(tt.script)
			if err != nil {
				t.Fatal(err)
			}
			a, err := NewScriptedApprover(script, ApproverDeps{Clock: clk, Decider: dec, Sweeper: sw,
				First:  Identity{UserID: 1002, Roles: []model.Role{model.RoleAgronomist}},
				Second: Identity{UserID: 1004, Roles: []model.Role{model.RoleAgronomist}},
				ByRole: map[model.Role]Identity{
					model.RoleFarmManager: {UserID: 1001, Roles: []model.Role{model.RoleFarmManager}},
					model.RoleAgronomist:  {UserID: 1002, Roles: []model.Role{model.RoleAgronomist}}},
				Settle: func() { settled++ }})
			if err != nil {
				t.Fatal(err)
			}
			ok, err := a.Act(context.Background(), farm, tk)
			if (err != nil) != tt.wantErr || ok != tt.wantOK {
				t.Fatalf("approved = %v, %v, want %v (err %v)", ok, err, tt.wantOK, tt.wantErr)
			}
			if wantSettle := tt.script == "approve_then_wait:3h"; wantSettle != (settled == 1) ||
				(wantSettle && !clk.Now().Equal(start.Add(3*time.Hour))) {
				t.Errorf("settled = %d, clock = %v", settled, clk.Now())
			}
			if !reflect.DeepEqual(dec.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", dec.calls, tt.wantCalls)
			}
			if tt.wantSweep != (len(sw.at) == 1) || (tt.wantSweep && !sw.at[0].After(expires)) {
				t.Errorf("sweeps = %v", sw.at)
			}
		})
	}
}

func TestNewScriptedApprover_SameUserRejected(t *testing.T) {
	clk := NewFrozenClock(time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC))
	same := Identity{UserID: 1002}
	if _, err := NewScriptedApprover(ApproverScript{Action: ApproverApprove}, ApproverDeps{Clock: clk,
		Decider: &fakeDecider{clk: clk}, Sweeper: &fakeSweeper{clk: clk}, First: same, Second: same}); err == nil {
		t.Fatal("want error for same approver")
	}
}

// 应失败：approve_as 声明的角色没有种子用户时构造失败（不静默换人审批）。
func TestNewScriptedApprover_ApproveAsNeedsUser(t *testing.T) {
	clk := NewFrozenClock(time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC))
	_, err := NewScriptedApprover(ApproverScript{Action: ApproverApproveAs, As: model.RoleOperator}, ApproverDeps{Clock: clk,
		Decider: &fakeDecider{clk: clk}, Sweeper: &fakeSweeper{clk: clk}, First: Identity{UserID: 1002}, Second: Identity{UserID: 1004}})
	if err == nil {
		t.Fatal("want error for approve_as without seed user")
	}
}
