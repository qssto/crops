// Package judge 是 LLM-judge 打分器（P2 L2，路线 §6.3）：fact_consistency / conclusion_match / explanation_quality
// 各一个，实现 grader.Grader，输出 {score 1-3, evidence, verdict}。judge 只看材料（fixture、所引知识条目原文、
// 专家标准结论、最终输出），不看中间推理；模型经 Client 调用，现为录制回放（待真实模型）。
package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// Material 是 judge 的全部输入。只有这四类（加维度名）：fixture、所引知识条目原文、专家标准结论、最终输出；
// 不含工具调用、事件时间线、日志、transcript 等中间步骤（单测断言）。
type Material struct {
	Dimension string      `json:"dimension"`
	Fixture   Fixture     `json:"fixture"`
	Knowledge []KBText    `json:"knowledge"`
	Expected  Expected    `json:"expected"`
	Output    FinalOutput `json:"output"`
}

// Fixture 是样本给定的情境：提问、身份上下文、时钟与预置的工具返回 / 记忆。
type Fixture struct {
	Question string                          `json:"question"`
	Crop     string                          `json:"crop,omitempty"`
	Stage    string                          `json:"stage,omitempty"`
	Context  sample.Context                  `json:"context"`
	Clock    string                          `json:"clock,omitempty"`
	MCP      map[string][]sample.ToolFixture `json:"mcp,omitempty"`
	Memory   map[string]any                  `json:"memory,omitempty"`
}

// KBText 是最终输出引用的一条知识条目原文（kb_id@version 的 front-matter + 正文 JSON）；Missing 表示该版本不存在。
type KBText struct {
	KBID    string          `json:"kb_id"`
	Version string          `json:"version"`
	Body    json.RawMessage `json:"body,omitempty"`
	Missing bool            `json:"missing,omitempty"`
}

// Expected 是专家标准结论（样本 expect，经专家签字）。
type Expected struct {
	Outcome      model.Outcome `json:"outcome"`
	RefuseReason string        `json:"refuse_reason,omitempty"`
	Category     string        `json:"category,omitempty"`
	AdviceType   string        `json:"advice_type,omitempty"`
	Conclusion   string        `json:"conclusion,omitempty"`
}

// FinalOutput 是智能体最终输出（AdviceCard 的对用户可见部分；未出卡时只有 outcome / refuse_reason）。
type FinalOutput struct {
	Outcome         model.Outcome  `json:"outcome"`
	RefuseReason    string         `json:"refuse_reason,omitempty"`
	Conclusion      string         `json:"conclusion,omitempty"`
	Reasons         []string       `json:"reasons,omitempty"`
	SelectedProduct string         `json:"selected_product,omitempty"`
	Numbers         []model.Number `json:"numbers,omitempty"`
	Citations       []string       `json:"citations,omitempty"` // kb_id@version
}

// entryLoader 按 kb_id@version 读条目原文（*knowledge.Snapshot 实现）。
type entryLoader interface {
	Load(ctx context.Context, kbID, version string) (agent.KnowledgeEntry, error)
}

// ErrNoKnowledge 表示输出引了知识条目但 trial 没带可读原文的知识版本。
var ErrNoKnowledge = errors.New("judge: trial has citations but no readable knowledge snapshot")

// BuildMaterial 从 trial 组装 dim 维度的材料。只读 Sample（fixture 与 expect）、Outcome / RefuseReason / Card 与 KB。
func BuildMaterial(ctx context.Context, dim string, t *grader.Trial) (Material, error) {
	s := t.Sample
	m := Material{Dimension: dim,
		Fixture: Fixture{Question: s.Question, Crop: s.Crop, Stage: s.Stage, Context: s.Context,
			Clock: s.Fixtures.Clock, MCP: s.Fixtures.MCP, Memory: s.Fixtures.Memory},
		Expected: Expected{Outcome: s.Expect.Outcome, RefuseReason: s.Expect.RefuseReason},
		Output:   FinalOutput{Outcome: t.Outcome, RefuseReason: t.RefuseReason}}
	if a := s.Expect.Advice; a != nil {
		m.Expected.Category, m.Expected.AdviceType, m.Expected.Conclusion = a.Category, a.AdviceType, a.Conclusion
	}
	c := t.Card
	if c == nil {
		return m, nil
	}
	m.Output.Conclusion, m.Output.Reasons = c.Conclusion, c.Reasons
	m.Output.SelectedProduct, m.Output.Numbers = c.SelectedProduct, c.Numbers
	kb, err := citedKnowledge(ctx, t.KB, c.Citations)
	if err != nil {
		return Material{}, err
	}
	m.Knowledge = kb
	for _, k := range kb {
		m.Output.Citations = append(m.Output.Citations, k.KBID+"@"+k.Version)
	}
	return m, nil
}

// citedKnowledge 按引用顺序读知识原文（同一 kb_id@version 只读一次）；不存在的版本记 Missing。
func citedKnowledge(ctx context.Context, idx grader.KnowledgeIndex, cites []model.Citation) ([]KBText, error) {
	var out []KBText
	seen := map[string]bool{}
	for _, ct := range cites {
		if ct.KBID == "" {
			continue
		}
		key := ct.KBID + "@" + ct.KBVersion
		if seen[key] {
			continue
		}
		seen[key] = true
		loader, ok := idx.(entryLoader)
		if idx == nil || !ok {
			return nil, ErrNoKnowledge
		}
		e, err := loader.Load(ctx, ct.KBID, ct.KBVersion)
		switch {
		case errors.Is(err, knowledge.ErrNotFound):
			out = append(out, KBText{KBID: ct.KBID, Version: ct.KBVersion, Missing: true})
		case err != nil:
			return nil, fmt.Errorf("judge: load %s: %w", key, err)
		default:
			out = append(out, KBText{KBID: ct.KBID, Version: ct.KBVersion, Body: e.Body})
		}
	}
	return out, nil
}

// Hash 是材料的内容哈希（规范 JSON 的 sha256 hex），录制回放按 (维度, Hash) 取录制。
func (m Material) Hash() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("judge: marshal material: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// EvaluatedText 是被评文本（evidence 必须是它的原句）：最终输出的结论、理由与拒答原因。
func (m Material) EvaluatedText() string {
	parts := append([]string{m.Output.Conclusion}, m.Output.Reasons...)
	return strings.Join(append(parts, m.Output.RefuseReason), "\n")
}
