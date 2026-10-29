// Package guardrail 是输出护栏（P0 §6.6 功能任务 5-6）：OutputValidator 在建议卡落库前按
// schema → 引用 → 数值 → 药剂 顺序校验，任一失败即返回 Violation，由主链路转 VALIDATION_FAILED 拒答，
// 不修不补、不重试。脱敏规则在子包 pii。
package guardrail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"agri-steward/contracts"
	"agri-steward/internal/agent/calc"
	"agri-steward/internal/model"
)

// Check 是校验项（VALIDATION_FAILED 事件 payload 的 check 字段）。
type Check string

const (
	CheckSchema   Check = "schema"
	CheckCitation Check = "citation"
	CheckNumber   Check = "number"
	CheckProduct  Check = "product"
)

// Violation 是第一处不合规。
type Violation struct {
	Check  Check
	Detail string
}

func (v *Violation) Error() string { return "guardrail: " + string(v.Check) + ": " + v.Detail }

func violation(c Check, format string, a ...any) *Violation {
	return &Violation{Check: c, Detail: fmt.Sprintf(format, a...)}
}

// Knowledge 是本次 run 的知识快照（*knowledge.Snapshot 满足）。
type Knowledge interface {
	Has(kbID, version string) bool
	Deprecated(kbID string) bool
	Ref() string
}

// Number 是一个 calc 结果（与 card.Numbers 按下标一一对应）。
type Number struct {
	Field  string
	Result calc.Result
}

// Input 是一次校验的输入。
type Input struct {
	Card *model.AdviceCard
	// Numbers 是 calc 结果：card.Numbers 必须逐一等于其舍入值与单位，且每个都有 calc trace。
	Numbers []Number
	// Candidates 是规则层给出的药剂候选集合（不取卡片上的 product_candidates，防止一起被篡改）。
	Candidates []string
	// ModelText 是模型原文（结论模板与理由）：只允许 {{nK}} 占位符引用 Numbers，不得裸写数字。
	ModelText []string
	// RequireProduct：施药作业类且非降级时必须选定药剂。
	RequireProduct bool
	KB             Knowledge
}

// OutputValidator 构造后只读，并发安全。
type OutputValidator struct {
	card *jsonschema.Schema
}

// New 从内嵌契约编译 advice_card schema（$ref citation）。
func New() (*OutputValidator, error) {
	comp := jsonschema.NewCompiler()
	comp.AssertFormat()
	var cardID string
	for _, file := range []string{contracts.AdviceCardFile, contracts.CitationFile} {
		b, err := contracts.FS.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("guardrail: read %s: %w", file, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("guardrail: parse %s: %w", file, err)
		}
		m, isObj := doc.(map[string]any)
		id, isStr := m["$id"].(string)
		if !isObj || !isStr || id == "" {
			return nil, fmt.Errorf("guardrail: %s has no $id", file)
		}
		if err := comp.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("guardrail: add %s: %w", file, err)
		}
		if file == contracts.AdviceCardFile {
			cardID = id
		}
	}
	sch, err := comp.Compile(cardID)
	if err != nil {
		return nil, fmt.Errorf("guardrail: compile %s: %w", contracts.AdviceCardFile, err)
	}
	return &OutputValidator{card: sch}, nil
}

// Validate 按 schema → 引用 → 数值 → 药剂 顺序校验，返回第一处不合规；nil 表示通过。
// 拒答卡只做 schema 校验。
func (v *OutputValidator) Validate(in Input) *Violation {
	if in.Card == nil {
		return violation(CheckSchema, "no advice card")
	}
	if p := v.schema(in.Card); p != nil {
		return p
	}
	if in.Card.Outcome != model.OutcomeAdvice {
		return nil
	}
	for _, check := range []func(Input) *Violation{citations, numbers, product} {
		if p := check(in); p != nil {
			return p
		}
	}
	return nil
}

// schema 校验卡片 JSON。validate 在 persist 之前运行，此时 advice_id 尚未分配（0），
// 契约要求 ≥ 1，故以副本把 0 置为 1 后校验（其余字段原样）。
func (v *OutputValidator) schema(c *model.AdviceCard) *Violation {
	cp := *c
	if cp.AdviceID == 0 {
		cp.AdviceID = 1
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return violation(CheckSchema, "card not encodable: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return violation(CheckSchema, "card not decodable: %v", err)
	}
	if err := v.card.Validate(doc); err != nil {
		return violation(CheckSchema, "%v", err)
	}
	return nil
}

// claimConclusion 是空 claim_path 归属的字段（与 citation_valid 打分器一致）。
const claimConclusion = "conclusion"

// citations：每个结论字段（conclusion、reasons[i]、numbers[i]）≥ 1 条引用且引用不指向不存在的字段；
// kb 引用 kb_id@version 在本次知识快照中存在且未废弃；实测引用必须带 tool_call_id。
func citations(in Input) *Violation {
	c := in.Card
	if len(c.Citations) == 0 {
		return violation(CheckCitation, "advice has no citation")
	}
	if in.KB == nil {
		return violation(CheckCitation, "no knowledge snapshot to check citations against")
	}
	fields := claimFields(c)
	covered := make(map[string]bool, len(fields))
	for i, ct := range c.Citations {
		path := ct.ClaimPath
		if path == "" {
			path = claimConclusion
		}
		if !slices.Contains(fields, path) {
			return violation(CheckCitation, "citations[%d] claim_path %q points to no field", i, path)
		}
		covered[path] = true
		switch ct.Type {
		case model.CitationKB:
			if ct.KBID == "" || ct.KBVersion == "" || !in.KB.Has(ct.KBID, ct.KBVersion) || in.KB.Deprecated(ct.KBID) {
				return violation(CheckCitation, "citations[%d] %s@%s not valid in knowledge %s", i, ct.KBID, ct.KBVersion, in.KB.Ref())
			}
		case model.CitationMeasured:
			if ct.ToolCallID == nil || *ct.ToolCallID == 0 {
				return violation(CheckCitation, "citations[%d] measured without tool_call_id", i)
			}
		default:
			return violation(CheckCitation, "citations[%d] unknown type %q", i, ct.Type)
		}
	}
	for _, f := range fields {
		if !covered[f] {
			return violation(CheckCitation, "%s has no citation", f)
		}
	}
	return nil
}

// claimFields 按卡片顺序列出结论字段。
func claimFields(c *model.AdviceCard) []string {
	var out []string
	if c.Conclusion != "" {
		out = append(out, claimConclusion)
	}
	for i := range c.Reasons {
		out = append(out, fmt.Sprintf("reasons[%d]", i))
	}
	for i := range c.Numbers {
		out = append(out, fmt.Sprintf("numbers[%d]", i))
	}
	return out
}

var (
	// placeholderRe 匹配 {{nK}} 占位符（与 graph.fill 一致）。
	placeholderRe = regexp.MustCompile(`\{\{n(\d+)\}\}`)
	// bareDigitRe 匹配模型原文里的数字（含全角、小数、百分号）；graph 生成交给模型的无数字摘要也用它（RemoveDigits）。
	bareDigitRe = regexp.MustCompile(`[0-9０-９]+(?:\.[0-9]+)?%?`)
)

// RemoveDigits 删去文本中 numbers 校验会判为裸写数字的片段（graph 生成交给模型的无数字摘要用，与校验同一口径）。
func RemoveDigits(s string) string { return bareDigitRe.ReplaceAllString(s, "") }

// numbers：模型原文占位符不越界、无裸写数字；每个 calc 结果有 trace 且有限；card.Numbers 与 calc
// 结果逐一相等（字段、舍入值、单位），篡改或多出 / 缺少即失败。
func numbers(in Input) *Violation {
	for _, t := range in.ModelText {
		for _, m := range placeholderRe.FindAllStringSubmatch(t, -1) {
			if i, err := strconv.Atoi(m[1]); err != nil || i >= len(in.Numbers) {
				return violation(CheckNumber, "placeholder %s has no number", m[0])
			}
		}
		if bare := bareDigitRe.FindString(placeholderRe.ReplaceAllString(t, "")); bare != "" {
			return violation(CheckNumber, "model text has bare number %s", bare)
		}
	}
	for _, n := range in.Numbers {
		if n.Result.Trace.Formula == "" || !finite(n.Result.Value) {
			return violation(CheckNumber, "number %s has no calc trace", n.Field)
		}
	}
	got := in.Card.Numbers
	if len(got) != len(in.Numbers) {
		return violation(CheckNumber, "card has %d numbers, calc produced %d", len(got), len(in.Numbers))
	}
	for i, n := range in.Numbers {
		want := model.Number{Field: n.Field, Value: n.Result.Rounded(), Unit: n.Result.Unit}
		if got[i] != want {
			return violation(CheckNumber, "numbers[%d] = %s %v %s, calc says %s %v %s",
				i, got[i].Field, got[i].Value, got[i].Unit, want.Field, want.Value, want.Unit)
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// product：卡片候选 ⊆ 规则层候选；selected_product ∈ 规则层候选；施药作业类必须选定药剂。
func product(in Input) *Violation {
	c := in.Card
	for _, p := range c.ProductCandidates {
		if !slices.Contains(in.Candidates, p) {
			return violation(CheckProduct, "candidate %s not from rule layer", p)
		}
	}
	if c.SelectedProduct != "" && !slices.Contains(in.Candidates, c.SelectedProduct) {
		return violation(CheckProduct, "selected product %s not in candidates", c.SelectedProduct)
	}
	if in.RequireProduct && c.SelectedProduct == "" {
		return violation(CheckProduct, "spray operation without selected product")
	}
	return nil
}
