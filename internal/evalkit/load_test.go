package evalkit

import (
	"testing"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fakeEntries 是按 kb_id 查条目的假知识版本（实现 grader.KnowledgeIndex 与 entryLookup）。
type fakeEntries map[string]*knowledge.Entry

func (f fakeEntries) Has(kbID, version string) bool {
	e, ok := f[kbID]
	return ok && e.Version == version
}

func (f fakeEntries) Entry(kbID string) (*knowledge.Entry, bool) {
	e, ok := f[kbID]
	return e, ok
}

// hasOnly 只实现 Has（不能按 kb_id 取元数据），observeLoad 应返回 nil。
type hasOnly struct{}

func (hasOnly) Has(string, string) bool { return true }

func loadFixture() (fakeEntries, *sample.Sample) {
	entry := func(id, crop string, stages, cats []string) *knowledge.Entry {
		return &knowledge.Entry{KBID: id, Crop: crop, Stage: stages, Category: cats, Version: "v1"}
	}
	kb := fakeEntries{
		"rice/pest/planthopper": entry("rice/pest/planthopper", "rice", []string{"分蘖期"}, []string{"protect"}),
		"rice/pest/borer":       entry("rice/pest/borer", "rice", []string{"拔节孕穗期"}, []string{"protect"}),    // 生育期不符
		"rice/fertilization/n":  entry("rice/fertilization/n", "rice", []string{"*"}, []string{"fertilize"}), // 问题类型不符
		"common/safety/phi":     entry("common/safety/phi", "*", []string{"*"}, []string{"protect"}),
		"wheat/pest/aphid":      entry("wheat/pest/aphid", "wheat", []string{"*"}, []string{"protect"}), // 作物不符
	}
	s := &sample.Sample{ID: "rice-pest-9001", Crop: "rice", Stage: "分蘖期", Tags: []string{"L4", "protect.dosage"},
		Expect: sample.Expect{Advice: &sample.AdviceExpect{Category: "protect",
			MustCite: []string{"kb:rice/pest/planthopper@v1", "kb:common/safety/phi@v1", "kb:rice/pest/missing@v1"}}}}
	return kb, s
}

// 已知答案：两次加载拼接去重，pick_reason 按 Items 对齐，卡片引用与无关判定逐条可算。
func TestObserveLoad_KnownAnswer(t *testing.T) {
	kb, s := loadFixture()
	tr := Trial{RefuseReason: "", Card: &model.AdviceCard{Citations: []model.Citation{
		{Type: model.CitationKB, KBID: "rice/pest/planthopper", KBVersion: "v1"},
		{Type: model.CitationKB, KBID: "rice/pest/borer", KBVersion: "v2"}, // 版本不同不算引用
		{Type: model.CitationMeasured},
	}}}
	tr.Detail = &service.RunDetail{Evidence: service.Evidence{KBLoaded: []model.KBLoadedPayload{
		{Strategy: "v1", Entries: []string{"kb:common/safety/phi@v1", "kb:rice/pest/planthopper@v1", "kb:rice/pest/borer@v1"},
			Items: []model.KBLoadedEntry{{Ref: "kb:common/safety/phi@v1", PickReason: "MUST_INCLUDE"},
				{Ref: "kb:rice/pest/planthopper@v1", PickReason: "RANKED", Rank: 1}, {Ref: "kb:rice/pest/borer@v1", PickReason: "RANKED", Rank: 2}}},
		// 第二次加载（P2 前形态，无 Items / Strategy）：重复的 planthopper 只记一次。
		{Entries: []string{"kb:rice/pest/planthopper@v1", "kb:rice/fertilization/n@v1", "kb:wheat/pest/aphid@v1", "kb:rice/pest/gone@v1"}},
	}}}
	o := observeLoad(s, &tr, kb)
	if o == nil {
		t.Fatal("observeLoad = nil")
	}
	if o.QuestionType != "protect" || o.Topic != "protect.dosage" || o.KBMiss || len(o.MustCite) != 3 {
		t.Fatalf("obs header = %+v", o)
	}
	want := []LoadedItem{
		{Ref: "kb:common/safety/phi@v1", PickReason: "MUST_INCLUDE"},
		{Ref: "kb:rice/pest/planthopper@v1", PickReason: "RANKED", Cited: true},
		{Ref: "kb:rice/pest/borer@v1", PickReason: "RANKED", Irrelevant: true},
		{Ref: "kb:rice/fertilization/n@v1", Irrelevant: true},
		{Ref: "kb:wheat/pest/aphid@v1", Irrelevant: true},
		{Ref: "kb:rice/pest/gone@v1", Irrelevant: true}, // 不在样本知识版本
	}
	if len(o.Items) != len(want) {
		t.Fatalf("items = %+v", o.Items)
	}
	for i := range want {
		if o.Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, o.Items[i], want[i])
		}
	}
	if len(o.Strategies) != 1 || o.Strategies[0] != "v1" {
		t.Errorf("strategies = %v", o.Strategies)
	}
}

func TestObserveLoad_NilWithoutDetailOrIndex(t *testing.T) {
	kb, s := loadFixture()
	if o := observeLoad(s, &Trial{}, kb); o != nil {
		t.Errorf("no detail: %+v", o)
	}
	tr := Trial{Detail: &service.RunDetail{}}
	if o := observeLoad(s, &tr, hasOnly{}); o != nil {
		t.Errorf("index without Entry: %+v", o)
	}
	if o := observeLoad(s, &tr, nil); o != nil {
		t.Errorf("nil index: %+v", o)
	}
	tr.RefuseReason = string(model.FaultKBMiss)
	s.Expect.Advice, s.Stage = nil, knowledge.StageAny
	o := observeLoad(s, &tr, kb)
	if o == nil || !o.KBMiss || o.QuestionType != "protect" || len(o.MustCite) != 0 || len(o.Items) != 0 {
		t.Fatalf("kb miss obs = %+v", o)
	}
	// 样本生育期「不限」不判生育期。
	if irrelevant(kb, "kb:rice/pest/borer@v1", s, "protect") {
		t.Error("stage 不限 should not mark stage mismatch")
	}
}
