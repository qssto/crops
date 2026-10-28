package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../.."

func TestRun_ExitCodes(t *testing.T) {
	// 指向不可达端口：run 必须明确报错退出，不得静默跳过 DB
	t.Setenv("AS_MYSQL_PORT", "1")
	for _, k := range []string{"AS_MYSQL_PASSWORD", "AS_REDIS_PASSWORD", "AS_MINIO_ACCESS_KEY", "AS_MINIO_SECRET_KEY"} {
		t.Setenv(k, "unit-test-dummy")
	}
	badDir := filepath.Join(t.TempDir(), "regression")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	noAuthor := `id: tmp-no-author
version: 1
suite: regression
tags: [L4, protect.timing]
question: 要不要打药？
context: {farm: farm-demo-01, field: field-001, role: farm_manager, user: u-1}
fixtures: {clock: "2026-07-15T09:00:00+08:00"}
knowledge_ref: WORKTREE
expect: {outcome: REFUSED, refuse_reason: KB_MISS}
review_status: pending_expert
signed_at: 2026-10-28
`
	if err := os.WriteFile(filepath.Join(badDir, "tmp-no-author.yaml"), []byte(noAuthor), 0o644); err != nil {
		t.Fatal(err)
	}
	// 参考解自相矛盾：must_call 与 must_not_call 冲突 → reference_passes_graders 违规
	refDir := filepath.Join(t.TempDir(), "regression")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contra := strings.Replace(noAuthor, "tmp-no-author", "tmp-contra", 1) + "author: tester\n"
	contra = strings.Replace(contra, "expect: {outcome: REFUSED, refuse_reason: KB_MISS}",
		"expect: {outcome: REFUSED, refuse_reason: KB_MISS, trajectory: {must_call: [device.list], must_not_call: [device.*]}}", 1)
	if err := os.WriteFile(filepath.Join(refDir, "tmp-contra.yaml"), []byte(contra), 0o644); err != nil {
		t.Fatal(err)
	}
	tax := filepath.Join(repoRoot, defaultTax)
	suites := filepath.Join(repoRoot, defaultDir)
	outDir := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: exitUsage},
		{name: "unknown subcommand", args: []string{"foo"}, wantCode: exitUsage},
		{name: "validate repo samples", wantCode: exitOK, wantStdout: "validate OK: 103 samples",
			args: []string{"validate", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot}},
		{name: "validate rejects missing author", wantCode: exitFailed, wantStderr: "failed sample ids: tmp-no-author",
			args: []string{"validate", "--suites-dir", filepath.Dir(badDir), "--taxonomy", tax, "--repo", repoRoot}},
		{name: "validate rejects failing reference solution", wantCode: exitFailed, wantStderr: "reference_passes_graders",
			args: []string{"validate", "--suites-dir", filepath.Dir(refDir), "--taxonomy", tax, "--repo", repoRoot}},
		{name: "run fails loudly when mysql down", wantCode: exitFailed, wantStderr: "eval infra unavailable",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir,
				"--config", filepath.Join(repoRoot, "configs/app.yaml"), "--env-file", filepath.Join(outDir, "none.env"),
				"--suite", "regression", "--n", "2", "--tags", "L5,irrigate.amount"}},
		{name: "run no matching tag", wantCode: exitFailed, wantStderr: "no sample matches",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir, "--tags", "nope"}},
		{name: "run n=0", wantCode: exitFailed, wantStderr: "n must be >= 1",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir, "--n", "0"}},
		{name: "kbdump regression", wantCode: exitOK, wantStdout: "kbdump: 103 samples",
			args: []string{"kbdump", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--suite", "regression"}},
		{name: "kbdump unknown suite", wantCode: exitFailed, wantStderr: "no sample matches",
			args: []string{"kbdump", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--suite", "nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stdout=%s stderr=%s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want containing %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
