//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

// l7bTrials：红线 pass^3 要求 n ≥ 3（n < 3 失败关闭判不过），gate 集成测试按 3 次跑。
const l7bTrials = 3

// gateArgs 是 gate 子命令参数：配置文件取仓库内的绝对路径，签字校验用 repo（临时 git 仓库，信任 ref 为本地 main）。
func gateArgs(exp, base uint64, repo, out string) []string {
	a := []string{"gate", "--exp", store.FormatID(exp), "--baseline", store.FormatID(base), "--out", out, "--repo", repo,
		"--trust-ref", sigTrustRef,
		"--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--thresholds", filepath.Join(repoRoot, defaultThresholds),
		"--cost-latency", filepath.Join(repoRoot, defaultCostLatency),
		"--judges", filepath.Join(repoRoot, review.DefaultGate)}
	return append(a, storeArgs()...)
}

func verdictOf(t *testing.T, exps *store.Experiments, id uint64) *store.GateVerdict {
	t.Helper()
	g, err := exps.GateVerdictOf(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// overConservative 把候选全部「拒答判断通过」的 trial 改成过度保守（refusal_correct 仍 pass，红线不变）：
// 拒答正确率 1 → 0、过度保守率 0 → 1，构造可信度退化。返回改动行数。
func overConservative(t *testing.T, db *repository.DB, id uint64) int64 {
	t.Helper()
	res := db.Exec(`UPDATE trial SET scores = JSON_SET(scores, '$.refusal_correct.evidence', 'over_conservative: 构造回归')
  WHERE experiment_id = ? AND JSON_EXTRACT(scores, '$.refusal_correct.pass') = CAST('true' AS JSON)
  AND JSON_EXTRACT(scores, '$.refusal_correct.not_applicable') IS NULL`, id)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	return res.RowsAffected
}

// TestL7b_Gate_Integration（§6.8 验收 2）：同配置候选 judge 全未评 → NEED_SIGNOFF，无签字退出非 0，CODEOWNERS 作者签字后
// 写 signed_by、退出 0；构造回归候选 → BLOCK_QUALITY，报告首行结论、翻转样本逐条列出；多因子 → NOT_ATTRIBUTABLE 且清空签字。
func TestL7b_Gate_Integration(t *testing.T) {
	ctx := context.Background()
	db, _ := evalDB(t)
	exps := store.New(db)
	base := runExp(t, "regression", l7bTrials, l0Tags, store.TriggerManual)
	if _, err := exps.Freeze(ctx, base, nil); err != nil {
		t.Fatal(err)
	}
	repo := newSigRepo(t)

	same := copyExp(t, db, base, "DONE", true, true)
	out, errOut, code := cli(t, gateArgs(same, base, repo.dir, t.TempDir())...)
	if code != exitFailed || !strings.Contains(out, "verdict=NEED_SIGNOFF signed=false") || !strings.Contains(errOut, errGateBlocked.Error()) {
		t.Fatalf("unsigned gate code = %d out=%s stderr=%s", code, out, errOut)
	}
	if g := verdictOf(t, exps, same); g.Verdict != store.VerdictNeedSignoff || g.SignedBy != nil {
		t.Fatalf("unsigned gate row = %+v", g)
	}
	repo.commit("owner@example.com", fmt.Sprintf("eval/signoffs/%d.md", same),
		fmt.Sprintf("experiment_id: %d\n\n构造签字：judge 维度未评（待专家），集成测试用。\n", same))
	out, errOut, code = cli(t, gateArgs(same, base, repo.dir, t.TempDir())...)
	if code != exitOK || !strings.Contains(out, "verdict=NEED_SIGNOFF signed=true") {
		t.Fatalf("signed gate code = %d out=%s stderr=%s", code, out, errOut)
	}
	if g := verdictOf(t, exps, same); g.SignedBy == nil || *g.SignedBy != 42 {
		t.Fatalf("signed gate row = %+v", g)
	}
	// 评审 P2：每次 gate 先清 signed_by 再校验签字。签字被撤（main 上删除签字文件）后同结论重跑：退出非 0，库里也无签字。
	repo.git("owner@example.com", "rm", "-q", fmt.Sprintf("eval/signoffs/%d.md", same))
	repo.git("owner@example.com", "commit", "-q", "-m", "revoke")
	out, errOut, code = cli(t, gateArgs(same, base, repo.dir, t.TempDir())...)
	if code != exitFailed || !strings.Contains(out, "verdict=NEED_SIGNOFF signed=false") {
		t.Fatalf("revoked gate code = %d out=%s stderr=%s", code, out, errOut)
	}
	if g := verdictOf(t, exps, same); g.Verdict != store.VerdictNeedSignoff || g.SignedBy != nil {
		t.Fatalf("revoked gate row = %+v, want signed_by NULL", g)
	}
	// 评审 P2：不给 --baseline 时读 {baselines-dir}/release.yaml 指针（指向 {名}.yaml 的 exp_id），与显式给同一基线结论一致。
	pointerDir := t.TempDir()
	for name, body := range map[string]string{
		"release.yaml": "baseline: gate_it\n",
		"gate_it.yaml": fmt.Sprintf("exp_id: %q\n", store.FormatID(base)),
	} {
		if err := os.WriteFile(filepath.Join(pointerDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := gateArgs(same, base, repo.dir, t.TempDir())
	if args[3] != "--baseline" {
		t.Fatalf("gateArgs layout changed: %v", args)
	}
	args = append(append(args[:3:3], args[5:]...), "--baselines-dir", pointerDir)
	out, errOut, code = cli(t, args...)
	if code != exitFailed || !strings.Contains(out, fmt.Sprintf("baseline=%d", base)) || !strings.Contains(out, "verdict=NEED_SIGNOFF") {
		t.Fatalf("release pointer gate code = %d out=%s stderr=%s", code, out, errOut)
	}

	reg := copyExp(t, db, base, "DONE", true, true)
	if n := overConservative(t, db, reg); n == 0 {
		t.Fatal("no refusal_correct trial to regress")
	}
	outDir := t.TempDir()
	out, errOut, code = cli(t, gateArgs(reg, base, repo.dir, outDir)...)
	if code != exitFailed || !strings.Contains(out, "verdict=BLOCK_QUALITY") {
		t.Fatalf("regressed gate code = %d out=%s stderr=%s", code, out, errOut)
	}
	if g := verdictOf(t, exps, reg); g.Verdict != store.VerdictBlockQuality || g.SignedBy != nil {
		t.Fatalf("regressed gate row = %+v", g)
	}
	rep, err := os.ReadFile(filepath.Join(outDir, "gate", fmt.Sprintf("%d-%d.md", base, reg)))
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(string(rep), "\n"); !strings.Contains(first, "门禁结论：BLOCK_QUALITY") {
		t.Fatalf("report first line = %q", first)
	}
	for _, want := range []string{"- regressed refusal_accuracy ", "- regressed over_conservative_rate ", "trust_regression"} {
		if !strings.Contains(string(rep), want) {
			t.Fatalf("report missing %q:\n%s", want, rep)
		}
	}
	// 签字不能覆盖阻断。
	if err := exps.SetSignedBy(ctx, reg, 42); !errors.Is(err, store.ErrNotSignable) {
		t.Fatalf("sign BLOCK_QUALITY: %v", err)
	}

	// 多因子（提示词 + 加载策略）：NOT_ATTRIBUTABLE，结论变化清空已签字。
	if err := db.Exec(`UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.prompt_version', 'v-l7b', '$.loader_strategy', 'v-l7b')
  WHERE id = ?`, same).Error; err != nil {
		t.Fatal(err)
	}
	out, errOut, code = cli(t, gateArgs(same, base, repo.dir, t.TempDir())...)
	if code != exitFailed || !strings.Contains(out, "verdict=NOT_ATTRIBUTABLE") {
		t.Fatalf("multi-factor gate code = %d out=%s stderr=%s", code, out, errOut)
	}
	if g := verdictOf(t, exps, same); g.Verdict != store.VerdictNotAttributable || g.SignedBy != nil {
		t.Fatalf("multi-factor gate row = %+v", g)
	}
	l7bRefused(t, db, exps, base, repo.dir)
}

// l7bRefused：VOID / BUDGET_EXCEEDED / 阶段级对比的实验不给结论（gate 退出非 0、gate_verdict 不写），store 层同样拒绝。
func l7bRefused(t *testing.T, db *repository.DB, exps *store.Experiments, base uint64, repo string) {
	t.Helper()
	ctx := context.Background()
	// 作废用 GRADER_ERROR（出现即作废）：基线 9 样本 × 3 = 27 trial，1 个 INFRA_ERROR 只占 3.7%，不过 5% 阈值。
	void, st := settled(t, db, base, false, "UPDATE trial SET status = 'GRADER_ERROR' WHERE experiment_id = ? LIMIT 1")
	if st.Status != store.StatusVoid || st.VoidReason != store.VoidGraderError {
		t.Fatalf("void settle = %+v", st)
	}
	over, st := settled(t, db, base, true, "")
	if st.Status != store.StatusBudgetExceeded {
		t.Fatalf("budget settle = %+v", st)
	}
	phase := copyExp(t, db, base, "DONE", true, true)
	if err := db.Exec("UPDATE experiment SET baseline_id = ?, compare_mode = ? WHERE id = ?", base, store.ComparePhase, phase).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{void, over, phase} {
		if _, errOut, code := cli(t, gateArgs(id, base, repo, t.TempDir())...); code != exitFailed ||
			!strings.Contains(errOut, errGateRefused.Error()) {
			t.Fatalf("gate %d: code = %d stderr=%s", id, code, errOut)
		}
		if g := verdictOf(t, exps, id); g.Verdict != "" {
			t.Fatalf("refused exp %d has gate_verdict %+v", id, g)
		}
	}
	if err := exps.RecordGate(ctx, void, base, []byte("[]"), store.VerdictPass); !errors.Is(err, store.ErrBadStatus) {
		t.Fatalf("store RecordGate VOID: %v", err)
	}
	if err := exps.RecordGate(ctx, phase, base, []byte("[]"), store.VerdictPass); !errors.Is(err, store.ErrPhaseCompare) {
		t.Fatalf("store RecordGate PHASE: %v", err)
	}
}
