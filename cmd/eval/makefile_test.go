package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeEval 在仓库根以 GO=echo 跑 make（只回显 cmd/eval 参数，不连库）；本机没有 make 时跳过。
func makeEval(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	cmd := exec.CommandContext(t.Context(), "make", append([]string{"-s", "GO=echo"}, args...)...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// 进命令行的变量先校验格式再加引号传参：合法值原样透传；非法值（含 shell / make 注入）以退出码 2 拒绝、不执行注入。
func TestMakefile_ValidatesArgs(t *testing.T) {
	good := []struct {
		args []string
		want string
	}{
		{[]string{"eval", "MODE=smoke", "TOKEN_BUDGET=100", "BASELINE=baseline_p1.v2"}, "run ./cmd/eval run --mode smoke --token-budget 100 --baseline baseline_p1.v2"},
		{[]string{"eval", "TAGS=L2,L3"}, "run ./cmd/eval run --suite regression --n 3 --tags L2,L3"},
		{[]string{"eval", "SUITE=regression,redteam", "N=5"}, "run ./cmd/eval run --suite regression,redteam --n 5"},
		{[]string{"eval-nightly", "TOKEN_BUDGET=5"}, "run ./cmd/eval run --mode nightly --token-budget 5"},
		{[]string{"eval-resume", "EXP=7"}, "run ./cmd/eval resume --exp 7"},
		{[]string{"eval-compare", "BASELINE=baseline_p1", "CANDIDATE=42", "FAIL_ON_UNRELEASABLE=1"}, "run ./cmd/eval compare --baseline baseline_p1 --candidate 42 --fail-on-unreleasable"},
		{[]string{"eval-compare", "BASELINE=12", "CANDIDATE=3", "MODE=phase", "REFERENCE=4"}, "run ./cmd/eval compare --baseline 12 --candidate 3 --mode phase --reference 4"},
		{[]string{"eval-promptcheck", "PROMPT_BASE=origin/main"}, "run ./cmd/eval promptcheck --base origin/main"},
		// 评审 P2：不给 GATE_BASELINE（或给空）不再默认 baseline_p1，由 gate 读 eval/baselines/release.yaml 指针。
		{[]string{"eval-gate", "EXP=9"}, "run ./cmd/eval gate --exp 9"},
		{[]string{"eval-gate", "EXP=9", "GATE_BASELINE="}, "run ./cmd/eval gate --exp 9"},
		{[]string{"eval-gate", "EXP=9", "GATE_BASELINE=baseline_p2.v1"}, "run ./cmd/eval gate --exp 9 --baseline baseline_p2.v1"},
	}
	for _, g := range good {
		out, err := makeEval(t, g.args...)
		if err != nil || strings.TrimSpace(out) != g.want {
			t.Errorf("%v: err=%v out=%q", g.args, err, out)
		}
	}

	marker := filepath.Join(t.TempDir(), "pwned")
	bad := []struct {
		target, arg string
	}{
		{"eval", "TOKEN_BUDGET=1; touch " + marker},
		{"eval", "TOKEN_BUDGET=$(shell touch " + marker + ")"},
		{"eval", "TOKEN_BUDGET=0"},
		{"eval", "TOKEN_BUDGET=-5"},
		{"eval", "TOKEN_BUDGET=1e6"},
		{"eval", "MODE=smoke; touch " + marker},
		{"eval", "MODE=weekly"},
		{"eval", "BASELINE=Base"},
		{"eval", "BASELINE=../x"},
		{"eval", "BASELINE=a'$(touch " + marker + ")'"},
		{"eval", "BASELINE=" + strings.Repeat("a", 65)},
		{"eval", "N=3 --tags x"},
		{"eval", "SUITE=regression --n 1"},
		{"eval", "SUITE=,regression"},
		{"eval", "SUITE=regression;touch " + marker},
		{"eval", "TAGS=L2 --n 1"},
		{"eval-resume", "EXP="},
		{"eval-resume", "EXP=1 --x"},
		{"eval-compare", "CANDIDATE=`touch " + marker + "`"},
		{"eval-promptcheck", "PROMPT_BASE=--output=" + marker},
		{"eval-promptcheck", "PROMPT_BASE=main;touch " + marker},
		{"eval-gate", "EXP="},
		{"eval-gate", "EXP=1;touch " + marker},
		{"eval-gate", "GATE_BASELINE=../x"},
		{"eval-gate", "GATE_BASELINE=a'$(touch " + marker + ")'"},
	}
	for _, b := range bad {
		args := []string{b.target, b.arg}
		switch {
		case b.target == "eval-compare":
			args = append(args, "BASELINE=baseline_p1")
		case b.target == "eval-gate" && !strings.HasPrefix(b.arg, "EXP="):
			args = append(args, "EXP=9")
		}
		out, err := makeEval(t, args...)
		var ee *exec.ExitError
		if !errors.As(err, &ee) || !strings.Contains(out, "make: invalid ") {
			t.Errorf("%s %q: err=%v out=%q", b.target, b.arg, err, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s %q: injection executed", b.target, b.arg)
		}
	}
}
