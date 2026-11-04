//go:build integration

package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

// l7aBudget 是集成测试给 run 的 token 预算（桩模型 token 为 0，不会超出）。
const l7aBudget = 1000000

// settled 复制 src（含 trial）为 DONE 实验，执行 mutate 改 trial 后 Settle，返回新 id 与判定。
func settled(t *testing.T, db *repository.DB, src uint64, budgetExceeded bool, mutate string) (uint64, store.Settlement) {
	t.Helper()
	id := copyExp(t, db, src, "DONE", true, true)
	if mutate != "" {
		if err := db.Exec(mutate, id).Error; err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.New(db).Settle(context.Background(), id, budgetExceeded, 0.05)
	if err != nil {
		t.Fatalf("settle %d: %v", id, err)
	}
	return id, st
}

// TestL7a_VoidBudget_Integration：run --token-budget 写 token_budget 并 DONE；注入 INFRA_ERROR / GRADER_ERROR 后
// Settle 判 VOID（void_reason 落库），超预算判 BUDGET_EXCEEDED；三者 compare / freeze 一律拒绝。
func TestL7a_VoidBudget_Integration(t *testing.T) {
	ctx := context.Background()
	db, _ := evalDB(t)
	exps := store.New(db)
	args := append(runArgs(t), "--suite", "regression", "--n", strconv.Itoa(l0Trials), "--tags", l0Tags,
		"--trigger", store.TriggerManual, "--token-budget", strconv.Itoa(l7aBudget))
	out, errOut, code := cli(t, args...)
	if code != exitOK || !strings.Contains(out, "status=DONE") {
		t.Fatalf("run code = %d out=%s stderr=%s", code, out, errOut)
	}
	base := expIDOf(t, out)
	g, err := exps.GateOf(ctx, base)
	if err != nil || g.Status != "DONE" || g.TokenBudget == nil || *g.TokenBudget != l7aBudget || g.VoidReason != nil {
		t.Fatalf("gate = %+v err=%v", g, err)
	}
	// 不带 --token-budget 的 manual 实验：token_budget 为 NULL。
	if n := countRows(t, db, "SELECT COUNT(*) FROM experiment WHERE id = ? AND token_budget IS NULL",
		runExp(t, "regression", l0Trials, l0Tags, store.TriggerManual)); n != 1 {
		t.Fatalf("manual run token_budget not NULL: %d", n)
	}

	infra, st := settled(t, db, base, false, "UPDATE trial SET status = 'INFRA_ERROR', scores = NULL WHERE experiment_id = ? LIMIT 1")
	if st.Status != store.StatusVoid || st.VoidReason != store.VoidInfraErrorRate {
		t.Fatalf("infra settle = %+v", st)
	}
	grader, st := settled(t, db, base, false, "UPDATE trial SET status = 'GRADER_ERROR' WHERE experiment_id = ? LIMIT 1")
	if st.Status != store.StatusVoid || st.VoidReason != store.VoidGraderError {
		t.Fatalf("grader settle = %+v", st)
	}
	over, st := settled(t, db, base, true, "")
	if st.Status != store.StatusBudgetExceeded {
		t.Fatalf("budget settle = %+v", st)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM experiment WHERE id = ? AND status = 'VOID' AND void_reason = 'GRADER_ERROR'", grader); n != 1 {
		t.Fatalf("void_reason not stored: %d", n)
	}

	baselines := t.TempDir()
	freeze := append([]string{"baseline", "freeze", "--name", "l7a_base", "--exp", store.FormatID(base), "--baselines-dir", baselines},
		storeArgs()...)
	if out, errOut, code := cli(t, freeze...); code != exitOK {
		t.Fatalf("freeze code = %d out=%s stderr=%s", code, out, errOut)
	}
	for _, id := range []uint64{infra, grader, over} {
		if _, err := exps.Freeze(ctx, id, nil); !errors.Is(err, store.ErrNotDone) {
			t.Fatalf("freeze %d: %v", id, err)
		}
		cmp := append([]string{"compare", "--baseline", "l7a_base", "--candidate", store.FormatID(id),
			"--baselines-dir", baselines, "--out", t.TempDir(), "--repo", repoRoot}, storeArgs()...)
		if _, errOut, code := cli(t, cmp...); code != exitFailed || !strings.Contains(errOut, "not DONE") {
			t.Fatalf("compare %d: code = %d stderr=%s", id, code, errOut)
		}
	}
}

// TestL7a_Resume_Integration：删一条 trial、把一条置 INFRA_ERROR 并判 VOID 后，快照不符拒绝；resume 只补这两条、
// 写回同一实验并重判为 DONE；再 resume 无可补；冻结后拒绝。
func TestL7a_Resume_Integration(t *testing.T) {
	ctx := context.Background()
	db, _ := evalDB(t)
	exps := store.New(db)
	id := runExp(t, "regression", l0Trials, l0Tags, store.TriggerManual)
	total := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ?", id)
	if err := db.Exec("DELETE FROM trial WHERE experiment_id = ? AND trial_index = 1 ORDER BY sample_id LIMIT 1", id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE trial SET status = 'INFRA_ERROR', scores = NULL WHERE experiment_id = ? AND trial_index = 0 ORDER BY sample_id LIMIT 1", id).Error; err != nil {
		t.Fatal(err)
	}
	if st, err := exps.Settle(ctx, id, false, 0.05); err != nil || st.Status != store.StatusVoid {
		t.Fatalf("settle = %+v %v", st, err)
	}
	resume := append(append([]string{"resume"}, runArgs(t)[1:]...), "--exp", store.FormatID(id))

	// 快照可比因子不符（如提示词版本已变）：拒绝，库不动。
	setPrompt := func(v string) {
		t.Helper()
		if err := db.Exec("UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.prompt_version', ?) WHERE id = ?", v, id).Error; err != nil {
			t.Fatal(err)
		}
	}
	var prompt string
	if err := db.Raw("SELECT JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.prompt_version')) FROM experiment WHERE id = ?", id).Scan(&prompt).Error; err != nil {
		t.Fatal(err)
	}
	setPrompt("v-l7a-other")
	if _, errOut, code := cli(t, resume...); code != exitFailed || !strings.Contains(errOut, errResumeSnapshot.Error()) {
		t.Fatalf("snapshot mismatch: code = %d stderr=%s", code, errOut)
	}
	setPrompt(prompt)
	resumeRollsBack(t, db, id)

	// 快照没记录作废阈值（评审修复前的实验）：拒绝，不读当时配置。
	if err := db.Exec("UPDATE experiment SET snapshot = JSON_REMOVE(snapshot, '$.void_infra_error_rate') WHERE id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := cli(t, resume...); code != exitFailed || !strings.Contains(errOut, "void_infra_error_rate") {
		t.Fatalf("no recorded rate: code = %d stderr=%s", code, errOut)
	}
	if err := db.Exec("UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.void_infra_error_rate', 0.05) WHERE id = ?", id).Error; err != nil {
		t.Fatal(err)
	}

	out, errOut, code := cli(t, resume...)
	if code != exitOK || !strings.Contains(out, "status=DONE") || expIDOf(t, out) != id {
		t.Fatalf("resume code = %d out=%s stderr=%s", code, out, errOut)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ?", id); n != total {
		t.Fatalf("trials after resume = %d, want %d", n, total)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ? AND status = 'INFRA_ERROR'", id); n != 0 {
		t.Fatalf("infra_error left = %d", n)
	}
	if g, err := exps.GateOf(ctx, id); err != nil || g.Status != "DONE" || g.VoidReason != nil {
		t.Fatalf("gate after resume = %+v %v", g, err)
	}
	if _, errOut, code := cli(t, resume...); code != exitFailed || !strings.Contains(errOut, errNothingToResume.Error()) {
		t.Fatalf("nothing to resume: code = %d stderr=%s", code, errOut)
	}
	if _, err := exps.Freeze(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := cli(t, resume...); code != exitFailed || !strings.Contains(errOut, "frozen") {
		t.Fatalf("frozen: code = %d stderr=%s", code, errOut)
	}
}

// resumeRollsBack：store.Resume 在置 DONE 之后、作废判定失败（阈值越界）时整体回滚——VOID 实验仍为 VOID、
// void_reason 与 INFRA_ERROR 行原样保留，不会被洗成 DONE。
func resumeRollsBack(t *testing.T, db *repository.DB, id uint64) {
	t.Helper()
	ctx := context.Background()
	exps := store.New(db)
	rec, err := exps.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var key struct {
		SampleID   string `gorm:"column:sample_id"`
		TrialIndex int    `gorm:"column:trial_index"`
	}
	if err := db.Raw("SELECT sample_id, trial_index FROM trial WHERE experiment_id = ? AND status = 'INFRA_ERROR'", id).Scan(&key).Error; err != nil {
		t.Fatal(err)
	}
	rows := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ?", id)
	fresh := &evalkit.Experiment{Snapshot: *rec.Snapshot, FinishedAt: time.Now().UTC(), Trials: []evalkit.Trial{
		{SampleID: key.SampleID, TrialIndex: key.TrialIndex, Suite: "regression", Scores: []evalkit.Score{{Name: "x", Pass: true, Value: 1}}}}}
	if _, err := exps.Resume(ctx, id, fresh, false, 0.5); !errors.Is(err, store.ErrBadVoidRate) {
		t.Fatalf("resume with bad rate: err = %v", err)
	}
	if g, err := exps.GateOf(ctx, id); err != nil || g.Status != store.StatusVoid || g.VoidReason == nil {
		t.Fatalf("gate after failed settle = %+v %v, want VOID", g, err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ? AND status = 'INFRA_ERROR'", id); n != 1 {
		t.Fatalf("infra_error rows after rollback = %d, want 1", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ?", id); n != rows {
		t.Fatalf("trials after rollback = %d, want %d", n, rows)
	}
}
