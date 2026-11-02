package calc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"agri-steward/internal/agent/knowledge"
)

// phiOnlyProduct 是追加到仓库登记表末尾的仅 PHI 来源产品（candidate:false，数值仅供测试）。
const phiOnlyProduct = `
  - id: phi-only-test
    common_name: 测试药
    formulation: 悬浮剂
    content: 10%
    registration_no: null
    candidate: false
    registrations:
      - {crop: rice, target: planthopper, method: 喷雾, dilution_note: x, phi_days: 7, max_times_per_season: 1,
         regions: ["*"], confidence: medium}
`

// TestCandidates_SkipsPHIOnly（复核 7 遗留）：candidate:false 产品不进候选集合，也不能作为作业单所用药剂。
func TestCandidates_SkipsPHIOnly(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "..", "..", "..", "knowledge")
	m := fstest.MapFS{}
	err = fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return err
		}
		m[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m[knowledge.RegistryFile].Data = append(m[knowledge.RegistryFile].Data, []byte(phiOnlyProduct)...)
	snap, err := knowledge.NewStore(knowledge.FSReader{FS: m}).Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(snap)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Candidates("rice", "planthopper", "jiangsu")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "phi-only-test") {
		t.Fatalf("candidates %v include candidate:false product", got)
	}
	if _, err := c.Registration(use("phi-only-test", "planthopper")); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Registration(phi-only) err = %v, want ErrNotRegistered", err)
	}
	if p := snap.Registry().Resolve("测试药"); len(p) != 1 {
		t.Fatalf("Resolve(测试药) = %d products, want 1 (PHI source)", len(p))
	}
}
