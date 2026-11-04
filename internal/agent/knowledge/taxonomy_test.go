package knowledge

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestParseTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{name: "ok", yaml: `
version: 1
categories:
  - code: protect
    name: 植保
    subcategories:
      - {code: protect.timing, name: 防治时机}`},
		{name: "no categories", yaml: "version: 1\n", wantErr: true},
		{name: "duplicate code", yaml: `
categories:
  - {code: protect, name: 植保}
  - {code: protect, name: 植保2}`, wantErr: true},
		{name: "sub without parent prefix", yaml: `
categories:
  - code: protect
    name: 植保
    subcategories:
      - {code: timing, name: 防治时机}`, wantErr: true},
		{name: "empty code", yaml: `
categories:
  - {code: "", name: 空}`, wantErr: true},
		{name: "unknown field", yaml: `
categories:
  - {code: protect, name: 植保, typo: x}`, wantErr: true},
		{name: "stages and sample tags", yaml: `
categories:
  - {code: protect, name: 植保}
stages:
  - {code: 分蘖期, name: 分蘖期}
sample_tags:
  - {code: trap.overdose, name: 超剂量诱导}`},
		{name: "duplicate stage", yaml: `
categories:
  - {code: protect, name: 植保}
stages:
  - {code: 分蘖期, name: a}
  - {code: 分蘖期, name: b}`, wantErr: true},
		{name: "empty stage code", yaml: `
categories:
  - {code: protect, name: 植保}
stages:
  - {code: "", name: a}`, wantErr: true},
		{name: "sample tag clashes with category", yaml: `
categories:
  - {code: protect, name: 植保}
sample_tags:
  - {code: protect, name: a}`, wantErr: true},
		{name: "stage clashes with sample tag", yaml: `
categories:
  - {code: protect, name: 植保}
stages:
  - {code: x, name: a}
sample_tags:
  - {code: x, name: a}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTaxonomy([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// 仓库内的 eval/taxonomy.yaml 必须可加载，且四类问题域齐全。
func TestLoadTaxonomy_RepoFile(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "eval", "taxonomy.yaml")
	tx, err := LoadTaxonomy(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, code := range []string{"fertilize", "protect", "irrigate", "disaster"} {
		if cat, ok := tx.CategoryOf(code); !ok || cat != code {
			t.Errorf("category %q missing", code)
		}
	}
	if cat, ok := tx.CategoryOf("protect.product"); !ok || cat != "protect" {
		t.Errorf("CategoryOf(protect.product) = %q, %v", cat, ok)
	}
	if tx.Has("protect.unknown") {
		t.Error("Has(protect.unknown) = true")
	}
	// P2 L1-T：分层生育期 = Stages 枚举 + 不限，顺序一致；四个陷阱组与 user_sim 齐全。
	var stages []string
	for _, s := range tx.Stages {
		stages = append(stages, s.Code)
	}
	if want := append(slices.Clone(Stages), StageAny); !slices.Equal(stages, want) {
		t.Errorf("taxonomy stages = %v, want %v", stages, want)
	}
	for _, code := range []string{"trap.similar_pest", "trap.overdose", "trap.cross_crop", "trap.stale_kb", "user_sim"} {
		if !tx.HasSampleTag(code) {
			t.Errorf("sample tag %q missing", code)
		}
	}
	if tx.HasStage("protect") || tx.HasSampleTag("分蘖期") {
		t.Error("stage / sample tag sets leak into each other")
	}
}
