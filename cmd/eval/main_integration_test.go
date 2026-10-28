//go:build integration

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
)

// TestRun_WritesExperiment_Integration 真实连 docker MySQL：run 写 agri_eval.experiment / trial。
func TestRun_WritesExperiment_Integration(t *testing.T) {
	cfgFile := filepath.Join(repoRoot, "configs/app.yaml")
	envFile := filepath.Join(repoRoot, "deploy/.env")
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--taxonomy", filepath.Join(repoRoot, defaultTax), "--out", t.TempDir(),
		"--config", cfgFile, "--env-file", envFile,
		"--migrations", filepath.Join(repoRoot, "migrations"), "--seed", filepath.Join(repoRoot, "configs/seed/fields.yaml"),
		"--suite", "regression", "--n", "2", "--tags", "L5,irrigate.amount"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	m := regexp.MustCompile(`exp_id=(\d+) samples=2 trials=4`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("stdout = %s", stdout.String())
	}

	cfg, err := config.Load(cfgFile, envFile)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := app.NewLogger(&stderr, "warn")
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
	var done, trials int64
	if err := db.Raw("SELECT COUNT(*) FROM experiment WHERE id = ? AND status = 'DONE' AND finished_at IS NOT NULL",
		m[1]).Scan(&done).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT COUNT(*) FROM trial WHERE experiment_id = ? AND status IN ('PASS', 'FAIL')",
		m[1]).Scan(&trials).Error; err != nil {
		t.Fatal(err)
	}
	if done != 1 || trials != 4 {
		t.Fatalf("experiment DONE=%d trials=%d, want 1 and 4", done, trials)
	}
}
