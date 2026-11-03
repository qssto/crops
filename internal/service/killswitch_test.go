package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

var ksT0 = time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)

// fakeKillStore 是 KillSwitchStore 桩；held 按 farm_id 给 DISPATCH_HELD 计数；until 记录三条计数查询收到的上界。
type fakeKillStore struct {
	changes  []repository.AuditEntry
	maxSent  *time.Time
	sent     int64
	readLate int64
	held     map[uint64]int64
	err      error
	sysTask  string
	until    []*time.Time
}

func (f *fakeKillStore) FlagChangesSince(context.Context, string, time.Time, int) ([]repository.AuditEntry, error) {
	return f.changes, f.err
}

func (f *fakeKillStore) RealSentAfter(_ context.Context, sys repository.SystemScope, _ time.Time,
	until *time.Time) (*time.Time, int64, error) {
	f.sysTask = sys.Task()
	f.until = append(f.until, until)
	return f.maxSent, f.sent, nil
}

func (f *fakeKillStore) RealReadAfter(_ context.Context, _ repository.SystemScope, _ time.Time,
	until *time.Time) (int64, error) {
	f.until = append(f.until, until)
	return f.readLate, nil
}

func (f *fakeKillStore) HeldSince(_ context.Context, _, farmID uint64, _ time.Time, until *time.Time) (int64, error) {
	f.until = append(f.until, until)
	return f.held[farmID], nil
}

type fakeFlagList struct {
	rows []model.FlagSetting
	err  error
}

func (f fakeFlagList) List(context.Context, model.FlagKey) ([]model.FlagSetting, error) {
	return f.rows, f.err
}

// sealAudit 把 rec 封成 row_hash 按 L7-A 规范化一致的平台链审计行（seq = id）；rec.Summary 须是合法 JSON（写错是用例本身的 bug）。
func sealAudit(id uint64, rec model.AuditRecord) repository.AuditEntry {
	c := repository.ChainRow{ID: id, ChainSeq: id, PrevHash: strings.Repeat("0", 64), Rec: rec}
	h, err := repository.AuditRowHash(c.PrevHash, c)
	if err != nil {
		panic(fmt.Sprintf("sealAudit %d: %v", id, err))
	}
	c.RowHash = h
	return repository.AuditEntry{ID: id, CreatedAt: rec.CreatedAt, Result: rec.Result, Summary: rec.Summary, Chain: c}
}

// ksFlagChange 是一条已封链的 enable_robot_dispatch FLAG_CHANGE；oldValue 为 "null" 表示首次设置。
func ksFlagChange(id uint64, at time.Time, scope model.FlagScopeType, oldValue, newValue, result string) repository.AuditEntry {
	s := `{"flag_key":"enable_robot_dispatch","scope_type":"` + string(scope) + `","old":` + oldValue + `,"new":` + newValue + `}`
	return sealAudit(id, model.AuditRecord{Actor: model.Actor{Type: model.ActorUser, ID: 1},
		Action: string(model.AuditFlagChange), Target: killSwitchTarget, Summary: json.RawMessage(s), Result: result,
		CreatedAt: at})
}

const (
	ksOn  = `{"force_off":false}`
	ksOff = `{"force_off":true}`
)

// newKillFixture：FARM 行、失败的 GLOBAL 关、关→关重复写（不算 T0）、T0（id 12）、T0+1m 解除（id 13）。
func newKillFixture() (*fakeKillStore, fakeFlagList) {
	maxSent := ksT0.Add(20 * time.Second)
	store := &fakeKillStore{
		changes: []repository.AuditEntry{
			ksFlagChange(9, ksT0.Add(-2*time.Minute), model.FlagScopeGlobal, ksOff, ksOff, model.AuditResultOK),
			ksFlagChange(10, ksT0.Add(-time.Minute), model.FlagScopeFarm, "null", `{"on":true}`, model.AuditResultOK),
			ksFlagChange(11, ksT0.Add(-time.Second), model.FlagScopeGlobal, ksOn, ksOff, model.AuditResultFailed),
			ksFlagChange(12, ksT0, model.FlagScopeGlobal, ksOn, ksOff, model.AuditResultOK),
			ksFlagChange(13, ksT0.Add(time.Minute), model.FlagScopeGlobal, ksOff, ksOn, model.AuditResultOK),
		},
		maxSent: &maxSent, sent: 2, held: map[uint64]int64{1: 3, 2: 1},
	}
	flags := fakeFlagList{rows: []model.FlagSetting{
		{ScopeType: model.FlagScopeGlobal, Value: json.RawMessage(ksOff)},
		{TenantID: 1, FarmID: 1, ScopeType: model.FlagScopeFarm, Value: json.RawMessage(`{"on":true}`)},
		{TenantID: 1, FarmID: 2, ScopeType: model.FlagScopeFarm, Value: json.RawMessage(`{"on":true}`)},
	}}
	return store, flags
}

func newKillReporter(t *testing.T, store KillSwitchStore, flags FlagRowLister, audit AuditAppender) *KillSwitchReporter {
	t.Helper()
	k, err := NewKillSwitchReporter(KillSwitchDeps{Store: store, Flags: flags, Audit: audit, Clock: fixedClock{ksT0.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestKillSwitchReport：T0 取第一条成功的 GLOBAL 开→关（关→关重复写不算），其后第一条 force_off:false 为计数上界；
// 任一断言不成立即 Passed=false（auditctl 退出码 1）。
func TestKillSwitchReport(t *testing.T) {
	release := ksT0.Add(time.Minute)
	cases := []struct {
		name       string
		mutate     func(s *fakeKillStore)
		expectHeld int64
		until      *time.Time
		fail       string
	}{
		{"pass", func(*fakeKillStore) {}, 4, &release, ""},
		{"not released yet", func(s *fakeKillStore) { s.changes = s.changes[:4] }, 4, nil, ""},
		{"re-shutdown after release keeps first T0", func(s *fakeKillStore) {
			s.changes = append(s.changes, ksFlagChange(14, ksT0.Add(2*time.Minute), model.FlagScopeGlobal, ksOn, ksOff,
				model.AuditResultOK))
		}, 4, &release, ""},
		{"no REAL after T0", func(s *fakeKillStore) { s.maxSent, s.sent = nil, 0 }, 0, &release, ""},
		{"exactly 60s", func(s *fakeKillStore) { at := ksT0.Add(KillSwitchMaxElapsed); s.maxSent = &at }, 0, &release, ""},
		{"REAL read flag after T0", func(s *fakeKillStore) { s.readLate = 1 }, 0, &release, "read the flag at or after T0"},
		{"elapsed over 60s", func(s *fakeKillStore) {
			at := ksT0.Add(KillSwitchMaxElapsed + time.Millisecond)
			s.maxSent = &at
		}, 0, &release, "60001ms > 60000ms"},
		{"held below preset", func(*fakeKillStore) {}, 5, &release, "DISPATCH_HELD total 4 < unsent preset orders 5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, flags := newKillFixture()
			tc.mutate(store)
			rep, err := newKillReporter(t, store, flags, nil).Report(context.Background(), ksT0.Add(-time.Hour), tc.expectHeld)
			if err != nil {
				t.Fatal(err)
			}
			if rep.T0 != ksT0 || rep.FlagChangeID != 12 {
				t.Fatalf("T0 = %s / audit %d, want %s / 12", rep.T0, rep.FlagChangeID, ksT0)
			}
			if store.sysTask != "kill_switch_report" {
				t.Fatalf("system scope task = %q", store.sysTask)
			}
			if rep.HeldTotal != 4 || len(rep.Farms) != 2 {
				t.Fatalf("held = %d over %d farms, want 4 over 2 (GLOBAL row excluded)", rep.HeldTotal, len(rep.Farms))
			}
			assertUntil(t, rep, store.until, tc.until)
			if tc.fail == "" {
				if !rep.Passed() {
					t.Fatalf("failures = %v", rep.Failures)
				}
				return
			}
			if rep.Passed() || !strings.Contains(strings.Join(rep.Failures, "; "), tc.fail) {
				t.Fatalf("failures = %v, want %q", rep.Failures, tc.fail)
			}
		})
	}
}

// assertUntil：报告的解除时刻与每条计数查询（REAL 发出、读开关、2 个农场 HELD）收到的上界都等于 want。
func assertUntil(t *testing.T, rep KillSwitchReport, got []*time.Time, want *time.Time) {
	t.Helper()
	same := func(a *time.Time) bool { return (a == nil) == (want == nil) && (a == nil || a.Equal(*want)) }
	if !same(rep.ReleasedAt) || (want != nil && rep.ReleaseID != 13) {
		t.Fatalf("released = %v / audit %d, want %v / 13", rep.ReleasedAt, rep.ReleaseID, want)
	}
	if len(got) != 4 {
		t.Fatalf("store got %d bounded queries, want 4", len(got))
	}
	for i, u := range got {
		if !same(u) {
			t.Fatalf("query %d until = %v, want %v", i, u, want)
		}
	}
}

func TestKillSwitchReport_Errors(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name   string
		mutate func(s *fakeKillStore, f *fakeFlagList)
		expect int64
		want   error
	}{
		{"negative expect held", func(*fakeKillStore, *fakeFlagList) {}, -1, ErrBadInput},
		{"only off→off and failed", func(s *fakeKillStore, _ *fakeFlagList) { s.changes = s.changes[:3] }, 0, ErrKillSwitchNoT0},
		{"release before shutdown only", func(s *fakeKillStore, _ *fakeFlagList) {
			s.changes = []repository.AuditEntry{s.changes[4]}
		}, 0, ErrKillSwitchNoT0},
		{"no audits", func(s *fakeKillStore, _ *fakeFlagList) { s.changes = nil }, 0, ErrKillSwitchNoT0},
		{"T0 row hash tampered", func(s *fakeKillStore, _ *fakeFlagList) {
			s.changes[3].Chain.Rec.CreatedAt = ksT0.Add(-time.Hour)
		}, 0, repository.ErrAuditRowHash},
		{"release row hash tampered", func(s *fakeKillStore, _ *fakeFlagList) {
			s.changes[4].Chain.RowHash = strings.Repeat("f", 64)
		}, 0, repository.ErrAuditRowHash},
		{"scan limit without release", func(s *fakeKillStore, _ *fakeFlagList) {
			s.changes = s.changes[:4]
			for i := len(s.changes); i < killSwitchScanLimit; i++ {
				s.changes = append(s.changes, ksFlagChange(uint64(100+i), ksT0.Add(time.Duration(i)*time.Second),
					model.FlagScopeFarm, "null", `{"on":true}`, model.AuditResultOK))
			}
		}, 0, ErrKillSwitchScanLimit},
		{"store error", func(s *fakeKillStore, _ *fakeFlagList) { s.err = boom }, 0, boom},
		{"flag list error", func(_ *fakeKillStore, f *fakeFlagList) { f.err = boom }, 0, boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, flags := newKillFixture()
			tc.mutate(store, &flags)
			_, err := newKillReporter(t, store, flags, nil).Report(context.Background(), ksT0.Add(-time.Hour), tc.expect)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := NewKillSwitchReporter(KillSwitchDeps{}); err == nil {
		t.Fatal("want deps error")
	}
}

// TestKillSwitchRecordDrill：演练审计落平台链 (0,0)，断言不成立也写（FAILED）；summary 带 t0 / flag_change_audit_id / released_at，
// 封链后可被 gray-preflight 读回并回查 T0 的 FLAG_CHANGE。
func TestKillSwitchRecordDrill(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readLate int64
		result   string
		pass     bool
	}{
		{"passed", 0, model.AuditResultOK, true},
		{"assertion failed", 1, model.AuditResultFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, flags := newKillFixture()
			store.readLate = tc.readLate
			audit := &memAppender{}
			k := newKillReporter(t, store, flags, audit)
			rep, err := k.Report(context.Background(), ksT0.Add(-time.Hour), 4)
			if err != nil {
				t.Fatal(err)
			}
			if err := k.RecordDrill(context.Background(), rep); err != nil {
				t.Fatal(err)
			}
			if len(audit.recs) != 1 {
				t.Fatalf("got %d audits, want 1", len(audit.recs))
			}
			r := audit.recs[0]
			if r.TenantID != 0 || r.FarmID != 0 || r.Action != string(model.AuditKillSwitchDrill) ||
				r.Target != killSwitchTarget || r.Result != tc.result || r.Actor.Type != model.ActorSystem {
				t.Fatalf("audit = %+v", r)
			}
			var s killSwitchDrill
			if err := json.Unmarshal(r.Summary, &s); err != nil {
				t.Fatal(err)
			}
			if !s.T0.Equal(ksT0) || s.FlagChangeID != 12 || s.ReleasedAt == nil || !s.ReleasedAt.Equal(ksT0.Add(time.Minute)) {
				t.Fatalf("drill summary = %+v", s)
			}
			g := &GrayPreflight{d: GrayPreflightDeps{Clock: fixedClock{ksT0.Add(time.Hour)}, Drills: &fakeDrills{found: true,
				entry: sealAudit(50, r), fc: store.changes[3], fcFound: true}}}
			if got := g.checkDrill(context.Background()); got.Pass != tc.pass {
				t.Fatalf("gray preflight drill check = %+v, want pass %v", got, tc.pass)
			}
		})
	}
	store, flags := newKillFixture()
	if err := newKillReporter(t, store, flags, nil).RecordDrill(context.Background(), KillSwitchReport{ExpectHeld: 1}); err == nil {
		t.Fatal("want error without audit appender")
	}
	empty := &memAppender{}
	err := newKillReporter(t, store, flags, empty).RecordDrill(context.Background(), KillSwitchReport{T0: ksT0, FlagChangeID: 12})
	if !errors.Is(err, ErrBadInput) || len(empty.recs) != 0 {
		t.Fatalf("drill without preset orders: err = %v, audits %d", err, len(empty.recs))
	}
	failing := &memAppender{fail: errors.New("boom")}
	if err := newKillReporter(t, store, flags, failing).RecordDrill(context.Background(), KillSwitchReport{ExpectHeld: 1}); err == nil {
		t.Fatal("want audit write error")
	}
}
