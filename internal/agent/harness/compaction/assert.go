package compaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/model"
)

// ErrWhitelist 表示最终会话块的白名单字段缺失或与源表不一致（红线：放弃本次压缩）。
var ErrWhitelist = errors.New("compaction: whitelist assertion failed")

// Assert 是白名单断言（红线，P2 §6.7 技术实现 5）：解码最终组装进 prompt 的会话块字节，逐字段与 src
// （调用方从业务表重新读出的会话原料，不是组装时的 facts）比对。任一字段缺失、多出或取值不一致即返回
// 包装 ErrWhitelist 的错误，错误文本列出全部不一致的字段路径。
// 原文轮次只允许是源轮次的后缀；少于源轮次时必须带非空摘要（被压缩的部分由摘要承接）。
func Assert(block []byte, src model.SessionSource, lex *Lexicon) error {
	var b Block
	dec := json.NewDecoder(bytes.NewReader(block))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return fmt.Errorf("%w: decode block: %w", ErrWhitelist, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(block, &top); err != nil {
		return fmt.Errorf("%w: decode block: %w", ErrWhitelist, err)
	}
	if _, ok := top["facts"]; !ok {
		return fmt.Errorf("%w: facts missing", ErrWhitelist)
	}
	var c checker
	c.check(b.CompactionVersion == model.CompactionVersion, "compaction_version")
	c.facts(b.Facts, src, lex)
	c.turns(b, src.Turns)
	if len(c.bad) > 0 {
		return fmt.Errorf("%w: %s", ErrWhitelist, strings.Join(c.bad, ", "))
	}
	return nil
}

// checker 收集不一致的字段路径。
type checker struct{ bad []string }

func (c *checker) check(ok bool, path string, args ...any) {
	if !ok {
		c.bad = append(c.bad, fmt.Sprintf(path, args...))
	}
}

func (c *checker) facts(f model.CompactionFacts, src model.SessionSource, lex *Lexicon) {
	c.check(f.FieldID == src.FieldID, "facts.field_id")
	crop, stage := "", ""
	for _, card := range src.Cards {
		if card.Crop != "" {
			crop, stage = card.Crop, card.PhenologyStage
		}
	}
	c.check(f.Crop == crop, "facts.crop")
	c.check(f.PhenologyStage == stage, "facts.phenology_stage")
	c.constraints(f.UserConstraints, constraintTurns(src), lex)
	c.orders(f, src.Orders)
	c.check(slices.Equal(f.VetoedProducts, vetoesOf(src)), "facts.vetoed_products")
	c.measured(f.Measured, src.Measured)
	c.conclusions(f.ConfirmedConclusions, confirmedCards(src))
	want := uniqueSorted(src.CitedKBIDs)
	c.check(slices.Equal(f.CitedKBIDs, want), "facts.cited_kb_ids")
}

// constraints：每轮问题中命中词表的原句须逐条、按序出现，且原句是该轮问题的子串（未被改写）。
func (c *checker) constraints(got []model.UserConstraint, turns []model.SessionTurn, lex *Lexicon) {
	var want []model.UserConstraint
	question := map[string]string{}
	for _, t := range turns {
		id := model.TurnStepID(t.RunID)
		question[id] = t.Question
		for _, s := range lex.Constraints(t.Question) {
			want = append(want, model.UserConstraint{Text: s, TurnStepID: id})
		}
	}
	c.check(len(got) == len(want), "facts.user_constraints (len %d, want %d)", len(got), len(want))
	for i := range min(len(got), len(want)) {
		c.check(got[i] == want[i], "facts.user_constraints[%d]", i)
		c.check(strings.Contains(question[got[i].TurnStepID], got[i].Text), "facts.user_constraints[%d].text not verbatim", i)
	}
}

func (c *checker) orders(f model.CompactionFacts, src []model.SessionOrderSource) {
	c.check(len(f.Orders) == len(src), "facts.orders (len %d, want %d)", len(f.Orders), len(src))
	for i := range min(len(f.Orders), len(src)) {
		c.check(f.Orders[i] == src[i].SessionOrder, "facts.orders[%d] (order %d)", i, src[i].OrderID)
	}
}

func (c *checker) measured(got, want []model.MeasuredFact) {
	c.check(len(got) == len(want), "facts.measured (len %d, want %d)", len(got), len(want))
	for i := range min(len(got), len(want)) {
		g, w := got[i], want[i]
		c.check(g.ToolCallID == w.ToolCallID && g.ToolName == w.ToolName, "facts.measured[%d] (tool_call %d)", i, w.ToolCallID)
		c.check(sameJSON(g.ResultSummary, w.ResultSummary), "facts.measured[%d].result_summary", i)
		c.check(!g.CollectedAt.IsZero() && g.CollectedAt.Equal(w.CollectedAt), "facts.measured[%d].collected_at", i)
	}
}

// conclusions：可确认的卡（confirmedCards）逐张比对结论正文、剂量、药剂与窗口；
// 拒答卡与作业单已驳回 / 取消的卡不得出现。
func (c *checker) conclusions(got []model.ConfirmedConclusion, want []model.SessionCard) {
	c.check(len(got) == len(want), "facts.confirmed_conclusions (len %d, want %d)", len(got), len(want))
	for i := range min(len(got), len(want)) {
		g, w := got[i], want[i]
		c.check(g.AdviceID == w.AdviceID, "facts.confirmed_conclusions[%d].advice_id", i)
		c.check(g.Conclusion == w.Conclusion, "facts.confirmed_conclusions[%d].conclusion", i)
		c.check(slices.Equal(g.Numbers, w.Numbers), "facts.confirmed_conclusions[%d].numbers", i)
		c.check(g.SelectedProduct == w.SelectedProduct, "facts.confirmed_conclusions[%d].selected_product", i)
		c.check(sameWindow(g.Window, w.Window), "facts.confirmed_conclusions[%d].window", i)
	}
}

// turns：原文轮次须为源轮次的后缀且逐字一致；被省略的更早轮次须由非空摘要承接。
func (c *checker) turns(b Block, src []model.SessionTurn) {
	want := turnsOf(src)
	if len(b.Turns) > len(want) {
		c.check(false, "turns (len %d > %d)", len(b.Turns), len(want))
		return
	}
	tail := want[len(want)-len(b.Turns):]
	for i := range b.Turns {
		c.check(b.Turns[i] == tail[i], "turns[%d] (%s)", i, tail[i].StepID)
	}
	if len(b.Turns) < len(want) {
		c.check(b.Summary != nil && b.Summary.Text != "", "summary (turns omitted)")
	}
}

func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

func sameWindow(a, b *model.TimeWindow) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.From.Equal(b.From) && a.To.Equal(b.To)
}
