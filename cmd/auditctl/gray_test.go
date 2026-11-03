package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type stubPreflight struct {
	rs  []service.CheckResult
	err error
}

func (s stubPreflight) Run(context.Context, model.Scope) ([]service.CheckResult, error) {
	return s.rs, s.err
}

func TestRunGrayPreflight(t *testing.T) {
	scope := model.Scope{TenantID: 1, FarmID: 2}
	pass := service.CheckResult{Name: service.CheckD5Closed, Pass: true, Detail: "D5 closed"}
	fail := service.CheckResult{Name: service.CheckLegalSignoff, Detail: "signoff legal_signer empty (待签字)"}
	cases := []struct {
		name string
		run  stubPreflight
		code int
		out  []string
	}{
		{"all pass", stubPreflight{rs: []service.CheckResult{pass}}, exitOK, []string{"PASS d5_closed", "RESULT PASS farm (1,2)"}},
		{"one fail", stubPreflight{rs: []service.CheckResult{pass, fail}}, exitBroken,
			[]string{"FAIL legal_signoff: signoff legal_signer empty (待签字)", "RESULT FAIL farm (1,2): 1/2"}},
		{"no checks", stubPreflight{}, exitBroken, []string{"RESULT FAIL"}},
		{"run error", stubPreflight{err: errors.New("db down")}, exitFailed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runGrayPreflight(context.Background(), tc.run, scope, &out)
			if got := exitCode(err, io.Discard); got != tc.code {
				t.Fatalf("exit = %d, want %d (err %v)", got, tc.code, err)
			}
			for _, s := range tc.out {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output missing %q:\n%s", s, out.String())
				}
			}
		})
	}
}

type stubKillSwitch struct {
	rep      service.KillSwitchReport
	err      error
	drillErr error
	drilled  bool
}

func (s *stubKillSwitch) Report(context.Context, time.Time, int64) (service.KillSwitchReport, error) {
	return s.rep, s.err
}

func (s *stubKillSwitch) RecordDrill(context.Context, service.KillSwitchReport) error {
	s.drilled = true
	return s.drillErr
}

// TestRunKillSwitchReport：断言不成立（REAL 读开关晚于 T0、耗时 > 60s）退出码 1；--drill 不论成败都写演练审计。
func TestRunKillSwitchReport(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
	ok := service.KillSwitchReport{T0: t0, FlagChangeID: 9, Failures: []string{}}
	released := ok
	rel := t0.Add(10 * time.Minute)
	released.ReleasedAt, released.ReleaseID = &rel, 12
	late := ok
	late.FlagReadAfter, late.Failures = 1, []string{"1 REAL attempts sent after T0 read the flag at or after T0 (want 0)"}
	slow := ok
	slow.ElapsedMS, slow.Failures = 61000, []string{"max(sent_at)-T0 = 61000ms > 60000ms"}
	cases := []struct {
		name    string
		k       *stubKillSwitch
		drill   bool
		code    int
		drilled bool
		out     string
	}{
		{"pass", &stubKillSwitch{rep: ok}, false, exitOK, false, "RESULT PASS"},
		{"not released", &stubKillSwitch{rep: ok}, false, exitOK, false, "not released yet"},
		{"released bound", &stubKillSwitch{rep: released}, false, exitOK, false, "(FLAG_CHANGE audit 12), counts bounded"},
		{"flag read after T0", &stubKillSwitch{rep: late}, false, exitBroken, false, "flag_read_at >= T0: 1 (want 0)"},
		{"elapsed over 60s", &stubKillSwitch{rep: slow}, false, exitBroken, false, "FAIL max(sent_at)-T0 = 61000ms"},
		{"drill on pass", &stubKillSwitch{rep: ok}, true, exitOK, true, "KILL_SWITCH_DRILL written"},
		{"drill on fail", &stubKillSwitch{rep: slow}, true, exitBroken, true, "KILL_SWITCH_DRILL written"},
		{"drill write error", &stubKillSwitch{rep: ok, drillErr: errors.New("chain")}, true, exitFailed, true, "T0"},
		{"no T0", &stubKillSwitch{err: service.ErrKillSwitchNoT0}, false, exitFailed, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runKillSwitchReport(context.Background(), tc.k, killSwitchOpts{Since: t0, Drill: tc.drill, ExpectHeld: 1}, &out)
			if got := exitCode(err, io.Discard); got != tc.code {
				t.Fatalf("exit = %d, want %d (err %v)", got, tc.code, err)
			}
			if tc.k.drilled != tc.drilled {
				t.Fatalf("drilled = %v, want %v", tc.k.drilled, tc.drilled)
			}
			if !strings.Contains(out.String(), tc.out) {
				t.Fatalf("output missing %q:\n%s", tc.out, out.String())
			}
		})
	}
}

// TestGrayCommands_Usage：参数不合法在读配置、连库之前返回退出码 2。
func TestGrayCommands_Usage(t *testing.T) {
	cases := [][]string{
		{"gray-preflight"},
		{"gray-preflight", "--farm", "0"},
		{"gray-preflight", "--farm", "1", "--tenant", "0"},
		{"kill-switch-report"},
		{"kill-switch-report", "--since", "yesterday"},
		{"kill-switch-report", "--since", "2026-11-03T02:00:00Z", "--expect-held", "-1"},
		{"kill-switch-report", "--since", "2026-11-03T02:00:00Z", "--drill"},
		{"kill-switch-report", "--since", "2026-11-03T02:00:00Z", "--drill", "--expect-held", "0"},
		{"gray-preflight", "--farm", "1", "--redteam-max-age", "0s"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(args, io.Discard, &stderr); got != exitUsage {
				t.Fatalf("exit = %d, want %d (%s)", got, exitUsage, stderr.String())
			}
		})
	}
}
