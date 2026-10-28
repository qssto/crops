package sample

import (
	"context"
	"fmt"
	"slices"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/knowledge"
)

// L3 追加的规则名。
const (
	RuleMustCiteKB          = "must_cite_kb"
	RuleProductInCandidates = "product_in_candidates"
)

// KnowledgeOpener 按样本 knowledge_ref 打开知识版本；*knowledge.Store 实现它（按 ref 缓存）。
type KnowledgeOpener interface {
	Open(ctx context.Context, ref string) (*knowledge.Snapshot, error)
}

// mustCiteKBCheck：knowledge_ref 能打开；must_cite 每项为 kb:<kb_id>@<version> 且在该版本中存在。
func mustCiteKBCheck(kb KnowledgeOpener) SampleCheck {
	return func(s *Sample) []string {
		if s.KnowledgeRef == "" {
			return nil // RuleRequired 已报
		}
		snap, err := kb.Open(context.Background(), s.KnowledgeRef)
		if err != nil {
			return []string{fmt.Sprintf("knowledge_ref %q cannot be opened: %v", s.KnowledgeRef, err)}
		}
		if s.Expect.Advice == nil {
			return nil
		}
		var msgs []string
		for i, ref := range s.Expect.Advice.MustCite {
			id, v, err := knowledge.ParseCitationRef(ref)
			switch {
			case err != nil:
				msgs = append(msgs, fmt.Sprintf("expect.advice.must_cite[%d]: %v", i, err))
			case !snap.Has(id, v):
				msgs = append(msgs, fmt.Sprintf("expect.advice.must_cite[%d] %s not in knowledge_ref %s", i, ref, s.KnowledgeRef))
			case snap.Deprecated(id):
				msgs = append(msgs, fmt.Sprintf("expect.advice.must_cite[%d] %s is deprecated", i, ref))
			}
		}
		return msgs
	}
}

// productInCheck：product_in 非空时必须给 target；product_in ⊆ calc.Candidates(taxonomy.crop, target, taxonomy.region)；
// must_cite 中带 target 的条目须与 expect.advice.target 一致。knowledge_ref 打不开时由 must_cite_kb 报，此处跳过。
func productInCheck(tx *knowledge.Taxonomy, kb KnowledgeOpener) SampleCheck {
	return func(s *Sample) []string {
		a := s.Expect.Advice
		if a == nil || (len(a.ProductIn) == 0 && a.Target == "") || s.KnowledgeRef == "" {
			return nil
		}
		snap, err := kb.Open(context.Background(), s.KnowledgeRef)
		if err != nil {
			return nil
		}
		if a.Target == "" {
			return []string{"expect.advice.target is required when product_in is set"}
		}
		var msgs []string
		for _, ref := range a.MustCite {
			id, _, err := knowledge.ParseCitationRef(ref)
			if err != nil {
				continue // must_cite_kb 已报
			}
			if e, ok := snap.Entry(id); ok && e.Target != "" && e.Target != a.Target {
				msgs = append(msgs, fmt.Sprintf("expect.advice.target %q differs from cited %s target %q", a.Target, id, e.Target))
			}
		}
		if len(a.ProductIn) == 0 {
			return msgs
		}
		c, err := calc.New(snap)
		if err != nil {
			return append(msgs, fmt.Sprintf("calc on knowledge_ref %s: %v", s.KnowledgeRef, err))
		}
		cands, err := c.Candidates(tx.Crop, a.Target, tx.Region)
		if err != nil {
			return append(msgs, fmt.Sprintf("product_in needs candidates: %v", err))
		}
		for i, p := range a.ProductIn {
			if !slices.Contains(cands, p) {
				msgs = append(msgs, fmt.Sprintf("expect.advice.product_in[%d] %q not in calc.Candidates(%s, %s, %s) = %v",
					i, p, tx.Crop, a.Target, tx.Region, cands))
			}
		}
		return msgs
	}
}
