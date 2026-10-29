package env

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fakeDecider 模拟 DecisionService 的票据语义：confirm 票第一人通过后 AwaitingConfirm；过期返回 ErrTicketExpired。
type fakeDecider struct {
	clk     *FrozenClock
	confirm bool
	calls   []string
	first   uint64
}

func (f *fakeDecider) Decide(_ context.Context, scope model.Scope, in service.DecideInput) (service.DecideResult, error) {
	f.calls = append(f.calls, in.Decision+"@"+in.RequestID)
	if !time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC).After(f.clk.Now()) {
		return service.DecideResult{}, service.ErrTicketExpired
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
		wantSweep bool
		wantOK    bool
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := NewFrozenClock(start)
			dec, sw := &fakeDecider{clk: clk, confirm: tt.confirm}, &fakeSweeper{clk: clk}
			script, err := ParseApproverScript(tt.script)
			if err != nil {
				t.Fatal(err)
			}
			a, err := NewScriptedApprover(script, ApproverDeps{Clock: clk, Decider: dec, Sweeper: sw,
				First:  Identity{UserID: 1002, Roles: []model.Role{model.RoleAgronomist}},
				Second: Identity{UserID: 1004, Roles: []model.Role{model.RoleAgronomist}}})
			if err != nil {
				t.Fatal(err)
			}
			ok, err := a.Act(context.Background(), farm, tk)
			if err != nil || ok != tt.wantOK {
				t.Fatalf("approved = %v, %v, want %v", ok, err, tt.wantOK)
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
