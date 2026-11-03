package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

var grayNow = time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)

type fakeDrills struct {
	entry   repository.AuditEntry
	found   bool
	err     error
	fc      repository.AuditEntry
	fcFound bool
	fcErr   error
	fcID    uint64
}

func (f *fakeDrills) LatestDrill(context.Context, time.Time) (repository.AuditEntry, bool, error) {
	return f.entry, f.found, f.err
}

func (f *fakeDrills) FlagChangeByID(_ context.Context, id uint64) (repository.AuditEntry, bool, error) {
	f.fcID = id
	if f.fcFound && f.fc.ID != id {
		return repository.AuditEntry{}, false, f.fcErr
	}
	return f.fc, f.fcFound, f.fcErr
}

type fakeRedteam struct {
	exp       repository.EvalExperiment
	found     bool
	err       error
	trials    []repository.EvalTrial
	trialsErr error
}

func (f *fakeRedteam) LatestExperimentWithSuite(context.Context, string) (repository.EvalExperiment, bool, error) {
	return f.exp, f.found, f.err
}

func (f *fakeRedteam) SuiteTrials(context.Context, uint64, string) ([]repository.EvalTrial, error) {
	return f.trials, f.trialsErr
}

type fakeFarmRoles struct {
	has bool
	err error
}

func (f fakeFarmRoles) FarmHasRole(context.Context, uint64, uint64, model.Role) (bool, error) {
	return f.has, f.err
}

type fakeFlagRows struct {
	rows []model.FlagRow
	err  error
}

func (f fakeFlagRows) Rows(context.Context, model.FlagKey, uint64, uint64) ([]model.FlagRow, error) {
	return f.rows, f.err
}

// grayFiles 返回三份文件都满足要求的仓库替身。
func grayFiles() fstest.MapFS {
	var rules strings.Builder
	rules.WriteString("groups:\n  - name: g\n    rules:\n")
	for _, a := range model.AlertNames {
		fmt.Fprintf(&rules, "      - alert: %s\n        expr: up == 0\n", a)
	}
	return fstest.MapFS{
		ExternalDecisionsPath: {Data: []byte("version: 1\ndecisions:\n  - id: D5\n    title: t\n    status: CLOSED\n" +
			"    closed_at: \"2026-10-01\"\n    approved_by: owner\n    record_ref: rec-1\n")},
		RegulationMapPath: {Data: []byte("version: 1\nsignoff:\n  legal_signer: legal\n  legal_signed_at: \"2026-10-02\"\n" +
			"  record_ref: rec-2\nentries: []\n")},
		AlertRulesPath:                   {Data: []byte(rules.String())},
		RedteamSuiteDir + "/x/rt-0.yaml": {Data: []byte("id: rt-0\nsuite: redteam\nquestion: q\n")},
		RedteamSuiteDir + "/x/rt-1.yml":  {Data: []byte("id: rt-1\nsuite: redteam\n")},
		RedteamSuiteDir + "/x/README.md": {Data: []byte("not a sample")},
	}
}

// grayT0 是演练 summary 引用的 T0（= FLAG_CHANGE 审计时间）。
var grayT0 = grayNow.Add(-2 * time.Hour)

// drillSummaryJSON 拼一条 KILL_SWITCH_DRILL summary（t0 指向 FLAG_CHANGE 21）。
func drillSummaryJSON(t0 time.Time, elapsed, held, expect int64, passed bool) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"t0":%q,"flag_change_audit_id":21,"elapsed_ms":%d,"held_total":%d,`+
		`"expect_held":%d,"passed":%t}`, t0.Format(time.RFC3339Nano), elapsed, held, expect, passed))
}

// drillAudit 是已封链的 KILL_SWITCH_DRILL 审计。
func drillAudit(summary json.RawMessage, result string) repository.AuditEntry {
	return sealAudit(7, model.AuditRecord{Actor: model.Actor{Type: model.ActorSystem},
		Action: string(model.AuditKillSwitchDrill), Target: killSwitchTarget, Summary: summary, Result: result,
		CreatedAt: grayNow.Add(-time.Hour)})
}

func drillEntry(elapsed int64, passed bool, result string) repository.AuditEntry {
	return drillAudit(drillSummaryJSON(grayT0, elapsed, 3, 3, passed), result)
}

// redteamTrials 返回 samples 个样本、每个 k 个 trial、红线维度 safety 全过的 trial。
func redteamTrials(samples, k int) []repository.EvalTrial {
	var out []repository.EvalTrial
	for s := range samples {
		for i := range k {
			out = append(out, repository.EvalTrial{SampleID: fmt.Sprintf("rt-%d", s), TrialIndex: i, Status: "PASS",
				Scores: json.RawMessage(`{"safety":{"pass":true,"red_line":true},"style":{"pass":false}}`)})
		}
	}
	return out
}

type grayFixture struct {
	files   fstest.MapFS
	drills  *fakeDrills
	redteam *fakeRedteam
	roles   fakeFarmRoles
	flags   fakeFlagRows
}

func newGrayFixture() *grayFixture {
	finished := grayNow.Add(-2 * time.Hour)
	return &grayFixture{
		files: grayFiles(),
		drills: &fakeDrills{entry: drillEntry(1500, true, model.AuditResultOK), found: true,
			fc: ksFlagChange(21, grayT0, model.FlagScopeGlobal, "null", ksOff, model.AuditResultOK), fcFound: true},
		redteam: &fakeRedteam{exp: repository.EvalExperiment{ID: 3, TriggerKind: "nightly", Status: experimentDone,
			GitSHA: "abc1234", FinishedAt: &finished}, found: true, trials: redteamTrials(2, redteamK)},
		roles: fakeFarmRoles{has: true},
		flags: fakeFlagRows{rows: []model.FlagRow{{ScopeType: model.FlagScopeFarm, Value: json.RawMessage(`{"on":true}`)}}},
	}
}

func (f *grayFixture) run(t *testing.T) map[string]CheckResult {
	t.Helper()
	g, err := NewGrayPreflight(GrayPreflightDeps{Files: f.files, Drills: f.drills, Redteam: f.redteam, FarmRoles: f.roles,
		Flags: f.flags, Clock: fixedClock{grayNow}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := g.Run(context.Background(), model.Scope{TenantID: 1, FarmID: 1})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]CheckResult{}
	for _, r := range rs {
		out[r.Name] = r
	}
	return out
}

// TestGrayPreflight_AllPass：全过；redteam 输出带 git_sha 与覆盖数，演练输出带 T0 与 FLAG_CHANGE id，告警注明仅文件。
func TestGrayPreflight_AllPass(t *testing.T) {
	f := newGrayFixture()
	rs := f.run(t)
	if len(rs) != 7 {
		t.Fatalf("got %d checks, want 7", len(rs))
	}
	for _, r := range rs {
		if !r.Pass {
			t.Errorf("%s FAIL: %s", r.Name, r.Detail)
		}
	}
	for name, want := range map[string]string{
		CheckRedteamPassK:   "git_sha abc1234",
		CheckKillSwitchTest: "(FLAG_CHANGE 21)",
		CheckAlertRules:     alertRulesFileOnly,
	} {
		if !strings.Contains(rs[name].Detail, want) {
			t.Errorf("%s detail %q missing %q", name, rs[name].Detail, want)
		}
	}
	if !strings.Contains(rs[CheckRedteamPassK].Detail, "2/2 samples") || f.drills.fcID != 21 {
		t.Errorf("redteam %q, FLAG_CHANGE looked up %d", rs[CheckRedteamPassK].Detail, f.drills.fcID)
	}
}

// setD5ClosedAt 把夹具 D5 的 closed_at 换成 v（其余字段仍齐全）。
func setD5ClosedAt(f *grayFixture, v string) {
	f.files[ExternalDecisionsPath] = &fstest.MapFile{Data: []byte("version: 1\ndecisions:\n  - id: D5\n    title: t\n" +
		"    status: CLOSED\n    closed_at: \"" + v + "\"\n    approved_by: owner\n    record_ref: rec-1\n")}
}

// TestGrayPreflight_AlertRecordRules：record 规则与告警规则并存时不算空告警。
func TestGrayPreflight_AlertRecordRules(t *testing.T) {
	f := newGrayFixture()
	f.files[AlertRulesPath].Data = append(f.files[AlertRulesPath].Data,
		[]byte("  - name: rec\n    rules:\n      - record: job:up:sum\n        expr: sum(up)\n")...)
	if r := f.run(t)[CheckAlertRules]; !r.Pass {
		t.Fatalf("alert rules with record rule = %+v", r)
	}
}

// TestGrayPreflight_RedteamMaxAge：时效可配；负值拒绝，0 取默认 36h。夹具实验 finished_at 为 2h 前。
func TestGrayPreflight_RedteamMaxAge(t *testing.T) {
	f := newGrayFixture()
	deps := GrayPreflightDeps{Files: f.files, Drills: f.drills, Redteam: f.redteam, FarmRoles: f.roles, Flags: f.flags,
		Clock: fixedClock{grayNow}, RedteamMaxAge: -time.Hour}
	if _, err := NewGrayPreflight(deps); !errors.Is(err, ErrBadInput) {
		t.Fatalf("negative max age err = %v", err)
	}
	for age, pass := range map[time.Duration]bool{0: true, time.Hour: false, 3 * time.Hour: true} {
		deps.RedteamMaxAge = age
		g, err := NewGrayPreflight(deps)
		if err != nil {
			t.Fatal(err)
		}
		if r := g.checkRedteam(context.Background()); r.Pass != pass {
			t.Errorf("max age %s: %+v, want pass %v", age, r, pass)
		}
	}
}

// TestLoadRedteamSamples_Repo：仓库现有 redteam 样本可加载、无隔离样本（至少含已有的 6 个 cross_farm 样本）。
func TestLoadRedteamSamples_Repo(t *testing.T) {
	ids, err := loadRedteamSamples(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
	}
	for _, id := range []string{"xf-irr-0001", "xf-pest-0001", "xf-pest-0002", "xf-refuse-0001", "xf-refuse-0002",
		"xf-refuse-0003"} {
		if !have[id] {
			t.Errorf("repo redteam suite missing %s (got %v)", id, ids)
		}
	}
}

// TestGrayPreflight_EachFails：每一项各一个失败用例，其余项仍 PASS（取数失败按该项 FAIL，不中断）。
func TestGrayPreflight_EachFails(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name   string
		check  string
		mutate func(f *grayFixture)
		detail string
	}{
		{"d5 open", CheckD5Closed, func(f *grayFixture) {
			f.files[ExternalDecisionsPath] = &fstest.MapFile{Data: []byte("version: 1\ndecisions:\n  - id: D5\n    status: OPEN\n")}
		}, "status OPEN"},
		{"d5 closed but unsigned", CheckD5Closed, func(f *grayFixture) {
			f.files[ExternalDecisionsPath] = &fstest.MapFile{Data: []byte(
				"version: 1\ndecisions:\n  - id: D5\n    status: CLOSED\n    closed_at: \"2026-10-01\"\n    approved_by: \" \"\n")}
		}, "approved_by, record_ref empty"},
		{"d5 missing", CheckD5Closed, func(f *grayFixture) {
			f.files[ExternalDecisionsPath] = &fstest.MapFile{Data: []byte("version: 1\ndecisions: []\n")}
		}, "D5 not found"},
		{"d5 typo field", CheckD5Closed, func(f *grayFixture) {
			f.files[ExternalDecisionsPath] = &fstest.MapFile{Data: []byte("version: 1\ndecisions:\n  - id: D5\n    statuss: CLOSED\n")}
		}, "parse"},
		{"d5 file absent", CheckD5Closed, func(f *grayFixture) { delete(f.files, ExternalDecisionsPath) }, "read"},
		{"d5 duplicate id", CheckD5Closed, func(f *grayFixture) {
			f.files[ExternalDecisionsPath] = &fstest.MapFile{Data: append(f.files[ExternalDecisionsPath].Data,
				[]byte("  - id: D5\n    status: OPEN\n")...)}
		}, "duplicate decision id"},
		{"d5 closed_at not a date", CheckD5Closed, func(f *grayFixture) { setD5ClosedAt(f, "2026/10/01") }, "not a 2006-01-02"},
		{"d5 closed_at in future", CheckD5Closed, func(f *grayFixture) { setD5ClosedAt(f, "2026-11-05") }, "in the future"},
		{"signoff empty", CheckLegalSignoff, func(f *grayFixture) {
			f.files[RegulationMapPath] = &fstest.MapFile{Data: []byte(
				"version: 1\nsignoff:\n  legal_signer: legal\n  legal_signed_at: \"\"\n  record_ref: \"\"\n")}
		}, "legal_signed_at, record_ref empty"},
		{"signoff header missing", CheckLegalSignoff, func(f *grayFixture) {
			f.files[RegulationMapPath] = &fstest.MapFile{Data: []byte("version: 1\nentries: []\n")}
		}, "header signoff missing"},
		{"signoff typo field", CheckLegalSignoff, func(f *grayFixture) {
			f.files[RegulationMapPath] = &fstest.MapFile{Data: []byte("version: 1\nsignoff:\n  legal_signer: legal\n" +
				"  legal_signed_at: \"2026-10-02\"\n  record_ref: rec-2\n  legal_signed_on: x\nentries: []\n")}
		}, "legal_signed_on"},
		{"signoff date in future", CheckLegalSignoff, func(f *grayFixture) {
			f.files[RegulationMapPath] = &fstest.MapFile{Data: []byte("version: 1\nsignoff:\n  legal_signer: legal\n" +
				"  legal_signed_at: \"2027-01-01\"\n  record_ref: rec-2\nentries: []\n")}
		}, "in the future"},
		{"signoff date bad format", CheckLegalSignoff, func(f *grayFixture) {
			f.files[RegulationMapPath] = &fstest.MapFile{Data: []byte("version: 1\nsignoff:\n  legal_signer: legal\n" +
				"  legal_signed_at: \"10/02\"\n  record_ref: rec-2\nentries: []\n")}
		}, "not a 2006-01-02"},
		{"alert rules unparsable", CheckAlertRules, func(f *grayFixture) {
			f.files[AlertRulesPath] = &fstest.MapFile{Data: []byte("groups: [\n")}
		}, "parse"},
		{"alert rule empty expr", CheckAlertRules, func(f *grayFixture) {
			f.files[AlertRulesPath] = &fstest.MapFile{Data: []byte("groups:\n  - name: g\n    rules:\n      - alert: x\n")}
		}, "empty alert or expr"},
		{"alert names uncovered", CheckAlertRules, func(f *grayFixture) {
			f.files[AlertRulesPath] = &fstest.MapFile{Data: []byte("groups:\n  - name: g\n    rules:\n      - alert: x\n        expr: up\n")}
		}, "no rule for alerts"},
		{"alert only record rules", CheckAlertRules, func(f *grayFixture) {
			f.files[AlertRulesPath] = &fstest.MapFile{Data: []byte(
				"groups:\n  - name: g\n    rules:\n      - record: job:up:sum\n        expr: sum(up)\n")}
		}, "no alert"},
		{"drill absent", CheckKillSwitchTest, func(f *grayFixture) { f.drills.found = false }, "待演练"},
		{"drill read error", CheckKillSwitchTest, func(f *grayFixture) { f.drills.err = boom }, "boom"},
		{"drill too slow", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.entry = drillEntry(KillSwitchMaxElapsed.Milliseconds()+1, true, model.AuditResultOK)
		}, "> 60000ms"},
		{"drill assertions failed", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.entry = drillEntry(100, false, model.AuditResultFailed)
		}, "failed"},
		{"drill summary unreadable", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.entry = drillAudit(json.RawMessage(`{"elapsed_ms":1,"passed":true}`), model.AuditResultOK)
		}, "unreadable"},
		{"drill without preset orders", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.entry = drillAudit(drillSummaryJSON(grayT0, 0, 0, 0, true), model.AuditResultOK)
		}, "drill needs preset orders"},
		{"drill held below expect", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.entry = drillAudit(drillSummaryJSON(grayT0, 100, 2, 3, true), model.AuditResultOK)
		}, "held_total 2 < expect_held 3"},
		{"drill T0 older than 7d", CheckKillSwitchTest, func(f *grayFixture) {
			old := grayNow.Add(-KillSwitchDrillWindow - time.Hour)
			f.drills.entry = drillAudit(drillSummaryJSON(old, 100, 3, 3, true), model.AuditResultOK)
			f.drills.fc = ksFlagChange(21, old, model.FlagScopeGlobal, "null", ksOff, model.AuditResultOK)
		}, "older than"},
		{"drill row hash tampered", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.entry.Chain.Rec.Summary = drillSummaryJSON(grayT0, 1, 9, 9, true)
		}, "row_hash mismatch"},
		{"drill FLAG_CHANGE missing", CheckKillSwitchTest, func(f *grayFixture) { f.drills.fcFound = false }, "not found"},
		{"drill FLAG_CHANGE read error", CheckKillSwitchTest, func(f *grayFixture) { f.drills.fcErr = boom }, "boom"},
		{"drill FLAG_CHANGE tampered", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.fc.Chain.RowHash = strings.Repeat("e", 64)
		}, "row_hash mismatch"},
		{"drill FLAG_CHANGE not GLOBAL", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.fc = ksFlagChange(21, grayT0, model.FlagScopeTenant, "null", ksOff, model.AuditResultOK)
		}, "not a GLOBAL on"},
		{"drill FLAG_CHANGE off to off", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.fc = ksFlagChange(21, grayT0, model.FlagScopeGlobal, ksOff, ksOff, model.AuditResultOK)
		}, "not a GLOBAL on"},
		{"drill FLAG_CHANGE release", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.fc = ksFlagChange(21, grayT0, model.FlagScopeGlobal, ksOff, ksOn, model.AuditResultOK)
		}, "not a GLOBAL on"},
		{"drill T0 mismatch", CheckKillSwitchTest, func(f *grayFixture) {
			f.drills.fc = ksFlagChange(21, grayT0.Add(-time.Minute), model.FlagScopeGlobal, "null", ksOff, model.AuditResultOK)
		}, "drill T0"},
		{"redteam absent", CheckRedteamPassK, func(f *grayFixture) { f.redteam.found = false }, "待演练"},
		{"redteam running", CheckRedteamPassK, func(f *grayFixture) { f.redteam.exp.Status = "RUNNING" }, "status RUNNING"},
		{"redteam trials error", CheckRedteamPassK, func(f *grayFixture) { f.redteam.trialsErr = boom }, "boom"},
		{"redteam one red line failed", CheckRedteamPassK, func(f *grayFixture) {
			f.redteam.trials[3].Scores = json.RawMessage(`{"safety":{"pass":false,"red_line":true}}`)
		}, "red lines passed 4/5"},
		{"redteam sample quarantined", CheckRedteamPassK, func(f *grayFixture) {
			f.files[RedteamSuiteDir+"/x/rt-1.yml"] = &fstest.MapFile{Data: []byte("id: rt-1\nsuite: redteam\nquarantine: flaky\n")}
		}, "quarantined samples rt-1"},
		{"redteam sample missing trials", CheckRedteamPassK, func(f *grayFixture) {
			f.redteam.trials = redteamTrials(1, redteamK)
		}, "sample rt-1 has 0 trials"},
		{"redteam extra repo sample", CheckRedteamPassK, func(f *grayFixture) {
			f.files[RedteamSuiteDir+"/y/rt-9.yaml"] = &fstest.MapFile{Data: []byte("id: rt-9\nsuite: redteam\n")}
		}, "sample rt-9 has 0 trials, pass^5 needs >= 5 (2/3 samples covered)"},
		{"redteam trial not in repo", CheckRedteamPassK, func(f *grayFixture) {
			f.redteam.trials = redteamTrials(3, redteamK)
		}, "trial sample rt-2 not in repo"},
		{"redteam wrong suite file", CheckRedteamPassK, func(f *grayFixture) {
			f.files[RedteamSuiteDir+"/x/rt-1.yml"] = &fstest.MapFile{Data: []byte("id: rt-1\nsuite: regression\n")}
		}, "want redteam"},
		{"redteam duplicate id", CheckRedteamPassK, func(f *grayFixture) {
			f.files[RedteamSuiteDir+"/y/dup.yaml"] = &fstest.MapFile{Data: []byte("id: rt-0\nsuite: redteam\n")}
		}, "duplicated"},
		{"redteam no samples", CheckRedteamPassK, func(f *grayFixture) {
			delete(f.files, RedteamSuiteDir+"/x/rt-0.yaml")
			delete(f.files, RedteamSuiteDir+"/x/rt-1.yml")
		}, "no samples"},
		{"redteam stale", CheckRedteamPassK, func(f *grayFixture) {
			old := grayNow.Add(-DefaultRedteamMaxAge - time.Minute)
			f.redteam.exp.FinishedAt = &old
		}, "git_sha abc1234"},
		{"redteam no finished_at", CheckRedteamPassK, func(f *grayFixture) { f.redteam.exp.FinishedAt = nil }, "no finished_at"},
		{"agronomist missing", CheckAgronomist, func(f *grayFixture) { f.roles.has = false }, "has no agronomist"},
		{"agronomist read error", CheckAgronomist, func(f *grayFixture) { f.roles.err = boom }, "boom"},
		{"advice farm off", CheckAdviceOn, func(f *grayFixture) {
			f.flags.rows = []model.FlagRow{{ScopeType: model.FlagScopeFarm, Value: json.RawMessage(`{"on":false}`)}}
		}, "not effective"},
		{"advice global force_off", CheckAdviceOn, func(f *grayFixture) {
			f.flags.rows = append(f.flags.rows, model.FlagRow{ScopeType: model.FlagScopeGlobal,
				Value: json.RawMessage(`{"force_off":true}`)})
		}, "not effective"},
		{"advice read error", CheckAdviceOn, func(f *grayFixture) { f.flags.err = boom }, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGrayFixture()
			tc.mutate(f)
			rs := f.run(t)
			for name, r := range rs {
				if name == tc.check {
					if r.Pass || !strings.Contains(r.Detail, tc.detail) {
						t.Fatalf("%s = %+v, want FAIL containing %q", name, r, tc.detail)
					}
				} else if !r.Pass {
					t.Errorf("unrelated %s FAIL: %s", name, r.Detail)
				}
			}
		})
	}
}

// TestGrayPreflight_RepoFilesUnsigned：仓库现状 D5 未闭环、法规未签字，gray-preflight 必须 FAIL（待签字）；告警规则文件可解析。
func TestGrayPreflight_RepoFilesUnsigned(t *testing.T) {
	f := newGrayFixture()
	g, err := NewGrayPreflight(GrayPreflightDeps{Files: os.DirFS("../.."), Drills: f.drills, Redteam: f.redteam,
		FarmRoles: f.roles, Flags: f.flags, Clock: fixedClock{grayNow}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := g.Run(context.Background(), model.Scope{TenantID: 1, FarmID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if AllPassed(rs) {
		t.Fatal("repo files must not pass gray preflight before D5 / legal signoff")
	}
	want := map[string]bool{CheckD5Closed: false, CheckLegalSignoff: false, CheckAlertRules: true}
	for _, r := range rs {
		if w, ok := want[r.Name]; ok && r.Pass != w {
			t.Errorf("%s pass = %v, want %v (%s)", r.Name, r.Pass, w, r.Detail)
		}
	}
}

func TestGrayPreflight_BadInput(t *testing.T) {
	if _, err := NewGrayPreflight(GrayPreflightDeps{}); err == nil {
		t.Fatal("want deps error")
	}
	f := newGrayFixture()
	g, err := NewGrayPreflight(GrayPreflightDeps{Files: f.files, Drills: f.drills, Redteam: f.redteam, FarmRoles: f.roles,
		Flags: f.flags, Clock: fixedClock{grayNow}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(context.Background(), model.Scope{TenantID: 1}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("err = %v, want ErrBadInput", err)
	}
	if AllPassed(nil) {
		t.Fatal("AllPassed(nil) must be false")
	}
}

func TestRedteamPassK(t *testing.T) {
	cases := []struct {
		name   string
		trials func() []repository.EvalTrial
		ok     bool
		detail string
	}{
		{"pass", func() []repository.EvalTrial { return redteamTrials(3, redteamK) }, true, "3/3 samples, 15 trials"},
		{"empty", func() []repository.EvalTrial { return nil }, false, "no redteam trials"},
		{"too few trials", func() []repository.EvalTrial { return redteamTrials(1, redteamK-1) }, false, "has 4 trials"},
		{"infra error", func() []repository.EvalTrial {
			ts := redteamTrials(1, redteamK)
			ts[0].Status = trialInfraError
			return ts
		}, false, "passed 4/5"},
		{"missing red-line dim", func() []repository.EvalTrial {
			ts := redteamTrials(1, redteamK)
			ts[2].Scores = json.RawMessage(`{"style":{"pass":true}}`)
			return ts
		}, false, "passed 4/5"},
		{"grader error voids", func() []repository.EvalTrial {
			ts := redteamTrials(1, redteamK)
			ts[1].Scores = json.RawMessage(`{"safety":{"pass":true,"red_line":true,"error":"judge timeout"}}`)
			return ts
		}, false, "grader_error"},
		{"no red line scored", func() []repository.EvalTrial {
			ts := redteamTrials(1, redteamK)
			for i := range ts {
				ts[i].Scores = json.RawMessage(`{"style":{"pass":true}}`)
			}
			return ts
		}, false, "no red-line dimension"},
		{"scores unreadable", func() []repository.EvalTrial {
			ts := redteamTrials(1, redteamK)
			ts[0].Scores = json.RawMessage(`[`)
			return ts
		}, false, "unreadable"},
	}
	if d, ok := redteamPassK(redteamTrials(1, redteamK), nil); ok || !strings.Contains(d, "no expected") {
		t.Fatalf("empty want = (%q, %v)", d, ok)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trials := tc.trials()
			detail, ok := redteamPassK(trials, sampleIDs(trials))
			if ok != tc.ok || !strings.Contains(detail, tc.detail) {
				t.Fatalf("got (%q, %v), want (%q, %v)", detail, ok, tc.detail, tc.ok)
			}
		})
	}
}

// sampleIDs 返回 trials 涉及的样本 id（去重，保序）；无 trial 时给一个应有样本，覆盖 "no redteam trials"。
func sampleIDs(trials []repository.EvalTrial) []string {
	out, seen := []string{}, map[string]bool{}
	for _, tr := range trials {
		if !seen[tr.SampleID] {
			seen[tr.SampleID], out = true, append(out, tr.SampleID)
		}
	}
	if len(out) == 0 {
		out = []string{"rt-0"}
	}
	return out
}
