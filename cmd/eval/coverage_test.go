package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// P2 L1-T：validate 输出覆盖报表与补写清单；覆盖不足默认不改退出码，--strict-coverage 时失败。
func TestValidate_Coverage(t *testing.T) {
	base := []string{"validate", "--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot}
	tests := []struct {
		name       string
		extra      []string
		wantCode   int
		wantStdout []string
		wantStderr string
	}{
		{name: "report only", wantCode: exitOK, wantStdout: []string{
			"coverage: ", "/ 500 samples", "user_sim: 0", "holdout: absent (skipped)",
			"refuse: ", "KB_MISS", "DATA_MISSING", "OUT_OF_SCOPE", "trap: ", "trap.similar_pest", "补写清单: ",
			"by category × stage × difficulty:",
		}},
		{name: "strict fails", extra: []string{"--strict-coverage"}, wantCode: exitFailed,
			wantStdout: []string{"补写清单: "}, wantStderr: "coverage below target: "},
		{name: "never color", extra: []string{"--color", "never"}, wantCode: exitOK, wantStdout: []string{"补写清单: "}},
		{name: "always color", extra: []string{"--color", "always"}, wantCode: exitOK, wantStdout: []string{"\x1b[31m"}},
		{name: "bad color", extra: []string{"--color", "rainbow"}, wantCode: exitFailed, wantStderr: "--color"},
		{name: "missing targets", extra: []string{"--coverage-targets", filepath.Join(t.TempDir(), "none.yaml")},
			wantCode: exitFailed, wantStderr: "none.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(append(append([]string{}, base...), tt.extra...), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%s", code, tt.wantCode, stderr.String())
			}
			for _, w := range tt.wantStdout {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("stdout missing %q:\n%s", w, stdout.String())
				}
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), tt.wantStderr)
			}
			if tt.name == "never color" && strings.Contains(stdout.String(), "\x1b[") {
				t.Error("--color never must not emit ANSI")
			}
		})
	}
}

// P2 L1-T：run --suite capability 选得到样本（库不可达时停在装配评测设施，而非「no sample matches」）。
func TestRun_SelectsCapabilitySuite(t *testing.T) {
	t.Setenv("AS_MYSQL_PORT", "1")
	for _, k := range []string{"AS_MYSQL_PASSWORD", "AS_REDIS_PASSWORD", "AS_MINIO_ACCESS_KEY", "AS_MINIO_SECRET_KEY"} {
		t.Setenv(k, "unit-test-dummy")
	}
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--suites-dir", filepath.Join(repoRoot, defaultDir), "--taxonomy", filepath.Join(repoRoot, defaultTax),
		"--repo", repoRoot, "--out", outDir, "--config", filepath.Join(repoRoot, "configs/app.yaml"),
		"--env-file", filepath.Join(outDir, "none.env"), "--suite", "capability", "--n", "1"}, &stdout, &stderr)
	if code != exitFailed || !strings.Contains(stderr.String(), "eval infra unavailable") {
		t.Fatalf("code = %d stderr = %s, want infra error after selecting capability samples", code, stderr.String())
	}
}
