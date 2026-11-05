package knowledge

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func mustStrategy(t *testing.T, spec StrategySpec, tx *Taxonomy) *Strategy {
	t.Helper()
	s, err := NewStrategy(spec, tx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// allSituations 枚举仓库知识库可达的全部情境：作物 × 生育期（含未知）× 区域 × 一级类别 × target（含空）。
func allSituations(snap *Snapshot, tx *Taxonomy) []Situation {
	crops, targets := []string{"rice", "wheat"}, []string{""}
	for _, e := range snap.Entries() {
		if e.Crop != Wildcard && !slices.Contains(crops, e.Crop) {
			crops = append(crops, e.Crop)
		}
		if e.Target != "" && !slices.Contains(targets, e.Target) {
			targets = append(targets, e.Target)
		}
	}
	var out []Situation
	for _, crop := range crops {
		for _, stage := range append([]string{""}, Stages...) {
			for _, region := range []string{"jiangsu", "anhui", "zhejiang"} {
				for _, cat := range tx.Categories {
					for _, target := range targets {
						out = append(out, Situation{Crop: crop, Stage: stage, Region: region, Category: cat.Code, Target: target})
					}
				}
			}
		}
	}
	return out
}

// v0 口径与现行 Select（上限 8）逐情境相同：同一顺序、同一错误类别（P2 §6.6 验收 3 的前提）。
func TestStrategy_PhaseZeroEqualsSelect(t *testing.T) {
	snap, tx := repoSnapshot(t), repoTaxonomy(t)
	v0 := mustStrategy(t, PhaseZeroSpec(), tx)
	sits := allSituations(snap, tx)
	hits := 0
	for _, sit := range sits {
		want, werr := Select(snap, tx, sit, PhaseZeroLimit)
		picks, gerr := v0.Select(snap, tx, sit)
		if (werr == nil) != (gerr == nil) || errors.Is(werr, ErrKBMiss) != errors.Is(gerr, ErrKBMiss) {
			t.Fatalf("%s: err %v vs %v", sit, werr, gerr)
		}
		if werr != nil {
			continue
		}
		hits++
		if got := Entries(picks); !slices.Equal(ids(got), ids(want)) {
			t.Fatalf("%s:\n v0 %v\n Select %v", sit, ids(got), ids(want))
		}
		for i, p := range picks {
			if p.Rank != i || p.Reason == PickMustInclude {
				t.Fatalf("%s: pick %d = %+v", sit, i, p)
			}
		}
	}
	if hits < 50 {
		t.Fatalf("only %d of %d situations hit; enumeration broken?", hits, len(sits))
	}
}

func TestStrategy_MustIncludeFirstAndReasons(t *testing.T) {
	snap, tx := repoSnapshot(t), repoTaxonomy(t)
	s := mustStrategy(t, StrategySpec{Version: "v9", Limit: 4, Order: []SortKey{SortAuthority, SortSpecificity},
		MustInclude: map[string][]MustRule{
			"protect":   {{KBID: "common/safety/phi"}, {KBID: "rice/pest/stem-borer"}}, // stem-borer 被 target 过滤掉
			"fertilize": {{File: "{crop}/fertilization.md", FormulasOnly: true}},
		}}, tx)

	picks, err := s.Select(snap, tx, Situation{Crop: "rice", Stage: "分蘖期", Region: "jiangsu", Category: "protect", Target: "planthopper"})
	if err != nil {
		t.Fatal(err)
	}
	if len(picks) != 4 || picks[0].Entry.KBID != "common/safety/phi" || picks[0].Reason != PickMustInclude {
		t.Fatalf("picks = %+v", picks)
	}
	if picks[1].Entry.KBID != "rice/pest/planthopper" || picks[1].Reason != PickRanked {
		t.Fatalf("pick 1 = %+v", picks[1])
	}
	for _, p := range picks[1:] {
		if p.Entry.KBID == "rice/pest/stem-borer" || p.Entry.KBID == "common/safety/phi" {
			t.Fatalf("must rule bypassed filter or duplicated: %+v", picks)
		}
		if want := rankedReason(p.Entry); p.Reason != want {
			t.Fatalf("%s reason %s, want %s", p.Entry.KBID, p.Reason, want)
		}
	}

	picks, err = s.Select(snap, tx, Situation{Crop: "rice", Stage: "分蘖期", Region: "jiangsu", Category: "fertilize"})
	if err != nil {
		t.Fatal(err)
	}
	if picks[0].Entry.KBID != "rice/fertilization/n-budget" || picks[0].Reason != PickMustInclude {
		t.Fatalf("fertilize formulas section not first: %+v", picks[0])
	}
	// 生育期不在 n-budget 范围（灌浆结实期）时规则跳过，不绕过过滤
	picks, err = s.Select(snap, tx, Situation{Crop: "rice", Stage: "灌浆结实期", Region: "jiangsu", Category: "fertilize"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range picks {
		if p.Entry.KBID == "rice/fertilization/n-budget" {
			t.Fatalf("n-budget loaded outside its stages: %+v", picks)
		}
	}
}

func TestStrategy_KBMissUnchanged(t *testing.T) {
	snap, tx := repoSnapshot(t), repoTaxonomy(t)
	s := mustStrategy(t, StrategySpec{Version: "v1", Limit: 5, Order: []SortKey{SortStageMatch},
		MustInclude: map[string][]MustRule{"protect": {{KBID: "common/safety/phi"}}}}, tx)
	_, err := s.Select(snap, tx, Situation{Crop: "wheat", Stage: "分蘖期", Region: "jiangsu", Category: "protect"})
	if !errors.Is(err, ErrKBMiss) {
		t.Fatalf("generic-only hits must stay KB_MISS even with must_include, got %v", err)
	}
}

func TestStrategy_SortKeys(t *testing.T) {
	sit := Situation{Crop: "rice", Stage: "分蘖期"}
	narrow := &Entry{KBID: "rice/a/z", Stage: []string{"分蘖期"}, Version: "v1"}
	wide := &Entry{KBID: "rice/a/a", Stage: []string{"返青期", "分蘖期"}, Version: "v3"}
	any := &Entry{KBID: "rice/a/b", Stage: []string{Wildcard}, Version: "v2"}
	byStage := &Strategy{spec: StrategySpec{Order: []SortKey{SortStageMatch}}}
	if !byStage.less(narrow, wide, sit) || !byStage.less(wide, any, sit) {
		t.Error("stage_match: narrower stage list must rank first, wildcard last")
	}
	byVersion := &Strategy{spec: StrategySpec{Order: []SortKey{SortVersion}}}
	if !byVersion.less(wide, any, sit) || !byVersion.less(any, narrow, sit) {
		t.Error("version: newer first")
	}
	if !byVersion.less(&Entry{KBID: "a/b/c", Version: "v10"}, &Entry{KBID: "a/b/d", Version: "v9"}, sit) {
		t.Error("version compares numerically")
	}
	tie := &Entry{KBID: "rice/a/y", Stage: []string{"分蘖期"}, Version: "v1"}
	if !byStage.less(tie, narrow, sit) {
		t.Error("ties break by kb_id asc")
	}
	byTopic := &Strategy{spec: StrategySpec{Order: []SortKey{SortTopicMatch}}}
	dosage := &Entry{KBID: "common/a/z", Topics: []string{"protect.timing", "protect.dosage"}}
	timing := &Entry{KBID: "common/a/a", Topics: []string{"protect.timing"}}
	if !byTopic.less(dosage, timing, Situation{RankTopic: "protect.dosage"}) {
		t.Error("topic_match: entry tagged with the question topic must rank first")
	}
	if !byTopic.less(timing, dosage, Situation{}) {
		t.Error("topic_match: no question topic means no preference (kb_id asc)")
	}
}

func TestNewStrategy_Rejects(t *testing.T) {
	tx := repoTaxonomy(t)
	ok := func() StrategySpec {
		return StrategySpec{Version: "v1", Limit: 5, Order: []SortKey{SortAuthority},
			MustInclude: map[string][]MustRule{"protect": {{KBID: "common/safety/phi"}}}}
	}
	tests := []struct {
		name   string
		mutate func(*StrategySpec)
		want   string
	}{
		{"bad version", func(s *StrategySpec) { s.Version = "1" }, "vN"},
		{"zero limit", func(s *StrategySpec) { s.Limit = 0 }, "limit"},
		{"limit over max", func(s *StrategySpec) { s.Limit = MaxLimit + 1 }, "limit"},
		{"empty order", func(s *StrategySpec) { s.Order = nil }, "order is empty"},
		{"unknown key", func(s *StrategySpec) { s.Order = []SortKey{"freshness"} }, "unknown sort key"},
		{"dup key", func(s *StrategySpec) { s.Order = []SortKey{SortVersion, SortVersion} }, "duplicate"},
		{"subcode key", func(s *StrategySpec) {
			s.MustInclude = map[string][]MustRule{"protect.timing": {{KBID: "common/safety/phi"}}}
		}, "top-level"},
		{"both kb_id and file", func(s *StrategySpec) {
			s.MustInclude["protect"] = []MustRule{{KBID: "common/safety/phi", File: "common/safety.md"}}
		}, "exactly one"},
		{"bad kb_id", func(s *StrategySpec) { s.MustInclude["protect"] = []MustRule{{KBID: "safety"}} }, "3 segments"},
		{"bad file", func(s *StrategySpec) { s.MustInclude["protect"] = []MustRule{{File: "../x.md"}} }, "relative .md"},
		{"empty rule list", func(s *StrategySpec) { s.MustInclude["protect"] = nil }, "has no rules"},
		{"no ranked slot", func(s *StrategySpec) {
			s.Limit = 1
		}, "no ranked slot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ok()
			tt.mutate(&spec)
			_, err := NewStrategy(spec, tx)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
	if _, err := NewStrategy(ok(), tx); err != nil {
		t.Fatal(err)
	}
}

func TestNewStrategy_CopiesSpec(t *testing.T) {
	spec := StrategySpec{Version: "v1", Limit: 5, Order: []SortKey{SortAuthority},
		MustInclude: map[string][]MustRule{"protect": {{KBID: "common/safety/phi"}}}}
	s := mustStrategy(t, spec, nil)
	spec.Order[0] = "bogus"
	spec.MustInclude["protect"][0].KBID = "x/y/z"
	if s.spec.Order[0] != SortAuthority || s.spec.MustInclude["protect"][0].KBID != "common/safety/phi" {
		t.Fatal("strategy must not alias caller's spec")
	}
}

// P1-1：必加条目数 > Limit-1（两条按文件规则在植保情境命中 8 条，上限 3）时 Select 报错、Check 启动失败，
// 不再把专属条目全部挤掉。
func TestStrategy_MustIncludeOverLimit(t *testing.T) {
	snap, tx := repoSnapshot(t), repoTaxonomy(t)
	s := mustStrategy(t, StrategySpec{Version: "v1", Limit: 3, Order: []SortKey{SortAuthority},
		MustInclude: map[string][]MustRule{"protect": {{File: "common/safety.md"}, {File: "common/operation_window.md"}}}}, tx)
	_, err := s.Select(snap, tx, Situation{Crop: "rice", Stage: "分蘖期", Region: "jiangsu", Category: "protect", Target: "planthopper"})
	if !errors.Is(err, ErrMustIncludeOverLimit) {
		t.Fatalf("Select err = %v, want ErrMustIncludeOverLimit", err)
	}
	if err := s.Check(snap); !errors.Is(err, ErrMustIncludeOverLimit) {
		t.Fatalf("Check err = %v, want ErrMustIncludeOverLimit", err)
	}
}

// P1-2：拼错的 kb_id / 文件名、类别不符的条目，在快照中匹配为空 → Check 失败并点名规则。
func TestStrategy_CheckUnmatchedRules(t *testing.T) {
	snap, tx := repoSnapshot(t), repoTaxonomy(t)
	for name, r := range map[string]MustRule{
		"typo kb_id":     {KBID: "common/safety/phl"},
		"typo file":      {File: "commn/safety.md"},
		"wrong category": {KBID: "common/operation_window/fertilize"}, // 施肥类条目不属植保
	} {
		t.Run(name, func(t *testing.T) {
			s := mustStrategy(t, StrategySpec{Version: "v1", Limit: 8, Order: []SortKey{SortAuthority},
				MustInclude: map[string][]MustRule{"protect": {{KBID: "common/safety/phi"}, r}}}, tx)
			if un := s.UnmatchedRules(snap, "protect"); !slices.Equal(un, []string{r.String()}) {
				t.Fatalf("UnmatchedRules = %v, want [%s]", un, r)
			}
			err := s.Check(snap)
			if err == nil || !strings.Contains(err.Error(), r.String()) || errors.Is(err, ErrMustIncludeOverLimit) {
				t.Fatalf("Check err = %v, want unmatched %s", err, r)
			}
		})
	}
	ok := mustStrategy(t, StrategySpec{Version: "v1", Limit: 10, Order: []SortKey{SortAuthority},
		MustInclude: map[string][]MustRule{"protect": {{File: "common/safety.md"}, {File: "common/operation_window.md"}},
			"fertilize": {{File: "{crop}/fertilization.md", FormulasOnly: true}}}}, tx)
	if err := ok.Check(snap); err != nil {
		t.Fatalf("repo-shaped v1 Check: %v", err)
	}
	if err := ok.Check(nil); err == nil {
		t.Fatal("Check(nil) must fail")
	}
}

// 必加条目全是 common/ 时，最后一个名额留给作物 / 防治对象专属条目（排序在前的通用条目不得占满）。
func TestStrategy_ReservesSpecificSlot(t *testing.T) {
	snap, tx := repoSnapshot(t), repoTaxonomy(t)
	must := map[string][]MustRule{"protect": {{KBID: "common/safety/phi"}}}
	s := mustStrategy(t, StrategySpec{Version: "v1", Limit: 2, Order: []SortKey{SortAuthority}, MustInclude: must}, tx)
	exercised := 0
	for _, sit := range allSituations(snap, tx) {
		if sit.Category != "protect" {
			continue
		}
		picks, err := s.Select(snap, tx, sit)
		if errors.Is(err, ErrKBMiss) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", sit, err)
		}
		if len(picks) != 2 || picks[1].Reason != PickRanked {
			t.Fatalf("%s: picks %+v, want must + one specific entry", sit, picks)
		}
		// 未留名额时排序第一的非必加条目是 common/：本情境确实走到了留名额分支。
		_, hits, err := filterHits(snap, tx, sit)
		if err != nil {
			t.Fatal(err)
		}
		hits = slices.DeleteFunc(hits, func(e *Entry) bool { return e.KBID == "common/safety/phi" })
		slices.SortStableFunc(hits, func(a, b *Entry) int {
			if s.less(a, b, sit) {
				return -1
			}
			if s.less(b, a, sit) {
				return 1
			}
			return 0
		})
		if rankedReason(hits[0]) == PickCommon {
			exercised++
		}
	}
	if exercised == 0 {
		t.Fatal("no situation ranks a common entry first; reserve branch never exercised")
	}
}
