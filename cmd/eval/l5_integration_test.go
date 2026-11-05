//go:build integration

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/report"
)

// P2 L5：开关开时报告带知识加载指标节（策略 v1、植保行），并在报告同目录写 L8 清单 kb_load_gaps.json。
func TestRun_LoaderV1Report_Integration(t *testing.T) {
	args := append(runArgs(t), "--suite", "regression", "--n", "1", "--tags", "protect.dosage",
		"--flags", "enable_loader_v1=on")
	stdout, stderr, code := cli(t, args...)
	if code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	m := regexp.MustCompile(`report=(\S+)`).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout=%s", stdout)
	}
	raw, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{"## 知识加载指标", "策略：v1", "| protect | ", "| 合计 | "} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m[1]), report.LoadGapsFile)); err != nil {
		t.Errorf("gaps file: %v", err)
	}
}
