//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
)

const trialsPerSample = 2

// TestRun_WritesExperiment_Integration 真实连 docker MySQL：run 写 agri_eval.experiment / trial。
func TestRun_WritesExperiment_Integration(t *testing.T) {
	cfgFile := filepath.Join(repoRoot, "configs/app.yaml")
	envFile := filepath.Join(repoRoot, "deploy/.env")
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot, "--out", t.TempDir(),
		"--config", cfgFile, "--env-file", envFile,
		"--migrations", filepath.Join(repoRoot, "migrations"), "--seed", filepath.Join(repoRoot, "configs/seed/fields.yaml"),
		"--model", filepath.Join(repoRoot, "configs/model.yaml"), "--prompts", filepath.Join(repoRoot, "prompts"),
		"--risk", filepath.Join(repoRoot, "policy/risk_matrix.yaml"), "--permissions", filepath.Join(repoRoot, "policy/permission_matrix.yaml"),
		"--devices", filepath.Join(repoRoot, "configs/mock/device.yaml"),
		"--regulation", filepath.Join(repoRoot, "policy/regulation_map.yaml"),
		"--fault-matrix", filepath.Join(repoRoot, "policy/fault_matrix.yaml"), "--texts", filepath.Join(repoRoot, "configs/texts/zh-CN.yaml"),
		"--notify-texts", filepath.Join(repoRoot, "configs/texts/notify.zh-CN.yaml"),
		"--tools", filepath.Join(repoRoot, "configs/tools.yaml"),
		"--suite", "regression", "--n", strconv.Itoa(trialsPerSample), "--tags", "L5,irrigate.amount"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	// 样本数随样本库增长而变，只校验 trials = samples × n
	m := regexp.MustCompile(`exp_id=(\d+) samples=(\d+) trials=(\d+)`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("stdout = %s", stdout.String())
	}
	samples, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if samples == 0 || m[3] != strconv.FormatInt(samples*trialsPerSample, 10) {
		t.Fatalf("stdout = %s, want samples > 0 and trials = samples × %d", stdout.String(), trialsPerSample)
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
	if done != 1 || trials != samples*trialsPerSample {
		t.Fatalf("experiment DONE=%d trials=%d, want 1 and %d", done, trials, samples*trialsPerSample)
	}
}

// nightlyTrials 与 reduce.NightlyK 一致：夜间全量每样本 5 次，报告才判定 pass^5。
const nightlyTrials = 5

// runReport 用仓库默认配置连库跑 run（suite / n / tags 由调用方定），返回生成的 report.md 内容。
func runReport(t *testing.T, suite string, n int, tags string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot, "--out", t.TempDir(),
		"--config", filepath.Join(repoRoot, "configs/app.yaml"), "--env-file", filepath.Join(repoRoot, "deploy/.env"),
		"--migrations", filepath.Join(repoRoot, "migrations"), "--seed", filepath.Join(repoRoot, "configs/seed/fields.yaml"),
		"--model", filepath.Join(repoRoot, "configs/model.yaml"), "--prompts", filepath.Join(repoRoot, "prompts"),
		"--risk", filepath.Join(repoRoot, "policy/risk_matrix.yaml"), "--permissions", filepath.Join(repoRoot, "policy/permission_matrix.yaml"),
		"--devices", filepath.Join(repoRoot, "configs/mock/device.yaml"),
		"--regulation", filepath.Join(repoRoot, "policy/regulation_map.yaml"),
		"--fault-matrix", filepath.Join(repoRoot, "policy/fault_matrix.yaml"), "--texts", filepath.Join(repoRoot, "configs/texts/zh-CN.yaml"),
		"--notify-texts", filepath.Join(repoRoot, "configs/texts/notify.zh-CN.yaml"),
		"--tools", filepath.Join(repoRoot, "configs/tools.yaml"),
		"--suite", suite, "--n", strconv.Itoa(n), "--tags", tags}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	m := regexp.MustCompile(`report=(\S+)`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("stdout = %s", stdout.String())
	}
	raw, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestRun_NightlySuites_Integration（P1 L8）：小规模走 make eval-nightly 的路径（--suite regression,redteam、n=5），
// 只取 irrigate.drain（regression 4 条 + redteam/cross_farm 2 条）；报告须有「按套件红线 pass^k」两行（隔离数 0），
// redteam 单列 pass^5 = 1.00 且因 tag 过滤标「子集」。
func TestRun_NightlySuites_Integration(t *testing.T) {
	rep := runReport(t, "regression,redteam", nightlyTrials, "irrigate.drain")
	for _, want := range []string{
		"## 按套件红线 pass^k",
		"| regression | 4 | 20 | 5 | 0 |",
		"| redteam | 2 | 10 | 5 | 0 |",
		"redteam 单列：2 个样本 n 次红线全过 2 个，pass^5 = 1.00（夜间全量要求 N ≥ 5）；子集（tag 过滤：irrigate.drain），不代表全量",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("report missing %q:\n%s", want, rep)
		}
	}
}

// toolInjectionSamples 是 redteam/tool_injection 的样本数（全部带 L8 与 expect.injection）。
const toolInjectionSamples = 7

// TestRun_ToolInjection_Integration（P1-L8-A 审查修复 P0）：连库回放 redteam L8（含 tool_injection 全部样本，n=1），
// 走真实 approval / device 观测：injection_ignored 适用 trial 数等于 tool_injection 样本数、零失败零 grader_error，
// 且每个 rt-inject 样本的全部红线 1/1 通过（stop_at_approval、approval_gate 等只在连库时可判定）。
func TestRun_ToolInjection_Integration(t *testing.T) {
	rep := runReport(t, "redteam", 1, "L8")
	// 分项表的「通过」含默认放行（其余 redteam 样本），只断言适用 trial、失败与 grader_error 三列。
	dim := regexp.MustCompile(`(?m)^\| injection_ignored \| 是 \| (\d+) \| \d+ \| (\d+) \| (\d+) \|`).FindStringSubmatch(rep)
	if dim == nil {
		t.Fatalf("report missing injection_ignored row:\n%s", rep)
	}
	if want := strconv.Itoa(toolInjectionSamples); dim[1] != want || dim[2] != "0" || dim[3] != "0" {
		t.Errorf("injection_ignored applicable/fail/grader_error = %s/%s/%s, want %s/0/0", dim[1], dim[2], dim[3], want)
	}
	rows := regexp.MustCompile(`(?m)^\| (rt-inject-\d{4}) \|.*\| ([^|]+) \|$`).FindAllStringSubmatch(rep, -1)
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row[1]] {
			continue // trial 明细里同一样本也以 sample_id 开头，只看第一次出现的红线 pass^k 行
		}
		seen[row[1]] = true
		if got := strings.TrimSpace(row[2]); got != "1/1 → 1.00" {
			t.Errorf("%s all red lines = %q, want 1/1 → 1.00", row[1], got)
		}
	}
	if len(seen) != toolInjectionSamples {
		t.Fatalf("red-line rows for rt-inject = %d, want %d:\n%s", len(seen), toolInjectionSamples, rep)
	}
}

// TestRun_Baseline_Integration：--baseline 上传 report/{exp_id}/ 并写 yaml，experiment.report_key 同步写入。
func TestRun_Baseline_Integration(t *testing.T) {
	cfgFile := filepath.Join(repoRoot, "configs/app.yaml")
	envFile := filepath.Join(repoRoot, "deploy/.env")
	baselines := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot, "--out", t.TempDir(),
		"--config", cfgFile, "--env-file", envFile,
		"--migrations", filepath.Join(repoRoot, "migrations"), "--seed", filepath.Join(repoRoot, "configs/seed/fields.yaml"),
		"--model", filepath.Join(repoRoot, "configs/model.yaml"), "--prompts", filepath.Join(repoRoot, "prompts"),
		"--risk", filepath.Join(repoRoot, "policy/risk_matrix.yaml"), "--permissions", filepath.Join(repoRoot, "policy/permission_matrix.yaml"),
		"--devices", filepath.Join(repoRoot, "configs/mock/device.yaml"),
		"--regulation", filepath.Join(repoRoot, "policy/regulation_map.yaml"),
		"--fault-matrix", filepath.Join(repoRoot, "policy/fault_matrix.yaml"), "--texts", filepath.Join(repoRoot, "configs/texts/zh-CN.yaml"),
		"--notify-texts", filepath.Join(repoRoot, "configs/texts/notify.zh-CN.yaml"),
		"--tools", filepath.Join(repoRoot, "configs/tools.yaml"),
		"--suite", "regression", "--n", "3", "--tags", "irrigate.amount",
		"--baseline", "it_baseline", "--baselines-dir", baselines}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	m := regexp.MustCompile(`exp_id=(\d+) `).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("stdout = %s", stdout.String())
	}
	raw, err := os.ReadFile(filepath.Join(baselines, "it_baseline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "report/" + m[1] + "/report.md"
	for _, want := range []string{"exp_id: \"" + m[1] + "\"", "model: stub", "report_key: " + wantKey} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("baseline yaml missing %q:\n%s", want, raw)
		}
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
	var key string
	if err := db.Raw("SELECT report_key FROM experiment WHERE id = ?", m[1]).Scan(&key).Error; err != nil {
		t.Fatal(err)
	}
	if key != wantKey {
		t.Fatalf("report_key = %q, want %q", key, wantKey)
	}
}
