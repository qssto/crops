package knowledge

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// 区域隔离 fixture（P1 L1-B）：仓库知识库 + 一个安徽专属条目 + 一个非 common/ 的 region="*" 条目，不改正式知识库。
const (
	fmAnhui = `---
kb_id: rice/anhui/planthopper-timing
title: 安徽沿淮稻飞虱防治适期（测试 fixture）
crop: rice
stage: [分蘖期, 拔节孕穗期]
region: anhui
category: [protect]
topics: [protect.timing]
target: planthopper
authority_level: 地标
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
basis:
  - {source: 测试 fixture, level: 地标, verified: false}
---
## 防治适期
测试 fixture，仅用于区域过滤用例。
`
	fmNational = `---
kb_id: rice/national/irrigation-note
title: 非 common 的全国条目（测试 fixture，应对任何区域不可见）
crop: rice
stage: ['*']
region: '*'
category: [irrigate]
topics: [irrigate.depth]
authority_level: 国标
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
basis:
  - {source: 测试 fixture, level: 国标, verified: false}
---
## 说明
测试 fixture。
`
	idxFixture = `  - kb_id: rice/anhui/planthopper-timing
    file: rice/anhui.md
    version: v1
    crop: rice
    stage: [分蘖期, 拔节孕穗期]
    region: anhui
    category: [protect]
    topics: [protect.timing]
    target: planthopper
    authority_level: 地标
  - kb_id: rice/national/irrigation-note
    file: rice/national.md
    version: v1
    crop: rice
    stage: ['*']
    region: '*'
    category: [irrigate]
    topics: [irrigate.depth]
    authority_level: 国标
`
)

// regionFixture 复制仓库 knowledge/ 并追加 fixture 条目。
func regionFixture(t *testing.T) *Snapshot {
	t.Helper()
	root := filepath.Join(repoRoot(t), "knowledge")
	m := fstest.MapFS{}
	err := fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
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
	m["index.yaml"].Data = append(m["index.yaml"].Data, []byte(idxFixture)...)
	m["rice/anhui.md"] = &fstest.MapFile{Data: []byte(fmAnhui)}
	m["rice/national.md"] = &fstest.MapFile{Data: []byte(fmNational)}
	snap, err := NewStore(FSReader{FS: m}).Open(context.Background(), RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestRegionCode(t *testing.T) {
	for in, want := range map[string]string{"江苏": "jiangsu", "江苏省": "jiangsu", "安徽": "anhui", " jiangsu ": "jiangsu", "Hubei": "hubei", "": ""} {
		if got := RegionCode(in); got != want {
			t.Errorf("RegionCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSelect_RegionIsolation20 是 §6.2 验收 5 的 20 条样本：只取本区域与 common/ 条目，结果逐条打印核对
// （记录见 docs/p1/L1-B-知识区域过滤-20样本.md）。
func TestSelect_RegionIsolation20(t *testing.T) {
	snap := regionFixture(t)
	tx := repoTaxonomy(t)
	const ah, js, hb = "anhui", "jiangsu", "hubei"
	samples := []Situation{
		{Crop: "rice", Region: js, Stage: "分蘖期", Topic: "protect.timing", Target: "planthopper"},
		{Crop: "rice", Region: ah, Stage: "分蘖期", Topic: "protect.timing", Target: "planthopper"},
		{Crop: "rice", Region: hb, Stage: "分蘖期", Topic: "protect.timing", Target: "planthopper"},
		{Crop: "rice", Region: js, Stage: "拔节孕穗期", Category: "protect", Target: "planthopper"},
		{Crop: "rice", Region: ah, Stage: "拔节孕穗期", Category: "protect", Target: "planthopper"},
		{Crop: "rice", Region: js, Stage: "分蘖期", Category: "protect", Target: "sheath_blight"},
		{Crop: "rice", Region: ah, Stage: "分蘖期", Category: "protect", Target: "sheath_blight"},
		{Crop: "rice", Region: js, Stage: "拔节孕穗期", Topic: "fertilize.rate"},
		{Crop: "rice", Region: ah, Stage: "拔节孕穗期", Topic: "fertilize.rate"},
		{Crop: "rice", Region: js, Stage: "分蘖期", Topic: "irrigate.depth"},
		{Crop: "rice", Region: ah, Stage: "分蘖期", Topic: "irrigate.depth"},
		{Crop: "rice", Region: hb, Stage: "分蘖期", Topic: "irrigate.depth"},
		{Crop: "rice", Region: js, Stage: "抽穗扬花期", Topic: "disaster.heat"},
		{Crop: "rice", Region: ah, Stage: "抽穗扬花期", Topic: "disaster.heat"},
		{Crop: "rice", Region: js, Stage: "分蘖期", Topic: "protect.product", Target: "stem_borer"},
		{Crop: "rice", Region: ah, Stage: "分蘖期", Topic: "protect.dosage", Target: "planthopper"},
		{Crop: "rice", Region: js, Stage: "秧苗期", Topic: "fertilize.timing"},
		{Crop: "rice", Region: hb, Stage: "秧苗期", Topic: "fertilize.timing"},
		{Crop: "rice", Region: js, Stage: "灌浆结实期", Topic: "irrigate.drain"},
		{Crop: "rice", Region: ah, Stage: "灌浆结实期", Topic: "irrigate.drain"},
	}
	var anhuiHit, jiangsuHit bool
	for i, sit := range samples {
		got, err := Select(snap, tx, sit, DefaultLimit)
		if err != nil && !errors.Is(err, ErrKBMiss) {
			t.Fatalf("#%02d %s: %v", i+1, sit, err)
		}
		var cells []string
		for _, e := range got {
			cells = append(cells, e.KBID+"@"+e.Region)
			if !(e.Region == sit.Region || (e.Region == Wildcard && strings.HasPrefix(e.KBID, commonPrefix))) {
				t.Errorf("#%02d %s: %s (region %s) leaked", i+1, sit, e.KBID, e.Region)
			}
			anhuiHit = anhuiHit || e.Region == ah
			jiangsuHit = jiangsuHit || e.Region == js
		}
		result := strings.Join(cells, ", ")
		if errors.Is(err, ErrKBMiss) {
			result = "KB_MISS"
		}
		t.Logf("| %02d | %s | %s | %s | %s | %s |", i+1, sit.Region, sit.Stage, sit.Category+sit.Topic, sit.Target, result)
	}
	if !anhuiHit || !jiangsuHit {
		t.Fatalf("fixture not exercised: anhui hit %v, jiangsu hit %v", anhuiHit, jiangsuHit)
	}
}

// TestSelect_RegionIsolation_NoCrossRegion：安徽只见安徽 + common/；江苏不见安徽；未知区域只剩通用条目即 KB_MISS。
func TestSelect_RegionIsolation_NoCrossRegion(t *testing.T) {
	snap := regionFixture(t)
	tx := repoTaxonomy(t)
	sit := Situation{Crop: "rice", Stage: "分蘖期", Topic: "protect.timing", Target: "planthopper"}
	tests := []struct {
		region   string
		mustHave string
		mustNot  string
		wantMiss bool
	}{
		{region: "anhui", mustHave: "rice/anhui/planthopper-timing", mustNot: "rice/pest/planthopper"},
		{region: "jiangsu", mustHave: "rice/pest/planthopper", mustNot: "rice/anhui/planthopper-timing"},
		{region: "hubei", wantMiss: true},
	}
	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			s := sit
			s.Region = tt.region
			got, err := Select(snap, tx, s, DefaultLimit)
			if tt.wantMiss {
				if !errors.Is(err, ErrKBMiss) {
					t.Fatalf("got %v, %v; want KB_MISS", ids(got), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(ids(got), tt.mustHave) || slices.Contains(ids(got), tt.mustNot) {
				t.Fatalf("got %v, want %s and not %s", ids(got), tt.mustHave, tt.mustNot)
			}
		})
	}
	irr := Situation{Crop: "rice", Region: "jiangsu", Stage: "分蘖期", Topic: "irrigate.depth"}
	got, err := Select(snap, tx, irr, 50)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids(got), "rice/national/irrigation-note") {
		t.Fatalf("non-common wildcard entry visible: %v", ids(got))
	}
}
