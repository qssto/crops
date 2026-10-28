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
knowledge_ref: kb@TBD
expect: {outcome: REFUSED, refuse_reason: KB_MISS}
review_status: pending_expert
signed_at: 2026-10-28
`
	if err := os.WriteFile(filepath.Join(badDir, "tmp-no-author.yaml"), []byte(noAuthor), 0o644); err != nil {
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
		{name: "validate repo samples", wantCode: exitOK, wantStdout: "validate OK: 20 samples",
			args: []string{"validate", "--suites-dir", suites, "--taxonomy", tax}},
		{name: "validate rejects missing author", wantCode: exitFailed, wantStderr: "failed sample ids: tmp-no-author",
			args: []string{"validate", "--suites-dir", filepath.Dir(badDir), "--taxonomy", tax}},
		{name: "run with stub agent", wantCode: exitOK, wantStdout: "samples=2 trials=4",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--out", outDir,
				"--suite", "regression", "--n", "2", "--tags", "L5,irrigate.amount"}},
		{name: "run no matching tag", wantCode: exitFailed, wantStderr: "no sample matches",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--out", outDir, "--tags", "nope"}},
		{name: "run n=0", wantCode: exitFailed, wantStderr: "n must be >= 1",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--out", outDir, "--n", "0"}},
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
