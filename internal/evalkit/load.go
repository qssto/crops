package evalkit

import (
	"slices"
	"strings"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// LoadObs 是一个 trial 的知识加载观测（P2 L5，路线 §6.6 功能任务 1）：样本标注 + 该 run 全部 KB_LOADED 的加载条目。
// 三个离线加载指标（reduce/load.go）只读它；生产 loader 不自评，「是否无关」由这里按样本标注判定。
// Detail 为空（无 trial 库的单测 harness）或样本知识版本打不开时 Trial.Load 为 nil，指标不计该 trial。
type LoadObs struct {
	// QuestionType 是样本标注的一级问题类型（advice.category，无 advice 时取首个子类 tag 的前缀），报告按它切片。
	QuestionType string
	// Topic 是样本标注的子类（首个带「.」的 tag，如 protect.dosage），L8 低命中切片用。
	Topic string
	// MustCite 是样本 expect.advice.must_cite（kb:<kb_id>@<version>）；无 advice 期望时为空，不计命中率。
	MustCite []string
	// Items 是本 run 全部加载条目（多次加载按 load_seq 顺序拼接，同一 ref 只记第一次）。
	Items []LoadedItem
	// Strategies 是本 run KB_LOADED 记录的策略版本（去重，按出现顺序；P2 前的事件无此字段则为空）。
	Strategies []string
	// KBMiss 表示该 trial 以 KB_MISS 拒答（L8 清单）。
	KBMiss bool
}

// LoadedItem 是一个加载条目及其离线判定。
type LoadedItem struct {
	Ref        string // kb:<kb_id>@<version>
	PickReason string // MUST_INCLUDE / RANKED / COMMON；P2 前的事件为空，按 RANKED 计
	// Cited 表示卡片 citations 引用了该条目（kb_id 与版本都相同）。
	Cited bool
	// Irrelevant 表示条目与样本标注的作物 / 生育期 / 问题类型任一不匹配（条目通配「*」算匹配；样本生育期「不限」不判生育期）。
	Irrelevant bool
}

// Loaded 报告 ref 是否被加载。
func (o *LoadObs) Loaded(ref string) bool {
	return slices.ContainsFunc(o.Items, func(it LoadedItem) bool { return it.Ref == ref })
}

// entryLookup 是样本知识版本按 kb_id 取条目元数据（*knowledge.Snapshot 实现）。
type entryLookup interface {
	Entry(kbID string) (*knowledge.Entry, bool)
}

// observeLoad 由样本标注、run 详情里的 KB_LOADED 与卡片引用得出加载观测；条件不足时返回 nil。
func observeLoad(s *sample.Sample, t *Trial, kb grader.KnowledgeIndex) *LoadObs {
	idx, ok := kb.(entryLookup)
	if !ok || t.Detail == nil {
		return nil
	}
	o := &LoadObs{QuestionType: sampleCategory(s), Topic: sampleTopic(s), KBMiss: t.RefuseReason == string(model.FaultKBMiss)}
	if a := s.Expect.Advice; a != nil {
		o.MustCite = slices.Clone(a.MustCite)
	}
	cited := cardKBRefs(t.Card)
	for _, p := range t.Detail.Evidence.KBLoaded {
		if p.Strategy != "" && !slices.Contains(o.Strategies, p.Strategy) {
			o.Strategies = append(o.Strategies, p.Strategy)
		}
		for i, ref := range p.Entries {
			if o.Loaded(ref) {
				continue
			}
			it := LoadedItem{Ref: ref, Cited: slices.Contains(cited, ref)}
			if i < len(p.Items) && p.Items[i].Ref == ref {
				it.PickReason = p.Items[i].PickReason
			}
			it.Irrelevant = irrelevant(idx, ref, s, o.QuestionType)
			o.Items = append(o.Items, it)
		}
	}
	return o
}

// irrelevant 按样本标注判条目是否无关；ref 解析失败或条目不在样本知识版本中按无关计（加载了不该有的东西）。
func irrelevant(idx entryLookup, ref string, s *sample.Sample, category string) bool {
	kbID, _, err := knowledge.ParseCitationRef(ref)
	if err != nil {
		return true
	}
	e, ok := idx.Entry(kbID)
	if !ok {
		return true
	}
	cropOK := s.Crop == "" || e.Crop == knowledge.Wildcard || e.Crop == s.Crop
	stageOK := s.Stage == "" || s.Stage == knowledge.StageAny || slices.Contains(e.Stage, knowledge.Wildcard) ||
		slices.Contains(e.Stage, s.Stage)
	categoryOK := category == "" || slices.Contains(e.Category, category)
	return !(cropOK && stageOK && categoryOK)
}

// sampleTopic 是首个带「.」的 tag（样本约定首个子类 tag 即问题子类，见 sample.StrataOf；这里无 taxonomy，按形态取）。
func sampleTopic(s *sample.Sample) string {
	for _, tag := range s.Tags {
		if strings.Contains(tag, ".") {
			return tag
		}
	}
	return ""
}

// sampleCategory 是样本一级问题类型：advice.category 优先，否则取子类 tag 的前缀。
func sampleCategory(s *sample.Sample) string {
	if a := s.Expect.Advice; a != nil && a.Category != "" {
		return a.Category
	}
	cat, _, _ := strings.Cut(sampleTopic(s), ".")
	return cat
}

// cardKBRefs 是卡片 kb 引用（kb:<kb_id>@<version>，去重）。
func cardKBRefs(c *model.AdviceCard) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, ci := range c.Citations {
		if ref := knowledge.CitationRef(ci.KBID, ci.KBVersion); ci.Type == model.CitationKB && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}
