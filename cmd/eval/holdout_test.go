package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mountedHoldout 把公开样本拷到临时目录，并在其 holdout/protect/ 下放由公开样本改写的两条合成 holdout 样本（模拟 submodule 挂载）。
func mountedHoldout(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "suites")
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(repoRoot, defaultDir))); err != nil {
		t.Fatal(err)
	}
	// 真实 submodule 已挂载时先移走，只用合成样本（单测不读真实 holdout 内容）。
	if err := os.RemoveAll(filepath.Join(dir, "holdout")); err != nil {
		t.Fatal(err)
	}
	// 一条建议、一条拒答（holdout 套件同样受拒答占比规则约束）。
	for i, src := range []string{"rice-p2-prot-0001", "rice-p2-prot-0003"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, defaultDir, "capability", "protect", src+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("rice-hosyn-%04d", i+1)
		s := strings.Replace(string(raw), "id: "+src+"\n", "id: "+id+"\n", 1)
		s = strings.Replace(s, "suite: capability\n", "suite: holdout\n", 1)
		p := filepath.Join(dir, "holdout", "protect", id+".yaml")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// 非名单身份读取 holdout 被拒（验收 4 前半）：validate --holdout 与 run --suite 含 holdout 都在读样本前失败关闭。
func TestHoldout_ReaderGate(t *testing.T) {
	dir := mountedHoldout(t)
	common := []string{"--suites-dir", dir, "--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot}
	tests := []struct {
		name, reader string
		args         []string
		wantCode     int
		wantOut      string
		wantErr      string
	}{
		{name: "validate no reader", args: []string{"validate", "--holdout"}, wantCode: exitFailed, wantErr: "holdout read denied"},
		{name: "validate not listed", reader: "prompt-tuner", args: []string{"validate", "--holdout"}, wantCode: exitFailed, wantErr: "holdout read denied"},
		{name: "run no reader", args: []string{"run", "--suite", "holdout"}, wantCode: exitFailed, wantErr: "holdout read denied"},
		{name: "run mixed not listed", reader: "QA-LEAD", args: []string{"run", "--suite", "regression,holdout"}, wantCode: exitFailed, wantErr: "holdout read denied"},
		// 不点名 holdout：默认跳过、只数文件，不需要身份。
		{name: "validate public", args: []string{"validate"}, wantCode: exitOK, wantOut: "holdout: 2 file(s) (content not read, not in total)"},
		{name: "validate owner", reader: "qa-lead", args: []string{"validate", "--holdout"}, wantCode: exitOK,
			wantOut: "holdout: 2 file(s), 2 counted (reader qa-lead; counts only)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envEvalReader, tt.reader)
			var stdout, stderr bytes.Buffer
			code := run(append(append([]string{}, tt.args...), common...), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%s", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOut) || !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stdout=%s\nstderr=%s\nwant out %q err %q", stdout.String(), stderr.String(), tt.wantOut, tt.wantErr)
			}
			if strings.Contains(stdout.String()+stderr.String(), "rice-hosyn-") {
				t.Fatal("holdout id leaked into validate output")
			}
		})
	}
}

// L2 评审 P1-1：--suites-dir 指向 holdout 子目录 / 换大小写时，不点名 holdout 也在读样本前拒绝。
func TestHoldout_SuitesDirInsideHoldoutDenied(t *testing.T) {
	dir := mountedHoldout(t)
	for _, sub := range []string{filepath.Join("holdout", "protect"), "Holdout"} {
		root := filepath.Join(dir, sub)
		if _, err := os.Stat(root); err != nil {
			continue // 大小写敏感文件系统上没有 Holdout
		}
		t.Run(sub, func(t *testing.T) {
			t.Setenv(envEvalReader, "")
			var stdout, stderr bytes.Buffer
			code := run([]string{"validate", "--suites-dir", root, "--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot}, &stdout, &stderr)
			if code != exitFailed || !strings.Contains(stderr.String(), "holdout read denied") {
				t.Fatalf("code = %d stderr=%s; want holdout read denied", code, stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "rice-hosyn-") {
				t.Fatal("holdout id leaked")
			}
		})
	}
}

// 真实 holdout 已挂载（eval/suites/holdout）时：授权读者下补写清单为 0（P2-L1 待办，公开侧 78 格由 holdout 补齐）。
// 未挂载时跳过；调度方在主仓挂 submodule 后跑。
func TestHoldout_MountedFillsCoverage(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot, defaultDir, "holdout")); err != nil {
		t.Skipf("holdout submodule not mounted: %v", err)
	}
	t.Setenv(envEvalReader, "qa-lead")
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--holdout", "--suites-dir", filepath.Join(repoRoot, defaultDir),
		"--taxonomy", filepath.Join(repoRoot, defaultTax), "--repo", repoRoot}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "补写清单: 0 sample(s) missing in 0 slot(s)") {
		t.Fatalf("authorized coverage still has gaps:\n%s", stdout.String())
	}
}
