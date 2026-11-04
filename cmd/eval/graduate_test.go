package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/sample"
)

// writeCapabilitySuite 把仓库 capability 样本复制到临时 suites 目录（含一个子目录），返回 suites 根。
func writeCapabilitySuite(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for i, name := range []string{"rice-cap-0001.yaml", "rice-cap-0002.yaml"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, defaultDir, suiteCapability, name))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, suiteCapability)
		if i == 1 {
			dir = filepath.Join(dir, "topic")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadCaps(t *testing.T, root string) []*sample.Sample {
	t.Helper()
	samples, problems, err := sample.LoadDir(root)
	if err != nil || len(problems) > 0 {
		t.Fatalf("load: %v %v", err, problems)
	}
	return samples
}

// --apply：可毕业样本移到 regression/（保留子目录），只改 suite 行；不可毕业的原地不动；dry run 不动文件。
func TestGraduate_Apply(t *testing.T) {
	root := writeCapabilitySuite(t)
	caps := loadCaps(t, root)
	src := filepath.Join(root, suiteCapability, "topic", "rice-cap-0002.yaml")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	var dry bytes.Buffer
	if err := graduate(&dry, root, caps, []string{"rice-cap-0002"}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), "1 eligible") || !strings.Contains(dry.String(), "dry run") {
		t.Errorf("dry run output = %s", dry.String())
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("dry run must not move: %v", err)
	}

	var out bytes.Buffer
	if err := graduate(&out, root, caps, []string{"rice-cap-0002"}, true); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, suiteRegression, "topic", "rice-cap-0002.yaml")
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("moved file: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source must be removed, stat err = %v", err)
	}
	if want := strings.Replace(string(before), "suite: capability\n", "suite: regression\n", 1); string(after) != want {
		t.Errorf("moved content differs beyond the suite line:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(root, suiteCapability, "rice-cap-0001.yaml")); err != nil {
		t.Errorf("non-eligible sample must stay: %v", err)
	}
	// 移动后仍是合法样本：suite 与目录一致、version 不变
	moved, err := sample.Parse(after)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Suite != suiteRegression || moved.Version != 1 {
		t.Errorf("moved suite=%s version=%d", moved.Suite, moved.Version)
	}
}

// 目标已存在时不覆盖、源文件保留。
func TestGraduate_ApplyRefusesOverwrite(t *testing.T) {
	root := writeCapabilitySuite(t)
	caps := loadCaps(t, root)
	dest := filepath.Join(root, suiteRegression, "rice-cap-0001.yaml")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := graduate(&out, root, caps, []string{"rice-cap-0001"}, true); err == nil {
		t.Fatal("want error on existing destination")
	}
	if raw, err := os.ReadFile(dest); err != nil || string(raw) != "existing" {
		t.Errorf("destination overwritten: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(root, suiteCapability, "rice-cap-0001.yaml")); err != nil {
		t.Errorf("source must stay: %v", err)
	}
}

// 应失败：第二个样本的目标已存在时，第一个样本也不得先被移走（移动前统一预检）。
func TestGraduate_ApplyPrechecksAllDestinations(t *testing.T) {
	root := writeCapabilitySuite(t)
	caps := loadCaps(t, root)
	dest := filepath.Join(root, suiteRegression, "topic", "rice-cap-0002.yaml")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := graduate(&out, root, caps, []string{"rice-cap-0001", "rice-cap-0002"}, true); err == nil {
		t.Fatal("want error on existing destination")
	}
	if _, err := os.Stat(filepath.Join(root, suiteCapability, "rice-cap-0001.yaml")); err != nil {
		t.Errorf("rice-cap-0001 moved before precheck failed: %v", err)
	}
}

// 应失败：同 id 两个文件时 graduate 拒绝（不连库），否则毕业 SQL 会把两份 trial 混在一起。
func TestCmdGraduate_RejectsDuplicateID(t *testing.T) {
	root := writeCapabilitySuite(t)
	raw, err := os.ReadFile(filepath.Join(root, suiteCapability, "rice-cap-0001.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, suiteCapability, "topic", "dup.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"graduate", "--suites-dir", root, "--config", filepath.Join(t.TempDir(), "missing.yaml")}, &stdout, &stderr)
	if code == exitOK || !strings.Contains(stderr.String(), "duplicate sample id rice-cap-0001") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestMoveToRegression_NeedsSingleSuiteLine(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.yaml")
	if err := os.WriteFile(src, []byte("id: x\nsuite: regression\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveToRegression(src, filepath.Join(dir, "out", "x.yaml")); err == nil {
		t.Fatal("want error without suite: capability line")
	}
}

// 没有 capability 样本时不连库（配置文件都不读）。
func TestCmdGraduate_NoCapabilitySamples(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"graduate", "--suites-dir", filepath.Join(repoRoot, defaultDir, "constraint"),
		"--config", filepath.Join(t.TempDir(), "missing.yaml")}, &stdout, &stderr)
	if code != exitOK || !strings.Contains(stdout.String(), "0 capability sample(s), 0 eligible") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
