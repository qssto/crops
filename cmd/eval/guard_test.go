package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/gate"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/smoke"
	"agri-steward/internal/evalkit/store"
)

// nightly 的 n 不得小于 graduate 的毕业门槛，否则 capability 永远 0 个可毕业。
func TestRunModes_NightlyCanGraduate(t *testing.T) {
	if n := runModes[modeNightly].n; n < store.GraduateTrials {
		t.Fatalf("nightly n = %d < store.GraduateTrials = %d", n, store.GraduateTrials)
	}
	if m := runModes[modeSmoke]; m.n != 3 || m.trigger != store.TriggerPR || !slices.Equal(m.suites, smoke.Suites) {
		t.Fatalf("smoke = %+v", m)
	}
	if m := runModes[modeRelease]; !m.allSuites || m.n != 5 || m.trigger != store.TriggerRelease {
		t.Fatalf("release = %+v", m)
	}
}

func TestCheckModeFlags(t *testing.T) {
	parse := func(args ...string) *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("suite", "", "")
		fs.Int("n", 1, "")
		fs.String("tags", "", "")
		fs.String("trigger", "", "")
		fs.String("smoke-list", "", "")
		fs.String("mode", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs
	}
	if err := checkModeFlags(parse("--suite", "x"), ""); err != nil {
		t.Fatalf("no mode: %v", err)
	}
	if err := checkModeFlags(parse("--smoke-list", "x"), modeSmoke); err != nil {
		t.Fatalf("smoke-list with mode: %v", err)
	}
	if err := checkModeFlags(parse(), "weekly"); !errors.Is(err, errBadMode) {
		t.Fatalf("bad mode: %v", err)
	}
	for _, f := range []string{"--suite=x", "--n=2", "--tags=x", "--trigger=manual"} {
		if err := checkModeFlags(parse(f), modeNightly); !errors.Is(err, errModeConflict) {
			t.Fatalf("%s with mode: %v", f, err)
		}
	}
}

// release 必须含 holdout，未挂载即失败关闭。
func TestRelease_NeedsHoldout(t *testing.T) {
	m := runModes[modeRelease]
	pool := []*sample.Sample{{ID: "a", Suite: "regression"}, {ID: "b", Suite: "capability"}}
	if _, err := m.suitesOf(pool); !errors.Is(err, errHoldoutMissing) {
		t.Fatalf("no holdout: %v", err)
	}
	got, err := m.suitesOf(append(pool, &sample.Sample{ID: "h", Suite: suiteHoldout}))
	if err != nil || !slices.Equal(got, []string{"capability", "holdout", "regression"}) {
		t.Fatalf("suites = %v %v", got, err)
	}
}

// 不含 holdout 的样本目录下 run --mode release 在连基础设施前就失败（cmd 级；只取 regression/，holdout 挂载后仍成立）。
func TestCmdRun_ReleaseWithoutHoldoutFails(t *testing.T) {
	_, errOut, code := cliUnit(t, "run", "--mode", modeRelease, "--token-budget", "1000",
		"--suites-dir", repoRoot+"/"+defaultDir+"/regression", "--taxonomy", repoRoot+"/"+defaultTax, "--repo", repoRoot)
	if code != exitFailed || !strings.Contains(errOut, "holdout") {
		t.Fatalf("code = %d stderr=%s", code, errOut)
	}
}

func TestCheckBudget(t *testing.T) {
	if err := checkBudget(store.TriggerManual, 0); err != nil {
		t.Fatalf("manual unlimited: %v", err)
	}
	for _, tr := range []string{store.TriggerPR, store.TriggerNightly, store.TriggerRelease} {
		if err := checkBudget(tr, 0); !errors.Is(err, errBudgetRequired) {
			t.Fatalf("%s without budget: %v", tr, err)
		}
		if err := checkBudget(tr, 100); err != nil {
			t.Fatalf("%s with budget: %v", tr, err)
		}
	}
	if err := checkBudget(store.TriggerManual, -1); !errors.Is(err, errBadBudget) {
		t.Fatalf("negative: %v", err)
	}
}

// --mode 的 trigger 也必须带预算（cmd 级，失败在连基础设施之前）。
func TestCmdRun_ModeWithoutBudgetFails(t *testing.T) {
	for _, m := range []string{modeSmoke, modeNightly, modeRelease} {
		if _, errOut, code := cliUnit(t, "run", "--mode", m); code != exitFailed || !strings.Contains(errOut, "--token-budget") {
			t.Fatalf("%s: code = %d stderr=%s", m, code, errOut)
		}
	}
	if _, errOut, code := cliUnit(t, "run", "--mode", modeNightly, "--n", "1"); code != exitFailed ||
		!strings.Contains(errOut, errModeConflict.Error()) {
		t.Fatalf("mode + n: code = %d stderr=%s", code, errOut)
	}
}

// CI 触发（pr / nightly / release）只能经 --mode 得到：手动 --trigger nightly 凑不出 graduate 的「最近 3 次 nightly」。
func TestCmdRun_CITriggerNeedsMode(t *testing.T) {
	for _, tr := range []string{store.TriggerPR, store.TriggerNightly, store.TriggerRelease} {
		_, errOut, code := cliUnit(t, "run", "--trigger", tr, "--token-budget", "100")
		if code != exitFailed || !strings.Contains(errOut, errCITriggerNeedsMode.Error()) {
			t.Fatalf("%s: code = %d stderr=%s", tr, code, errOut)
		}
	}
	if err := checkTrigger("", store.TriggerManual); err != nil {
		t.Fatalf("manual: %v", err)
	}
	if err := checkTrigger(modeNightly, store.TriggerNightly); err != nil {
		t.Fatalf("mode nightly: %v", err)
	}
}

func cliUnit(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// redTrial 构造一条红线维度 pass 的 trial。
func redTrial(id string, i int, pass bool) evalkit.Trial {
	return evalkit.Trial{SampleID: id, TrialIndex: i, Suite: "regression",
		Scores: []evalkit.Score{{Name: "citation_valid", RedLine: true, Pass: pass, Value: 1}}}
}

// userSim 把 trials 改成 capability 套件的 user_sim 样本 u（带 user_sim tag）。
func userSim(trials []evalkit.Trial) []evalkit.Trial {
	out := slices.Clone(trials)
	for i := range out {
		out[i].SampleID, out[i].Suite, out[i].Tags = "u", gate.SuiteCapability, []string{gate.TagUserSim}
	}
	return out
}

// 门禁判定（应失败用例）：VOID / BUDGET_EXCEEDED 一律非 0；CI 触发下红线 pass^3 < 1 非 0，manual 不拦。
func TestGateVerdict(t *testing.T) {
	ok := []evalkit.Trial{redTrial("a", 0, true), redTrial("a", 1, true), redTrial("a", 2, true)}
	bad := []evalkit.Trial{redTrial("a", 0, true), redTrial("a", 1, false), redTrial("a", 2, true)}
	cases := []struct {
		name    string
		trigger string
		exp     evalkit.Experiment
		fail    bool
	}{
		{"pr pass", store.TriggerPR, evalkit.Experiment{Trials: ok, Gate: evalkit.GateStatus{Status: "DONE"}}, false},
		{"pr red line", store.TriggerPR, evalkit.Experiment{Trials: bad, Gate: evalkit.GateStatus{Status: "DONE"}}, true},
		{"manual red line", store.TriggerManual, evalkit.Experiment{Trials: bad, Gate: evalkit.GateStatus{Status: "DONE"}}, false},
		{"void", store.TriggerManual, evalkit.Experiment{Trials: ok, Gate: evalkit.GateStatus{Status: store.StatusVoid}}, true},
		{"budget", store.TriggerManual, evalkit.Experiment{Trials: ok, Gate: evalkit.GateStatus{Status: store.StatusBudgetExceeded}}, true},
		{"pr n<3", store.TriggerPR, evalkit.Experiment{Trials: ok[:2], Gate: evalkit.GateStatus{Status: "DONE"}}, true},
		// P2-L7b 验收 4：user_sim 样本不进红线门禁（nightly 跑 capability 时其红线失败不拦）。
		{"nightly user_sim red line", store.TriggerNightly, evalkit.Experiment{Trials: append(slices.Clone(ok), userSim(bad)...),
			Gate: evalkit.GateStatus{Status: "DONE"}}, false},
	}
	for _, c := range cases {
		err := gateVerdict(c.trigger, &c.exp)
		if (err != nil) != c.fail || (err != nil && !errors.Is(err, errGateFailed)) {
			t.Errorf("%s: err = %v, want fail=%v", c.name, err, c.fail)
		}
	}
}

// fakeSettler 记录 gatedExperiments 的调用。
type fakeSettler struct {
	budget    int64
	budgetErr error
	failed    bool
	settle    store.Settlement
	exceeded  bool
	rate      float64
}

func (f *fakeSettler) Start(context.Context, string, evalkit.Snapshot, time.Time) (uint64, error) {
	return 7, nil
}
func (f *fakeSettler) SetTokenBudget(_ context.Context, _ uint64, b int64) error {
	f.budget = b
	return f.budgetErr
}
func (f *fakeSettler) FinishSettled(_ context.Context, _ uint64, _ *evalkit.Experiment, exceeded bool, rate float64) (store.Settlement, error) {
	f.exceeded, f.rate = exceeded, rate
	return f.settle, nil
}
func (f *fakeSettler) Fail(context.Context, uint64, time.Time) error { f.failed = true; return nil }

func TestGatedExperiments(t *testing.T) {
	ctx := context.Background()
	f := &fakeSettler{settle: store.Settlement{Status: store.StatusVoid, VoidReason: store.VoidGraderError, Hits: []string{store.VoidGraderError}}}
	g := gatedExperiments{s: f, budget: 500, maxRate: 0.05, now: time.Now}
	if id, err := g.Start(ctx, store.TriggerPR, evalkit.Snapshot{}, time.Now()); err != nil || id != 7 || f.budget != 500 {
		t.Fatalf("start id=%d err=%v budget=%d", id, err, f.budget)
	}
	exp := &evalkit.Experiment{BudgetExceeded: true}
	if err := g.Finish(ctx, 7, exp); err != nil {
		t.Fatal(err)
	}
	if !f.exceeded || f.rate != 0.05 || exp.Gate.Status != store.StatusVoid || exp.Gate.VoidReason != store.VoidGraderError ||
		exp.Gate.MaxInfraErrorRate != 0.05 || exp.Gate.TokenBudget != 500 {
		t.Fatalf("gate = %+v exceeded=%v rate=%v", exp.Gate, f.exceeded, f.rate)
	}
	// 预算 0：不写 token_budget（NULL = 不限）。
	f0 := &fakeSettler{}
	if _, err := (gatedExperiments{s: f0, now: time.Now}).Start(ctx, store.TriggerManual, evalkit.Snapshot{}, time.Now()); err != nil || f0.budget != 0 {
		t.Fatalf("no budget: err=%v budget=%d", err, f0.budget)
	}
	// 写预算失败：已建行置 FAILED。
	fe := &fakeSettler{budgetErr: errors.New("db down")}
	if _, err := (gatedExperiments{s: fe, budget: 1, now: time.Now}).Start(ctx, store.TriggerPR, evalkit.Snapshot{}, time.Now()); err == nil || !fe.failed {
		t.Fatalf("budget error: err=%v failed=%v", err, fe.failed)
	}
}
