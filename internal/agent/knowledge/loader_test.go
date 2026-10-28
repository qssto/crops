package knowledge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func ids(es []*Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.KBID
	}
	return out
}

func repoLoader(t *testing.T, opts ...LoaderOption) *Loader {
	t.Helper()
	l, err := NewLoader(NewStore(NewRepoReader(repoRoot(t))), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLoader_Load(t *testing.T) {
	l := repoLoader(t)
	tx := repoTaxonomy(t)
	tests := []struct {
		name    string
		sit     Situation
		want    []string // 期望命中（按顺序）；nil 时只检查 mustHave
		mustHas []string
		maxLen  int
	}{
		{name: "planthopper at tillering", maxLen: DefaultLimit,
			sit: Situation{Crop: "rice", Region: "jiangsu", Stage: "分蘖期", Topic: "protect.timing", Target: "planthopper"},
			// 专属条目（crop/region/stage/target 都具体）排在通配条目前；同分按 kb_id
			want: []string{"rice/pest/planthopper", "rice/phenology/stages", "common/operation_window/spray",
				"common/operation_window/uav", "common/safety/phi"}},
		{name: "panicle fertilizer rate",
			sit:     Situation{Crop: "rice", Region: "jiangsu", Stage: "拔节孕穗期", Topic: "fertilize.rate"},
			mustHas: []string{"rice/fertilization/panicle", "rice/fertilization/n-budget"}, maxLen: DefaultLimit},
		{name: "target filters other pests",
			sit:     Situation{Crop: "rice", Region: "jiangsu", Stage: "分蘖期", Category: "protect", Target: "sheath_blight"},
			mustHas: []string{"rice/pest/sheath-blight"}, maxLen: DefaultLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Load(context.Background(), RefWorktree, tx, tt.sit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) > tt.maxLen {
				t.Errorf("len = %d, want <= %d", len(got), tt.maxLen)
			}
			if tt.want != nil && !slices.Equal(ids(got), tt.want) {
				t.Errorf("got %v, want %v", ids(got), tt.want)
			}
			for _, id := range tt.mustHas {
				if !slices.Contains(ids(got), id) {
					t.Errorf("got %v, missing %s", ids(got), id)
				}
			}
			for _, e := range got {
				if e.Target != "" && tt.sit.Target != "" && e.Target != tt.sit.Target {
					t.Errorf("%s target %s leaked into %s query", e.KBID, e.Target, tt.sit.Target)
				}
			}
		})
	}
}

func TestLoader_LimitConfigurable(t *testing.T) {
	sit := Situation{Crop: "rice", Region: "jiangsu", Stage: "分蘖期", Category: "protect"}
	got, err := repoLoader(t, WithLimit(2)).Load(context.Background(), "", repoTaxonomy(t), sit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if _, err := NewLoader(NewStore(fsWith(validFiles())), WithLimit(0)); err == nil {
		t.Fatal("limit 0 must be rejected")
	}
}

// 排序：authority_level 高的排前，即使它是通配条目。
func TestSelect_AuthorityOrder(t *testing.T) {
	files := validFiles()
	files["common/safety.md"] = strings.NewReplacer(
		"kb_id: rice/pest/planthopper", "kb_id: common/safety/phi",
		"crop: rice", "crop: '*'", "region: jiangsu", "region: '*'", "[分蘖期]", "['*']",
		"target: planthopper\n", "", "authority_level: 专家经验", "authority_level: 国标").Replace(fmPlanthopper)
	files["index.yaml"] = idxPlanthopper + `  - kb_id: common/safety/phi
    file: common/safety.md
    version: v1
    crop: '*'
    stage: ['*']
    region: '*'
    category: [protect]
    topics: [protect.timing]
    authority_level: 国标
`
	snap, err := NewStore(fsWith(files)).Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(snap, repoTaxonomy(t), Situation{Crop: "rice", Region: "jiangsu", Stage: "分蘖期", Category: "protect"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"common/safety/phi", "rice/pest/planthopper"}; !slices.Equal(ids(got), want) {
		t.Fatalf("got %v, want %v", ids(got), want)
	}
}

// 验收 4：知识未覆盖的情境稳定返回 ErrKBMiss（重复 20 次结果一致），不返回任何条目。
func TestLoader_KBMissStable(t *testing.T) {
	l := repoLoader(t)
	tx := repoTaxonomy(t)
	misses := []struct {
		name string
		sit  Situation
	}{
		{name: "crop not covered (wheat)", sit: Situation{Crop: "wheat", Region: "jiangsu", Stage: "分蘖期", Category: "protect"}},
		{name: "region not covered", sit: Situation{Crop: "rice", Region: "heilongjiang", Stage: "分蘖期", Topic: "protect.identify"}},
		{name: "target not covered (kernel smut)", sit: Situation{Crop: "rice", Region: "jiangsu", Stage: "灌浆结实期", Topic: "protect.identify", Target: "kernel_smut"}},
		{name: "topic not covered at stage", sit: Situation{Crop: "rice", Region: "jiangsu", Stage: "成熟期", Topic: "fertilize.deficiency"}},
		{name: "unknown stage only matches nothing specific", sit: Situation{Crop: "rice", Region: "jiangsu", Topic: "disaster.lodging"}},
	}
	for _, m := range misses {
		t.Run(m.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				got, err := l.Load(context.Background(), RefWorktree, tx, m.sit)
				if !errors.Is(err, ErrKBMiss) || got != nil {
					t.Fatalf("run %d: got %v, err %v; want nil, ErrKBMiss", i, ids(got), err)
				}
			}
		})
	}
}

// 调用方错误（未知 code、缺维度）不是 KB_MISS，必须显式报错。
func TestLoader_BadSituation(t *testing.T) {
	l := repoLoader(t)
	tx := repoTaxonomy(t)
	for _, sit := range []Situation{
		{Region: "jiangsu", Category: "protect"},
		{Crop: "rice", Region: "jiangsu", Category: "protect.timing"},
		{Crop: "rice", Region: "jiangsu", Topic: "protect"},
		{Crop: "rice", Region: "jiangsu", Category: "irrigate", Topic: "protect.timing"},
		{Crop: "rice", Region: "jiangsu", Stage: "分蘖末期", Category: "protect"},
	} {
		if _, err := l.Load(context.Background(), "", tx, sit); err == nil || errors.Is(err, ErrKBMiss) {
			t.Errorf("%s: err = %v, want non-KB_MISS error", sit, err)
		}
	}
	if _, err := l.Load(context.Background(), "kb@TBD", tx, Situation{Crop: "rice", Region: "jiangsu", Category: "protect"}); !errors.Is(err, ErrBadRef) {
		t.Errorf("kb@TBD err = %v, want ErrBadRef", err)
	}
}
