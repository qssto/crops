//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

// gradEvalDB 是毕业集成测试专用的评测库（加 AS_TEST_NS 前缀）；不写共享 agri_eval。
const gradEvalDB = "agri_eval_grad_it"

// openGradDB 建库、迁移 migrations/eval、清空 experiment / trial，并让 cmd/eval 经 AS_MYSQL_EVAL_DATABASE 指向它。
func openGradDB(t *testing.T) *repository.DB {
	t.Helper()
	name := gradEvalDB
	if ns := os.Getenv(env.NamespaceEnv); ns != "" {
		name = ns + "_" + name
	}
	t.Setenv("AS_MYSQL_EVAL_DATABASE", name)
	ctx := context.Background()
	cfg, err := config.Load(filepath.Join(repoRoot, "configs/app.yaml"), filepath.Join(repoRoot, "deploy/.env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MySQL.EvalDatabase != name {
		t.Fatalf("eval database = %q, want %q", cfg.MySQL.EvalDatabase, name)
	}
	if err := repository.CreateDatabase(ctx, cfg.MySQL.DSN("", false), name); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Migrate(cfg.MySQL.DSN(name, true), filepath.Join(repoRoot, "migrations", "eval")); err != nil {
		t.Fatal(err)
	}
	logger, err := app.NewLogger(io.Discard, "warn")
	if err != nil {
		t.Fatal(err)
	}
	db, err := app.OpenMySQL(ctx, cfg, name, clock.System{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(db); err != nil {
			t.Error(err)
		}
	})
	for _, table := range []string{"trial", "experiment"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// insertExperiment 插一行实验与每个样本 store.GraduateTrials 个 trial（sample_id → 全过 / 全失败），返回实验 id。
func insertExperiment(t *testing.T, db *repository.DB, trigger, status string, trials map[string]bool, version int) uint64 {
	t.Helper()
	return insertExperimentN(t, db, trigger, status, trials, version, store.GraduateTrials)
}

// insertExperimentN 同 insertExperiment，每个样本插 n 个 trial（trial_index 0..n-1）。
func insertExperimentN(t *testing.T, db *repository.DB, trigger, status string, trials map[string]bool, version, n int) uint64 {
	t.Helper()
	now := time.Now()
	if err := db.Exec("INSERT INTO experiment (suite, trigger_kind, git_sha, status, started_at) VALUES ('capability', ?, ?, ?, ?)",
		trigger, strings.Repeat("a", 40), status, now).Error; err != nil {
		t.Fatal(err)
	}
	var id uint64
	if err := db.Raw("SELECT MAX(id) FROM experiment").Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	for sid, passed := range trials {
		for i := range n {
			if err := db.Exec(`INSERT INTO trial (experiment_id, sample_id, trial_index, suite, run_id, status, passed, scores, latency_ms, created_at, sample_version)
				VALUES (?, ?, ?, 'capability', NULL, ?, ?, '[]', 1, ?, ?)`,
				id, sid, i, map[bool]string{true: "PASS", false: "FAIL"}[passed], passed, now, version).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return id
}

// TestGraduate_Integration（P2 L1-T）：最近 3 次 nightly DONE 里 rice-cap-0001 全过 → 可毕业；rice-cap-0002 有一次失败 → 不可；
// 更早的失败、manual 实验与 RUNNING 实验不计入；--apply 后文件移到 regression/。
func TestGraduate_Integration(t *testing.T) {
	db := openGradDB(t)
	insertExperiment(t, db, "nightly", "DONE", map[string]bool{"rice-cap-0001": false, "rice-cap-0002": false}, 1) // 窗口外
	insertExperiment(t, db, "nightly", "DONE", map[string]bool{"rice-cap-0001": true, "rice-cap-0002": true}, 1)
	insertExperiment(t, db, "nightly", "DONE", map[string]bool{"rice-cap-0001": true, "rice-cap-0002": false}, 1)
	insertExperiment(t, db, "manual", "DONE", map[string]bool{"rice-cap-0001": false}, 1)     // 非 nightly
	insertExperiment(t, db, "nightly", "RUNNING", map[string]bool{"rice-cap-0001": false}, 1) // 未完成
	insertExperiment(t, db, "nightly", "DONE", map[string]bool{"rice-cap-0001": true, "rice-cap-0002": true}, 1)

	root := writeCapabilitySuite(t)
	args := []string{"graduate", "--suites-dir", root, "--config", filepath.Join(repoRoot, "configs/app.yaml"),
		"--env-file", filepath.Join(repoRoot, "deploy/.env")}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitOK {
		t.Fatalf("dry run code=%d stderr=%s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "2 capability sample(s), 1 eligible") || !strings.Contains(out, "rice-cap-0001@1") ||
		strings.Contains(out, "rice-cap-0002@1") {
		t.Fatalf("dry run stdout = %s", out)
	}
	stdout.Reset()
	if code := run(append(args, "--apply"), &stdout, &stderr); code != exitOK {
		t.Fatalf("apply code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, suiteRegression, "rice-cap-0001.yaml")); err != nil {
		t.Fatalf("not moved: %v\n%s", err, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, suiteCapability, "topic", "rice-cap-0002.yaml")); err != nil {
		t.Fatalf("rice-cap-0002 must stay: %v", err)
	}
}

// 版本区分：trial 记在旧版本上时新版本样本不毕业。
func TestGraduate_VersionMismatch_Integration(t *testing.T) {
	db := openGradDB(t)
	for range 3 {
		insertExperiment(t, db, "nightly", "DONE", map[string]bool{"rice-cap-0001": true}, 0)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"graduate", "--suites-dir", writeCapabilitySuite(t), "--config", filepath.Join(repoRoot, "configs/app.yaml"),
		"--env-file", filepath.Join(repoRoot, "deploy/.env")}, &stdout, &stderr)
	if code != exitOK || !strings.Contains(stdout.String(), "0 eligible") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

// 应失败：3 次 nightly 全过但每次只跑 1 个 trial（--n 1）不算 pass^5，不毕业。
func TestGraduate_TooFewTrials_Integration(t *testing.T) {
	db := openGradDB(t)
	for range 3 {
		insertExperimentN(t, db, "nightly", "DONE", map[string]bool{"rice-cap-0001": true}, 1, 1)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"graduate", "--suites-dir", writeCapabilitySuite(t), "--config", filepath.Join(repoRoot, "configs/app.yaml"),
		"--env-file", filepath.Join(repoRoot, "deploy/.env")}, &stdout, &stderr)
	if code != exitOK || !strings.Contains(stdout.String(), "0 eligible") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

// TestRun_CapabilitySuite_Integration（P2 L1-T）：run --suite capability 连库跑通并出报告（写 TestMain 建的隔离评测库，
// manifest 写 MinIO，对象键取隔离库的实验 id）。
func TestRun_CapabilitySuite_Integration(t *testing.T) {
	rep := runReport(t, suiteCapability, 1, "")
	if !strings.Contains(rep, "capability") {
		t.Errorf("report does not mention capability suite:\n%s", rep)
	}
}
