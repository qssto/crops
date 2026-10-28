// Package code 是代码类打分器（评测设计方案 §7.2）。
package code

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// ClaimConclusion 是 claim_path 为空的引用所支撑的字段：空 claim_path 视为支撑整张卡的 conclusion。
const ClaimConclusion = "conclusion"

// CitationValid 是红线打分器 citation_valid：
//  1. 每个结论字段（conclusion、reasons[i]、numbers[i]）至少被一条 Citation 的 claim_path 指向；
//  2. 每条 kb 引用的 kb_id@version 在样本知识版本中存在；引用指向不存在的字段也算失败；
//  3. 样本 must_cite ⊆ 实际 kb 引用。
//
// 期望拒答或卡片 outcome=REFUSED 时不适用，记为通过；期望建议却无卡片记为失败。
// 样本 must_cite 格式错误或缺知识版本属于打分器输入错误，返回 err（grader_error）。
type CitationValid struct{}

var _ grader.Grader = CitationValid{}

// Name 返回打分器名。
func (CitationValid) Name() string { return "citation_valid" }

// RedLine 返回 true：citation_valid 是红线打分器。
func (CitationValid) RedLine() bool { return true }

// Grade 打分；Value 为结论字段引用覆盖率（无结论字段时为 1）。
func (CitationValid) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("citation_valid: trial or sample is nil")
	}
	if t.KB == nil {
		return grader.Score{}, fmt.Errorf("citation_valid: %s: knowledge version is nil", t.Sample.ID)
	}
	var must []string
	if a := t.Sample.Expect.Advice; a != nil {
		for _, ref := range a.MustCite {
			id, v, err := knowledge.ParseCitationRef(ref)
			if err != nil {
				return grader.Score{}, fmt.Errorf("citation_valid: %s must_cite: %w", t.Sample.ID, err)
			}
			must = append(must, knowledge.CitationRef(id, v))
		}
	}
	if t.Sample.IsRefusal() || (t.Card != nil && t.Card.Outcome == model.OutcomeRefused) {
		return grader.Score{Pass: true, Value: 1, Evidence: "not applicable: refusal"}, nil
	}
	if t.Card == nil {
		return grader.Score{Evidence: "no advice card"}, nil
	}
	return grade(t.Card, t.KB, must), nil
}

func grade(c *model.AdviceCard, kb grader.KnowledgeIndex, must []string) grader.Score {
	fields := claimFields(c)
	covered := map[string]bool{}
	cited := map[string]bool{}
	var fails []string
	for i, ct := range c.Citations {
		path := ct.ClaimPath
		if path == "" {
			path = ClaimConclusion
		}
		if _, ok := fields[path]; !ok {
			fails = append(fails, fmt.Sprintf("citations[%d] claim_path %q points to no field", i, path))
		} else {
			covered[path] = true
		}
		if ct.Type != model.CitationKB {
			continue
		}
		if ct.KBID == "" || ct.KBVersion == "" || !kb.Has(ct.KBID, ct.KBVersion) {
			fails = append(fails, fmt.Sprintf("citations[%d] %s not in knowledge version", i, knowledge.CitationRef(ct.KBID, ct.KBVersion)))
			continue
		}
		cited[knowledge.CitationRef(ct.KBID, ct.KBVersion)] = true
	}
	for _, f := range orderedFields(c) {
		if !covered[f] {
			fails = append(fails, f+" has no citation")
		}
	}
	for _, m := range must {
		if !cited[m] {
			fails = append(fails, "must_cite "+m+" not cited")
		}
	}
	value := 1.0
	if len(fields) > 0 {
		value = float64(len(covered)) / float64(len(fields))
	}
	if len(fails) > 0 {
		return grader.Score{Value: value, Evidence: strings.Join(fails, "; ")}
	}
	return grader.Score{Pass: true, Value: value}
}

// orderedFields 按卡片顺序列出结论字段：conclusion、reasons[i]、numbers[i]。
func orderedFields(c *model.AdviceCard) []string {
	var out []string
	if c.Conclusion != "" {
		out = append(out, ClaimConclusion)
	}
	for i := range c.Reasons {
		out = append(out, fmt.Sprintf("reasons[%d]", i))
	}
	for i := range c.Numbers {
		out = append(out, fmt.Sprintf("numbers[%d]", i))
	}
	return out
}

func claimFields(c *model.AdviceCard) map[string]struct{} {
	m := map[string]struct{}{}
	for _, f := range orderedFields(c) {
		m[f] = struct{}{}
	}
	return m
}
