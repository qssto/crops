//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

// evalDBEnv 覆盖 mysql.eval_database（viper AS_ 前缀）：集成测试不写共享库 agri_eval。
const evalDBEnv = "AS_MYSQL_EVAL_DATABASE"

// itAutoIncrement 让隔离库的 experiment.id 远离共享库已用的 id：MinIO eval bucket 是共享的，
// {id}/manifest.json、report/{id}/ 等对象键不能与真实实验撞车。
const itAutoIncrement = 900000000

// TestMain 建 {AS_TEST_NS}_agri_eval_it 隔离评测库并迁移到 migrations/eval 最新版，再让本包全部集成测试连它。
func TestMain(m *testing.M) {
	if err := isolateEvalDB(); err != nil {
		fmt.Fprintln(os.Stderr, "eval integration setup:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func isolateEvalDB() error {
	cfg, err := config.Load(filepath.Join(repoRoot, "configs/app.yaml"), filepath.Join(repoRoot, "deploy/.env"))
	if err != nil {
		return err
	}
	if err := cfg.RequireSecrets("mysql.password"); err != nil {
		return err
	}
	name := "agri_eval_it"
	if ns := os.Getenv(env.NamespaceEnv); ns != "" {
		name = ns + "_" + name
	}
	ctx := context.Background()
	if err := repository.CreateDatabase(ctx, cfg.MySQL.DSN("", false), name); err != nil {
		return err
	}
	if _, err := repository.Migrate(cfg.MySQL.DSN(name, true), filepath.Join(repoRoot, "migrations", "eval")); err != nil {
		return err
	}
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		return err
	}
	db, err := app.OpenMySQL(ctx, cfg, name, clock.System{}, logger)
	if err != nil {
		return err
	}
	alterErr := db.Exec(fmt.Sprintf("ALTER TABLE experiment AUTO_INCREMENT = %d", itAutoIncrement)).Error
	if err := errors.Join(alterErr, repository.Close(db)); err != nil {
		return fmt.Errorf("auto_increment: %w", err)
	}
	return os.Setenv(evalDBEnv, name)
}

// storeArgs 是 baseline / compare / aa 的连接参数（TestMain 已把评测库切到隔离库）。
func storeArgs() []string {
	return []string{"--config", filepath.Join(repoRoot, "configs/app.yaml"), "--env-file", filepath.Join(repoRoot, "deploy/.env")}
}

func sampleArgs() []string {
	return []string{"--suites-dir", filepath.Join(repoRoot, defaultDir), "--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot}
}

// runExp 跑一次 run 并返回 experiment.id。
func runExp(t *testing.T, suite string, n int, tags, trigger string) uint64 {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append(runArgs(t), "--suite", suite, "--n", strconv.Itoa(n), "--tags", tags, "--trigger", trigger)
	// P2 L7-a 评审修复：CI 触发（pr / nightly / release）只能经 --mode 得到，不带 --mode 时 run 拒绝。
	if code := run(args, &stdout, &stderr); code != exitOK {
		t.Fatalf("run code = %d stderr=%s", code, stderr.String())
	}
	return expIDOf(t, stdout.String())
}

// runArgs 是 run 子命令连隔离库与仓库配置的公共参数（不含 suite / n / tags / trigger）。
func runArgs(t *testing.T) []string {
	t.Helper()
	args := append([]string{"run"}, sampleArgs()...)
	args = append(args, storeArgs()...)
	return append(args, "--out", t.TempDir(),
		"--migrations", filepath.Join(repoRoot, "migrations"), "--seed", filepath.Join(repoRoot, "configs/seed/fields.yaml"),
		"--model", filepath.Join(repoRoot, "configs/model.yaml"), "--prompts", filepath.Join(repoRoot, "prompts"),
		"--risk", filepath.Join(repoRoot, "policy/risk_matrix.yaml"), "--permissions", filepath.Join(repoRoot, "policy/permission_matrix.yaml"),
		"--devices", filepath.Join(repoRoot, "configs/mock/device.yaml"),
		"--regulation", filepath.Join(repoRoot, "policy/regulation_map.yaml"),
		"--fault-matrix", filepath.Join(repoRoot, "policy/fault_matrix.yaml"), "--texts", filepath.Join(repoRoot, "configs/texts/zh-CN.yaml"),
		"--notify-texts", filepath.Join(repoRoot, "configs/texts/notify.zh-CN.yaml"),
		"--tools", filepath.Join(repoRoot, "configs/tools.yaml"))
}

// expIDOf 从 run / resume 的 stdout 取 exp_id。
func expIDOf(t *testing.T, stdout string) uint64 {
	t.Helper()
	m := regexp.MustCompile(`exp_id=(\d+) `).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout = %s", stdout)
	}
	id, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// evalDB 打开隔离评测库。
func evalDB(t *testing.T) (*repository.DB, *config.Config) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(repoRoot, "configs/app.yaml"), filepath.Join(repoRoot, "deploy/.env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MySQL.EvalDatabase != os.Getenv(evalDBEnv) {
		t.Fatalf("eval database = %q, want isolated %q", cfg.MySQL.EvalDatabase, os.Getenv(evalDBEnv))
	}
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		t.Fatal(err)
	}
	db, err := app.OpenMySQL(context.Background(), cfg, cfg.MySQL.EvalDatabase, clock.System{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(db); err != nil {
			t.Error(err)
		}
	})
	return db, cfg
}

func cli(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// l0Tags 是 L0 集成测试回放的小子集（与 TestRun_WritesExperiment_Integration 同一 tag）。
const (
	l0Tags   = "irrigate.amount"
	l0Trials = 2
)

// fakeP1 复制 src 实验为一条「P1 实验」：snapshot 为 NULL、trial.sample_version 为 0（迁移默认值），返回新 id。
func fakeP1(t *testing.T, db *repository.DB, src uint64) uint64 {
	t.Helper()
	var id uint64
	err := db.Transaction(func(tx *repository.DB) error {
		if err := tx.Exec(`INSERT INTO experiment (suite, trigger_kind, git_sha, status, started_at, finished_at)
  SELECT suite, 'manual', git_sha, status, started_at, finished_at FROM experiment WHERE id = ?`, src).Error; err != nil {
			return err
		}
		if err := tx.Raw("SELECT LAST_INSERT_ID()").Scan(&id).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO trial (experiment_id, sample_id, trial_index, suite, run_id, status, passed, scores, latency_ms,
  created_at, sample_version) SELECT ?, sample_id, trial_index, suite, run_id, status, passed, scores, latency_ms, created_at, 0
  FROM trial WHERE experiment_id = ?`, id, src).Error
	})
	if err != nil || id == 0 {
		t.Fatalf("fake P1 experiment: id=%d err=%v", id, err)
	}
	return id
}

// writeP1Yaml 写 P1 形态的 baseline yaml（快照只有 P1 字段），作为 --backfill 来源。
func writeP1Yaml(t *testing.T, dir string, id uint64, rec *store.Record) string {
	t.Helper()
	s := rec.Snapshot
	p1 := report.Baseline{Name: "it_p1_exit", ExpID: store.FormatID(id), Model: s.Model, Suites: []string{"regression"},
		Tags: []string{l0Tags}, N: l0Trials}
	p1.Snapshot.GitSHA, p1.Snapshot.GitDirty, p1.Snapshot.PromptVersion = s.GitSHA, s.GitDirty, s.PromptVersion
	p1.Snapshot.ModelTier, p1.Snapshot.Model, p1.Snapshot.KnowledgeRef, p1.Snapshot.SamplesHash = s.ModelTier, s.Model, s.KnowledgeRef, s.SamplesHash
	raw, err := yaml.Marshal(p1)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "it_p1_exit.yaml")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func countRows(t *testing.T, db *repository.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(query, args...).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// TestL0_RunFreezeCompare_Integration 走真库：run（快照、trigger、sample_version、manifest）→ freeze（P1 补录同事务）→
// 冻结后 UPDATE 被拒 → compare 单因子（写 factor_diff）/ 多因子 NOT_ATTRIBUTABLE / phase 不写门禁字段 / knowledge_ref 拒绝 → aa。
func TestL0_RunFreezeCompare_Integration(t *testing.T) {
	ctx := context.Background()
	db, cfg := evalDB(t)
	exps := store.New(db)
	cand := runExp(t, "regression", l0Trials, l0Tags, store.TriggerManual)

	// run：快照无空字段、trigger_kind、trial.sample_version、MinIO manifest。
	rec, err := exps.Get(ctx, cand)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Snapshot == nil || len(rec.Snapshot.Missing()) != 0 || rec.TriggerKind != store.TriggerManual || rec.Frozen {
		t.Fatalf("candidate record = %+v snapshot = %+v", rec, rec.Snapshot)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ? AND sample_version = 0", cand); n != 0 {
		t.Fatalf("%d trials without sample_version", n)
	}
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := repository.MinIOGetter{Client: mc}.GetObject(ctx, cfg.MinIO.BucketEval, store.ManifestKey(cand))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), rec.Snapshot.SamplesHash) || !strings.Contains(string(manifest), "@") {
		t.Fatalf("manifest = %s", manifest)
	}

	// freeze --backfill：P1 实验补录快照与 sample_version，同事务置 frozen = 1，写指针文件。
	p1 := fakeP1(t, db, cand)
	baselines := t.TempDir()
	args := append([]string{"baseline", "freeze", "--name", "it_p1", "--exp", store.FormatID(p1),
		"--backfill", writeP1Yaml(t, baselines, p1, rec), "--baselines-dir", baselines}, sampleArgs()...)
	if out, errOut, code := cli(t, append(args, storeArgs()...)...); code != exitOK || !strings.Contains(out, "backfilled=true") {
		t.Fatalf("freeze code = %d out=%s stderr=%s", code, out, errOut)
	}
	frozen, err := exps.Get(ctx, p1)
	if err != nil {
		t.Fatal(err)
	}
	if !frozen.Frozen || frozen.Snapshot == nil || len(frozen.Snapshot.Missing()) != 0 || frozen.Snapshot.LoaderStrategy != "v0" {
		t.Fatalf("frozen record = %+v snapshot = %+v", frozen, frozen.Snapshot)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM trial WHERE experiment_id = ? AND sample_version = 0", p1); n != 0 {
		t.Fatalf("%d trials not backfilled", n)
	}
	if id, err := resolveBaseline(ctx, exps, baselines, "it_p1"); err != nil || id != p1 {
		t.Fatalf("pointer resolves to %d %v, want %d", id, err, p1)
	}
	// 冻结 knowledge_ref 是 git_sha 上 knowledge/ 的 kc: 内容哈希。
	if !strings.HasPrefix(frozen.Snapshot.KnowledgeRef, "kc:") {
		t.Fatalf("backfilled knowledge_ref = %q", frozen.Snapshot.KnowledgeRef)
	}
	l0PointerOnly(t, args, baselines, p1)

	// 冻结后：store 层任何 experiment UPDATE 返回 ErrFrozen；重复冻结失败。
	if err := exps.SetReportKey(ctx, p1, "report/x/report.md"); !errors.Is(err, store.ErrFrozen) {
		t.Fatalf("SetReportKey after freeze: %v", err)
	}
	if err := exps.RecordCompare(ctx, p1, cand, store.CompareFactor, []byte("[]")); !errors.Is(err, store.ErrFrozen) {
		t.Fatalf("RecordCompare after freeze: %v", err)
	}
	if _, err := exps.Freeze(ctx, p1, nil); !errors.Is(err, store.ErrFrozen) {
		t.Fatalf("refreeze: %v", err)
	}

	l0Compare(t, db, exps, baselines, p1, cand)
}

// l0Compare 是 TestL0_RunFreezeCompare_Integration 的 compare / aa 部分。
func l0Compare(t *testing.T, db *repository.DB, exps *store.Experiments, baselines string, base, cand uint64) {
	t.Helper()
	ctx := context.Background()
	compareArgs := func(candidate uint64, extra ...string) []string {
		a := append([]string{"compare", "--baseline", "it_p1", "--candidate", store.FormatID(candidate),
			"--baselines-dir", baselines, "--out", t.TempDir(), "--repo", repoRoot}, storeArgs()...)
		return append(a, extra...)
	}
	// 同一配置：单因子、可归因、可放行，factor_diff = []、baseline_id / compare_mode 写入候选。
	out, errOut, code := cli(t, compareArgs(cand)...)
	if code != exitOK || !strings.Contains(out, "conclusion=NO_SIGNIFICANT_DIFF releasable=true") || strings.Contains(out, "paired=0 ") {
		t.Fatalf("factor compare code = %d out=%s stderr=%s", code, out, errOut)
	}
	var row struct {
		BaselineID  uint64 `gorm:"column:baseline_id"`
		CompareMode string `gorm:"column:compare_mode"`
		FactorDiff  string `gorm:"column:factor_diff"`
	}
	if err := db.Raw("SELECT baseline_id, compare_mode, factor_diff FROM experiment WHERE id = ?", cand).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.BaselineID != base || row.CompareMode != store.CompareFactor || row.FactorDiff != "[]" {
		t.Fatalf("candidate compare columns = %+v", row)
	}

	// A/A：同一快照的两次实验（--from-exps），六个指标都列、signed_by 留空。
	other := runExp(t, "regression", l0Trials, l0Tags, store.TriggerManual)
	thPath := filepath.Join(t.TempDir(), "thresholds.yaml")
	aaArgs := append([]string{"aa", "--from-exps", store.FormatID(cand) + "," + store.FormatID(other), "--out", thPath}, storeArgs()...)
	if out, errOut, code := cli(t, aaArgs...); code != exitOK {
		t.Fatalf("aa code = %d out=%s stderr=%s", code, out, errOut)
	}
	th, err := os.ReadFile(thPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"signed_by: \"\"", "citation_coverage", "numeric_accuracy", "refusal_accuracy",
		"over_conservative_rate", "fact_consistency", "hallucination_rate", "A/A 差值为 0"} {
		if !strings.Contains(string(th), want) {
			t.Fatalf("thresholds missing %q:\n%s", want, th)
		}
	}

	// 多因子（提示词 + 加载策略）：NOT_ATTRIBUTABLE、不可放行、报告首行标红。
	if err := db.Exec(`UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.prompt_version', 'v2', '$.loader_strategy', 'v1')
  WHERE id = ?`, other).Error; err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	a := append([]string{"compare", "--baseline", "it_p1", "--candidate", store.FormatID(other), "--baselines-dir", baselines,
		"--out", outDir, "--repo", repoRoot}, storeArgs()...)
	if out, errOut, code := cli(t, a...); code != exitOK || !strings.Contains(out, "conclusion=NOT_ATTRIBUTABLE releasable=false") {
		t.Fatalf("multi-factor compare code = %d out=%s stderr=%s", code, out, errOut)
	}
	rep, err := os.ReadFile(filepath.Join(outDir, "compare", fmt.Sprintf("%d-%d.md", base, other)))
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(string(rep), "\n"); !strings.Contains(first, "多因子对比，结论不可归因") {
		t.Fatalf("report first line = %q", first)
	}

	// phase：允许多因子，只出整体结论，不写门禁字段（先清掉上一步写入的 factor 结果再验证）。
	if err := db.Exec("UPDATE experiment SET baseline_id = NULL, compare_mode = NULL, factor_diff = NULL WHERE id = ?", other).Error; err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := cli(t, compareArgs(other, "--mode", "phase")...); code != exitOK || !strings.Contains(out, "conclusion=- releasable=false") {
		t.Fatalf("phase compare code = %d out=%s stderr=%s", code, out, errOut)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM experiment WHERE id = ? AND baseline_id IS NULL AND compare_mode IS NULL AND factor_diff IS NULL",
		other); n != 1 {
		t.Fatal("phase compare must not write gate fields")
	}

	// knowledge_ref 不是 kc: 格式（旧实验的 WORKTREE 等）：快照格式校验拒绝。
	if err := db.Exec(`UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.knowledge_ref', 'WORKTREE') WHERE id = ?`, other).Error; err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := cli(t, compareArgs(other)...); code != exitFailed || !strings.Contains(errOut, "malformed knowledge_ref") {
		t.Fatalf("malformed knowledge_ref code = %d stderr=%s", code, errOut)
	}
	// knowledge_ref 不一致（格式合法的另一个 kc: 哈希）：单因子模式拒绝（退出码非 0，提示重建 baseline）。
	otherRef := evalkit.KnowledgeContentPrefix + strings.Repeat("f", 64)
	if err := db.Exec(`UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.knowledge_ref', ?) WHERE id = ?`, otherRef, other).Error; err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := cli(t, compareArgs(other)...); code != exitFailed || !strings.Contains(errOut, "rebuild the baseline") {
		t.Fatalf("knowledge_ref mismatch code = %d stderr=%s", code, errOut)
	}
	if rec, err := exps.Get(ctx, base); err != nil || !rec.Frozen {
		t.Fatalf("baseline must stay frozen: %+v %v", rec, err)
	}
}

// l0PointerOnly：冻结已提交但指针没写成（删掉指针模拟）→ 同一补录重跑只补写指针（pointer-only）；指针已在则拒绝；
// 补录内容变了（n 不同）则 ErrFrozenMismatch、不写指针。
func l0PointerOnly(t *testing.T, freezeArgs []string, baselines string, p1 uint64) {
	t.Helper()
	pointer := filepath.Join(baselines, "it_p1.yaml")
	if _, errOut, code := cli(t, append(freezeArgs, storeArgs()...)...); code != exitFailed || !strings.Contains(errOut, errPointerExists.Error()) {
		t.Fatalf("refreeze with pointer: code = %d stderr=%s", code, errOut)
	}
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := cli(t, append(freezeArgs, storeArgs()...)...)
	if code != exitOK || !strings.Contains(out, "pointer-only") {
		t.Fatalf("pointer-only code = %d out=%s stderr=%s", code, out, errOut)
	}
	pb, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatalf("pointer not rewritten: %v", err)
	}
	// pointer-only 的 frozen_at 取 finished_at，notes 写明。
	if !strings.Contains(string(pb), pointerOnlyFrozenAtNote) {
		t.Fatalf("pointer-only pointer lacks frozen_at note:\n%s", pb)
	}
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	// 换个名字给已冻结实验补指针：与登记名不同，拒绝，不写新指针。
	renamed := slices.Clone(freezeArgs)
	renamed[slices.Index(renamed, "--name")+1] = "it_p1.r2"
	if _, errOut, code := cli(t, append(renamed, storeArgs()...)...); code != exitFailed || !strings.Contains(errOut, store.ErrBaselineName.Error()) {
		t.Fatalf("pointer-only rename: code = %d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(baselines, "it_p1.r2.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename must not write pointer: %v", err)
	}
	// 改 n 的补录 yaml：快照不一致，拒绝只补写指针。
	idx := slices.Index(freezeArgs, "--backfill")
	raw, err := os.ReadFile(freezeArgs[idx+1])
	if err != nil {
		t.Fatal(err)
	}
	var b report.Baseline
	if err := yaml.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	b.N++
	changed, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "changed.yaml")
	if err := os.WriteFile(other, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	mismatch := slices.Clone(freezeArgs)
	mismatch[idx+1] = other
	if _, errOut, code := cli(t, append(mismatch, storeArgs()...)...); code != exitFailed || !strings.Contains(errOut, store.ErrFrozenMismatch.Error()) {
		t.Fatalf("pointer-only mismatch: code = %d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(pointer); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatch must not write pointer: %v", err)
	}
	// 恢复指针供后续 compare 使用。
	if out, errOut, code := cli(t, append(freezeArgs, storeArgs()...)...); code != exitOK || !strings.Contains(out, "pointer-only") {
		t.Fatalf("restore pointer: code = %d out=%s stderr=%s", code, out, errOut)
	}
}

// copyExp 复制 src 实验行（含快照）为新实验，status / finished_at 按参数；withTrials 时连 trial 一起复制。返回新 id。
func copyExp(t *testing.T, db *repository.DB, src uint64, status string, finished, withTrials bool) uint64 {
	t.Helper()
	fin := "NULL"
	if finished {
		fin = "finished_at"
	}
	var id uint64
	err := db.Transaction(func(tx *repository.DB) error {
		if err := tx.Exec(`INSERT INTO experiment (suite, trigger_kind, git_sha, status, started_at, finished_at, snapshot)
  SELECT suite, trigger_kind, git_sha, ?, started_at, `+fin+`, snapshot FROM experiment WHERE id = ?`, status, src).Error; err != nil {
			return err
		}
		if err := tx.Raw("SELECT LAST_INSERT_ID()").Scan(&id).Error; err != nil {
			return err
		}
		if !withTrials {
			return nil
		}
		return tx.Exec(`INSERT INTO trial (experiment_id, sample_id, trial_index, suite, run_id, status, passed, scores, latency_ms,
  created_at, sample_version) SELECT ?, sample_id, trial_index, suite, run_id, status, passed, scores, latency_ms, created_at, sample_version
  FROM trial WHERE experiment_id = ?`, id, src).Error
	})
	if err != nil || id == 0 {
		t.Fatalf("copy experiment: id=%d err=%v", id, err)
	}
	return id
}

// TestL0_FailClosed_Integration 走真库验证失败关闭：空实验 / RUNNING 实验不能冻结；RUNNING 置 FAILED 走 store 守卫更新；
// 未冻结的实验不能当 --baseline；RUNNING 候选被 compare 拒绝；--fail-on-unreleasable 在不可放行时退出码 1。
func TestL0_FailClosed_Integration(t *testing.T) {
	ctx := context.Background()
	db, _ := evalDB(t)
	exps := store.New(db)
	base := runExp(t, "regression", l0Trials, l0Tags, store.TriggerManual)
	cand := runExp(t, "regression", l0Trials, l0Tags, store.TriggerManual)
	baselines := t.TempDir()

	empty := copyExp(t, db, base, "DONE", true, false)
	if _, err := exps.Freeze(ctx, empty, nil); !errors.Is(err, store.ErrNoTrials) {
		t.Fatalf("freeze 0-trial experiment: %v", err)
	}
	running := copyExp(t, db, base, "RUNNING", false, true)
	if _, err := exps.Freeze(ctx, running, nil); !errors.Is(err, store.ErrNotDone) {
		t.Fatalf("freeze RUNNING experiment: %v", err)
	}

	// 未冻结的实验不能当基线（数字 id 与指针名都查库）。
	compareArgs := func(baseline string, candidate uint64, extra ...string) []string {
		a := append([]string{"compare", "--baseline", baseline, "--candidate", store.FormatID(candidate),
			"--baselines-dir", baselines, "--out", t.TempDir(), "--repo", repoRoot}, storeArgs()...)
		return append(a, extra...)
	}
	if _, errOut, code := cli(t, compareArgs(store.FormatID(base), cand)...); code != exitFailed || !strings.Contains(errOut, errBaselineNotFrozen.Error()) {
		t.Fatalf("unfrozen baseline: code = %d stderr=%s", code, errOut)
	}
	freeze := append([]string{"baseline", "freeze", "--name", "it_base", "--exp", store.FormatID(base), "--baselines-dir", baselines},
		storeArgs()...)
	if out, errOut, code := cli(t, freeze...); code != exitOK || !strings.Contains(out, "backfilled=false") {
		t.Fatalf("freeze code = %d out=%s stderr=%s", code, out, errOut)
	}

	// RUNNING 候选：compare 拒绝（不出结论）。
	if _, errOut, code := cli(t, compareArgs("it_base", running)...); code != exitFailed || !strings.Contains(errOut, "not DONE") {
		t.Fatalf("RUNNING candidate: code = %d stderr=%s", code, errOut)
	}
	// RUNNING → FAILED 走 store.Fail；已结束的实验不能再置 FAILED，冻结的更不行。
	if err := exps.Fail(ctx, running, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec, err := exps.Get(ctx, running); err != nil || rec.Status != "FAILED" || rec.FinishedAt == nil {
		t.Fatalf("failed record = %+v %v", rec, err)
	}
	if err := exps.Fail(ctx, running, time.Now()); !errors.Is(err, store.ErrNotRunning) {
		t.Fatalf("fail twice: %v", err)
	}
	if err := exps.Fail(ctx, base, time.Now()); !errors.Is(err, store.ErrNotRunning) {
		t.Fatalf("fail DONE/frozen: %v", err)
	}

	// --fail-on-unreleasable：同配置可放行 → 0；多因子不可放行 → 1（报告照写）。
	if out, errOut, code := cli(t, compareArgs("it_base", cand, "--fail-on-unreleasable")...); code != exitOK ||
		!strings.Contains(out, "releasable=true") {
		t.Fatalf("releasable compare: code = %d out=%s stderr=%s", code, out, errOut)
	}
	if err := db.Exec(`UPDATE experiment SET snapshot = JSON_SET(snapshot, '$.prompt_version', 'v2', '$.loader_strategy', 'v1'),
  baseline_id = NULL, compare_mode = NULL, factor_diff = NULL WHERE id = ?`, cand).Error; err != nil {
		t.Fatal(err)
	}
	out, errOut, code := cli(t, compareArgs("it_base", cand, "--fail-on-unreleasable")...)
	if code != exitFailed || !strings.Contains(out, "releasable=false") || !strings.Contains(errOut, errUnreleasable.Error()) {
		t.Fatalf("unreleasable compare: code = %d out=%s stderr=%s", code, out, errOut)
	}
	if out, errOut, code := cli(t, compareArgs("it_base", cand)...); code != exitOK || !strings.Contains(out, "releasable=false") {
		t.Fatalf("default compare must exit 0: code = %d out=%s stderr=%s", code, out, errOut)
	}
}
