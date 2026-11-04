package coverage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/sample"
)

// holdoutDirWith 在 dir/holdout/protect/ 下写 n 个样本文件（内容非法：未授权路径不得打开）。
func holdoutDirWith(t *testing.T, n int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	hd := filepath.Join(dir, HoldoutSuite, "protect")
	if err := os.MkdirAll(hd, 0o755); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := range n {
		p := filepath.Join(hd, "rice-ho-prot-900"+string(rune('1'+i))+".yaml")
		if err := os.WriteFile(p, []byte("{{not yaml"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return dir, paths
}

func at(path string) func(*sample.Sample) { return func(s *sample.Sample) { s.Path = path } }

// L1 评审：holdout/ 下 suite 字段不是 holdout 的文件曾被 Counted 与 countHoldout 各算一次。
func TestCount_HoldoutDirNotDoubleCounted(t *testing.T) {
	tx := mustTaxonomy(t)
	dir, paths := holdoutDirWith(t, 1)
	samples := []*sample.Sample{
		smp("regression", "分蘖期", "protect.timing", "easy"),
		smp("regression", "分蘖期", "protect.timing", "medium", at(paths[0])),
	}
	r, err := Count(samples, tx, mustTargets(t, tx), dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counted != 1 || r.Holdout != 1 || r.BySuite["regression"] != 1 {
		t.Fatalf("counted=%d holdout=%d by=%v; holdout/ file counted twice", r.Counted, r.Holdout, r.BySuite)
	}
}

// 授权读者：holdout 样本（按路径或 suite 字段）计入格子与总数，HoldoutCounted 单列；未授权凭证被拒。
func TestCountWithHoldout(t *testing.T) {
	tx := mustTaxonomy(t)
	tg := mustTargets(t, tx)
	dir, paths := holdoutDirWith(t, 2)
	samples := []*sample.Sample{
		smp("regression", "分蘖期", "protect.timing", "easy"),
		smp(HoldoutSuite, "分蘖期", "protect.timing", "medium", at(paths[0])),
		smp(HoldoutSuite, "分蘖期", "protect.dosage", "hard", at(paths[1]), tagged("trap.overdose")),
	}
	if _, err := CountWithHoldout(samples, tx, tg, dir, nil); !errors.Is(err, sample.ErrHoldoutDenied) {
		t.Fatalf("nil grant: err = %v, want ErrHoldoutDenied", err)
	}
	g := grant(t)
	r, err := CountWithHoldout(samples, tx, tg, dir, g)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counted != 3 || r.HoldoutCounted != 2 || r.Holdout != 2 || r.BySuite[HoldoutSuite] != 2 || r.Trapped != 1 {
		t.Fatalf("counted=%d holdout_counted=%d files=%d by=%v trapped=%d", r.Counted, r.HoldoutCounted, r.Holdout, r.BySuite, r.Trapped)
	}
	if got := r.Count(CellKey{"分蘖期", "protect.dosage"}, "hard"); got != 1 {
		t.Fatalf("dosage/hard = %d, want 1 (holdout fills the cell)", got)
	}
	var b bytes.Buffer
	if err := Render(&b, r, tx, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "holdout: 2 file(s), 2 counted (reader qa-lead; counts only)") {
		t.Errorf("render missing authorized holdout line:\n%s", out)
	}
	// 报表对 holdout 只出计数，不出 ID / 路径。
	if strings.Contains(out, "rice-ho-") {
		t.Errorf("render leaks holdout id:\n%s", out)
	}
}

func grant(t *testing.T) *sample.HoldoutGrant {
	t.Helper()
	p := filepath.Join(t.TempDir(), "readers.yaml")
	if err := os.WriteFile(p, []byte("readers:\n  - id: qa-lead\n    role: owner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := sample.AuthorizeHoldout(p, "qa-lead")
	if err != nil {
		t.Fatal(err)
	}
	return g
}
