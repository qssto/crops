package coverage

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const testTaxonomy = `
version: 1
crop: rice
categories:
  - code: protect
    name: 植保
    subcategories:
      - {code: protect.timing, name: 防治时机}
      - {code: protect.dosage, name: 用量计算}
stages:
  - {code: 分蘖期, name: 分蘖期}
  - {code: 不限, name: 与生育期无关}
sample_tags:
  - {code: trap.similar_pest, name: a}
  - {code: trap.overdose, name: b}
  - {code: trap.cross_crop, name: c}
  - {code: trap.stale_kb, name: d}
  - {code: user_sim, name: e}
`

const testTargets = `
version: 1
crop: rice
total_min: 6
cells:
  - {stage: 分蘖期, topic: protect.timing, easy: 1, medium: 2, hard: 1}
  - {stage: 分蘖期, topic: protect.dosage, easy: 0, medium: 1, hard: 1}
refuse:
  ratio_min: 0.25
  min: {KB_MISS: 1, OUT_OF_SCOPE: 1}
traps:
  ratio_min: 0.10
  min: {trap.overdose: 1, trap.stale_kb: 1}
`

func mustTaxonomy(t *testing.T) *knowledge.Taxonomy {
	t.Helper()
	tx, err := knowledge.ParseTaxonomy([]byte(testTaxonomy))
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func mustTargets(t *testing.T, tx *knowledge.Taxonomy) *Targets {
	t.Helper()
	tg, err := ParseTargets([]byte(testTargets), tx)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func smp(suite, stage, topic, difficulty string, opts ...func(*sample.Sample)) *sample.Sample {
	s := &sample.Sample{Suite: suite, Stage: stage, Difficulty: difficulty, Crop: "rice",
		Tags: []string{"L4", topic}, Expect: sample.Expect{Outcome: model.OutcomeAdvice}}
	for _, o := range opts {
		o(s)
	}
	return s
}

func refused(reason string) func(*sample.Sample) {
	return func(s *sample.Sample) { s.Expect = sample.Expect{Outcome: model.OutcomeRefused, RefuseReason: reason} }
}

func tagged(tag string) func(*sample.Sample) {
	return func(s *sample.Sample) { s.Tags = append(s.Tags, tag) }
}

// 已知答案：8 条样本（1 条 user_sim、1 条 holdout 不计）→ 6 条计入；格子、拒答来源、陷阱组逐项核对。
func TestCount_KnownAnswer(t *testing.T) {
	tx := mustTaxonomy(t)
	tg := mustTargets(t, tx)
	reason := "flaky"
	samples := []*sample.Sample{
		smp("regression", "分蘖期", "protect.timing", "easy"),
		smp("regression", "分蘖期", "protect.timing", "medium", refused("KB_MISS")),
		smp("regression", "分蘖期", "protect.timing", "hard", tagged("trap.overdose"), tagged("trap.stale_kb")),
		smp("redteam", "分蘖期", "protect.dosage", "hard", refused("LLM_UNAVAILABLE")),
		smp("capability", "不限", "protect.timing", "medium", refused("OUT_OF_SCOPE"), func(s *sample.Sample) { s.Quarantine = &reason }),
		smp("capability", "分蘖期", "protect.dosage", "medium", tagged("trap.overdose")),
		smp("capability", "分蘖期", "protect.timing", "medium", tagged(sample.TagUserSim)),
		smp(HoldoutSuite, "分蘖期", "protect.timing", "medium"),
	}
	dir := t.TempDir()
	holdout := filepath.Join(dir, HoldoutSuite, "x")
	if err := os.MkdirAll(holdout, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.yaml", "b.yml", "README.md"} {
		// 内容非法也无妨：holdout 只数文件，不读内容
		if err := os.WriteFile(filepath.Join(holdout, name), []byte("{{not yaml"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Count(samples, tx, tg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counted != 6 || r.UserSim != 1 || r.Holdout != 2 || !r.HoldoutPresent || r.Quarantined != 1 {
		t.Fatalf("counted=%d user_sim=%d holdout=%d/%v quarantined=%d", r.Counted, r.UserSim, r.Holdout, r.HoldoutPresent, r.Quarantined)
	}
	if want := map[string]int{"regression": 3, "redteam": 1, "capability": 2}; !reflect.DeepEqual(r.BySuite, want) {
		t.Errorf("BySuite = %v, want %v", r.BySuite, want)
	}
	wantCells := map[CellKey]map[string]int{
		{"分蘖期", "protect.timing"}: {"easy": 1, "medium": 1, "hard": 1},
		{"分蘖期", "protect.dosage"}: {"medium": 1, "hard": 1},
		{"不限", "protect.timing"}:  {"medium": 1},
	}
	if !reflect.DeepEqual(r.Cells, wantCells) {
		t.Errorf("Cells = %v, want %v", r.Cells, wantCells)
	}
	if r.Refused != 3 || r.RefuseOther != 1 || !reflect.DeepEqual(r.RefuseBySrc, map[string]int{"KB_MISS": 1, "OUT_OF_SCOPE": 1}) {
		t.Errorf("refused=%d other=%d by=%v", r.Refused, r.RefuseOther, r.RefuseBySrc)
	}
	if r.Trapped != 2 || !reflect.DeepEqual(r.TrapByGroup, map[string]int{"trap.overdose": 2, "trap.stale_kb": 1}) {
		t.Errorf("trapped=%d by=%v", r.Trapped, r.TrapByGroup)
	}
	wantGaps := []Gap{{Key: CellKey{"分蘖期", "protect.timing"}, Category: "protect", Difficulty: "medium", Have: 1, Want: 2}}
	if got := r.Gaps(tx); !reflect.DeepEqual(got, wantGaps) {
		t.Errorf("Gaps = %+v, want %+v", got, wantGaps)
	}
	if got := r.OffTarget(); !reflect.DeepEqual(got, []CellKey{{"不限", "protect.timing"}}) {
		t.Errorf("OffTarget = %v", got)
	}
	// 6 >= 6 总数达标；拒答 3/6 = 50%；陷阱 2/6；只差一格 medium。
	if got, want := r.Shortfalls(tx), []string{"1 slot(s) below target, 1 sample(s) missing"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Shortfalls = %v, want %v", got, want)
	}
}

func TestCount_HoldoutAbsentAndShortfalls(t *testing.T) {
	tx := mustTaxonomy(t)
	r, err := Count([]*sample.Sample{smp("regression", "分蘖期", "protect.timing", "easy")}, tx, mustTargets(t, tx), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.HoldoutPresent || r.Holdout != 0 {
		t.Fatalf("holdout = %d/%v, want absent", r.Holdout, r.HoldoutPresent)
	}
	want := []string{
		// 只有 timing/easy 一条：timing medium 缺 2、hard 缺 1，dosage medium、hard 各缺 1 → 4 槽 5 条
		"total 1 < 6", "4 slot(s) below target, 5 sample(s) missing",
		"refuse ratio 0.0% < 25%", "refuse KB_MISS 0 < 1", "refuse OUT_OF_SCOPE 0 < 1",
		"trap ratio 0.0% < 10%", "trap trap.overdose 0 < 1", "trap trap.stale_kb 0 < 1",
	}
	if got := r.Shortfalls(tx); !reflect.DeepEqual(got, want) {
		t.Errorf("Shortfalls = %q\nwant %q", got, want)
	}
}

func TestRender(t *testing.T) {
	tx := mustTaxonomy(t)
	r, err := Count([]*sample.Sample{
		smp("regression", "分蘖期", "protect.timing", "easy"),
		smp("regression", "分蘖期", "protect.timing", "medium", refused("KB_MISS")),
	}, tx, mustTargets(t, tx), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var plain, color bytes.Buffer
	if err := Render(&plain, r, tx, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Render(&color, r, tx, RenderOptions{Color: true}); err != nil {
		t.Fatal(err)
	}
	out := plain.String()
	for _, want := range []string{
		"coverage: 2 / 6 samples !", "holdout: absent (skipped)", "1/2!", "refuse: 1/2 = 50.0% (min 25%)",
		"OUT_OF_SCOPE", "补写清单: 4 sample(s) missing in 4 slot(s)", "protect    分蘖期       medium  +2", "protect    分蘖期       hard    +2",
		"分蘖期       protect.timing       medium  1/2    +1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ansiRed) || !strings.Contains(color.String(), ansiRed) {
		t.Error("ANSI red must appear only with Color")
	}
}

func TestSummarize(t *testing.T) {
	gaps := []Gap{
		{Key: CellKey{"不限", "protect.dosage"}, Category: "protect", Difficulty: "hard", Have: 0, Want: 1},
		{Key: CellKey{"分蘖期", "protect.timing"}, Category: "protect", Difficulty: "hard", Have: 0, Want: 2},
		{Key: CellKey{"分蘖期", "protect.dosage"}, Category: "protect", Difficulty: "hard", Have: 1, Want: 2},
		{Key: CellKey{"分蘖期", "protect.dosage"}, Category: "protect", Difficulty: "easy", Have: 0, Want: 1},
	}
	// 生育期按物候序（不限殿后），同生育期内难度按 easy → hard
	want := []GapSummary{{"protect", "分蘖期", "easy", 1}, {"protect", "分蘖期", "hard", 3}, {"protect", "不限", "hard", 1}}
	if got := Summarize(gaps); !reflect.DeepEqual(got, want) {
		t.Fatalf("Summarize = %+v, want %+v", got, want)
	}
}

func TestParseTargets_Errors(t *testing.T) {
	tx := mustTaxonomy(t)
	base := "version: 1\ncrop: rice\ntotal_min: 1\n"
	tests := map[string]string{
		"empty":           "",
		"unknown field":   base + "typo: 1\n",
		"other crop":      "crop: wheat\ntotal_min: 1\ncells: [{stage: 分蘖期, topic: protect.timing, easy: 1}]\n",
		"unknown stage":   base + "cells: [{stage: 分蘖盛期, topic: protect.timing, easy: 1}]\n",
		"category topic":  base + "cells: [{stage: 分蘖期, topic: protect, easy: 1}]\n",
		"negative":        base + "cells: [{stage: 分蘖期, topic: protect.timing, easy: -1, medium: 2}]\n",
		"duplicate row":   base + "cells: [{stage: 分蘖期, topic: protect.timing, easy: 1}, {stage: 分蘖期, topic: protect.timing, easy: 1}]\n",
		"total above sum": "crop: rice\ntotal_min: 5\ncells: [{stage: 分蘖期, topic: protect.timing, easy: 1}]\n",
		"bad refuse key":  base + "cells: [{stage: 分蘖期, topic: protect.timing, easy: 1}]\nrefuse: {min: {LLM_UNAVAILABLE: 1}}\n",
		"bad trap key":    base + "cells: [{stage: 分蘖期, topic: protect.timing, easy: 1}]\ntraps: {min: {trap.other: 1}}\n",
		"ratio over 1":    base + "cells: [{stage: 分蘖期, topic: protect.timing, easy: 1}]\nrefuse: {ratio_min: 2}\n",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTargets([]byte(raw), tx); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

// 仓库的 eval/coverage/targets.yaml 须可加载、目标之和 ≥ 500，文件头注明待修订。
func TestRepoTargets(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "eval")
	tx, err := knowledge.LoadTaxonomy(filepath.Join(root, "taxonomy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "coverage", "targets.yaml")
	tg, err := LoadTargets(path, tx)
	if err != nil {
		t.Fatal(err)
	}
	if tg.TotalMin < 500 {
		t.Errorf("total_min = %d, want >= 500", tg.TotalMin)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "待依 Phase 1 线上分布修订") {
		t.Error("targets.yaml header must say 待依 Phase 1 线上分布修订")
	}
}
