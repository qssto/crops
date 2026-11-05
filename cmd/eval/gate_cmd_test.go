package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/gate"
	"agri-steward/internal/evalkit/report"
)

// 退出判定（简报 L7-b 第 6 条）：PASS 与已签字 NEED_SIGNOFF 退出 0，其余（含未签字 NEED_SIGNOFF）非 0。
func TestGateExit(t *testing.T) {
	cases := []struct {
		v      gate.Verdict
		signed bool
		ok     bool
	}{
		{gate.Pass, false, true},
		{gate.NeedSignoff, true, true},
		{gate.NeedSignoff, false, false},
		{gate.BlockRedLine, true, false},
		{gate.BlockQuality, false, false},
		{gate.NotAttributable, true, false},
	}
	for _, c := range cases {
		err := gateExit(c.v, c.signed)
		if (err == nil) != c.ok {
			t.Errorf("%s signed=%t: err = %v", c.v, c.signed, err)
		}
		if err != nil && !errors.Is(err, errGateBlocked) {
			t.Errorf("%s: err = %v, want errGateBlocked", c.v, err)
		}
	}
}

func TestCmdGate_RequiresFlags(t *testing.T) {
	empty := t.TempDir()
	for _, args := range [][]string{{}, {"--exp", "3", "--baselines-dir", empty}, {"--baseline", "baseline_p1"}} {
		if err := cmdGate(t.Context(), args, &discard{}, &discard{}); err == nil || errors.Is(err, errGateBlocked) {
			t.Errorf("%v: err = %v, want usage error", args, err)
		}
	}
}

// 评审 P2：release 门禁不再隐式用 baseline_p1。--baseline 为空时读 {baselines-dir}/release.yaml 指针（调度方冻结基线后写），
// 指针缺失、格式错、指向非法名或指向自身都失败关闭（连库前报错）。
func TestGateFlags_ReleasePointer(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, report.ReleasePointer+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	parse := func(dir string, extra ...string) (gateFlags, error) {
		var f gateFlags
		err := f.parse(append([]string{"--exp", "9", "--baselines-dir", dir}, extra...))
		return f, err
	}

	f, err := parse(write(t, "baseline: baseline_p2\n"))
	if err != nil || f.baseline != "baseline_p2" {
		t.Fatalf("pointer: baseline=%q err=%v, want baseline_p2", f.baseline, err)
	}
	if f, err := parse(write(t, "baseline: baseline_p2\n"), "--baseline", "baseline_p1"); err != nil || f.baseline != "baseline_p1" {
		t.Fatalf("explicit --baseline: baseline=%q err=%v, want baseline_p1 (explicit wins)", f.baseline, err)
	}
	if _, err := parse(t.TempDir()); err == nil || !strings.Contains(err.Error(), "release.yaml") {
		t.Fatalf("missing pointer: err = %v, want error naming release.yaml", err)
	}
	for name, body := range map[string]string{
		"empty":         "",
		"blank name":    "baseline: \"\"\n",
		"bad name":      "baseline: ../x\n",
		"self":          "baseline: release\n",
		"unknown field": "baseline: baseline_p2\nexp_id: \"3\"\n",
		"numeric id":    "baseline: \"42\"\n",
	} {
		if f, err := parse(write(t, body)); err == nil {
			t.Errorf("%s pointer: baseline=%q, want error", name, f.baseline)
		}
	}
}

// 评审 P2：基线与候选的套件不同（如 baseline_p1 只跑 regression、release 跑全部套件）直接报错、不给结论。
func TestCheckGateSuites(t *testing.T) {
	b := evalkit.Snapshot{Suites: []string{"regression"}}
	c := evalkit.Snapshot{Suites: []string{"regression", "holdout", "capability"}}
	err := checkGateSuites(b, c)
	if err == nil || !strings.Contains(err.Error(), "基线套件 regression 与候选 capability,holdout,regression 不一致") {
		t.Fatalf("err = %v, want 基线套件 … 与候选 … 不一致", err)
	}
	if !errors.Is(err, errGateSuites) {
		t.Fatalf("err = %v, want errGateSuites", err)
	}
	same := evalkit.Snapshot{Suites: []string{"regression", "capability", "holdout"}}
	if err := checkGateSuites(same, c); err != nil {
		t.Fatalf("same suites in another order: %v", err)
	}
}

// 评审 P2：user_sim 样本优先按候选 run 快照记录的 ID 判定（样本目录之后改了 tag 不影响已跑实验）；
// 快照没记录（旧实验 / 本次无 user_sim 样本）才退回扫描样本目录。
func TestUserSimPredicate(t *testing.T) {
	dir := map[string]bool{"dir-sim": true}
	capTrial := func(id string) evalkit.Trial { return evalkit.Trial{SampleID: id, Suite: gate.SuiteCapability} }
	recorded := userSimPredicate(evalkit.Snapshot{UserSim: &evalkit.UserSimSnapshot{SampleIDs: []string{"snap-sim"}}}, dir)
	if !recorded(capTrial("snap-sim")) || recorded(capTrial("dir-sim")) {
		t.Fatal("recorded snapshot ids not preferred over sample dir")
	}
	legacy := userSimPredicate(evalkit.Snapshot{}, dir)
	if !legacy(capTrial("dir-sim")) || legacy(capTrial("snap-sim")) {
		t.Fatal("snapshot without user_sim did not fall back to sample dir")
	}
}

// 评审 P3：Record 的 ok=false（非单因子对比，没有 factor_diff 可写）不得被丢弃后照写 gate_verdict。
func TestGateFactorDiff(t *testing.T) {
	if _, err := gateFactorDiff(&compare.Result{Mode: compare.ModePhase}); err == nil {
		t.Fatal("phase-mode compare result: want error, got nil")
	}
	diff, err := gateFactorDiff(&compare.Result{Mode: compare.ModeFactor})
	if err != nil || string(diff) != "[]" {
		t.Fatalf("factor-mode diff = %s err=%v, want []", diff, err)
	}
}

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }
