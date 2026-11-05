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
	"agri-steward/internal/model"
)

// Material 是 judge 的全部输入。只有这四类（加维度名）：fixture、所引知识条目原文、专家标准结论、最终输出；
// 不含工具调用、事件时间线、日志、transcript 等中间步骤。Fixture 由调用方序列化（离线是样本 fixture，
// 线上是提问与情境），本包只按原样参与哈希。
type Material struct {
	Dimension string          `json:"dimension"`
	Fixture   json.RawMessage `json:"fixture"`
	Knowledge []KBText        `json:"knowledge"`
	Expected  Expected        `json:"expected"`
	Output    FinalOutput     `json:"output"`
}

// KBText 是最终输出引用的一条知识条目原文（kb_id@version 的 front-matter + 正文 JSON）；Missing 表示该版本不存在。
type KBText struct {
	KBID    string          `json:"kb_id"`
	Version string          `json:"version"`
	Body    json.RawMessage `json:"body,omitempty"`
	Missing bool            `json:"missing,omitempty"`
}

// Expected 是专家标准结论（样本 expect，经专家签字）；线上抽检没有标准结论，为零值。
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

// EntryLoader 按 kb_id@version 读条目原文（*knowledge.Snapshot 实现）。
type EntryLoader interface {
	Load(ctx context.Context, kbID, version string) (agent.KnowledgeEntry, error)
}

// ErrNoKnowledge 表示输出引了知识条目但没有可读原文的知识版本。
var ErrNoKnowledge = errors.New("judge: trial has citations but no readable knowledge snapshot")

// SetCard 把卡片的对用户可见部分与所引知识原文写进材料（c 为 nil 时不动）。kb 为 nil 而卡片引了知识返回 ErrNoKnowledge。
func (m *Material) SetCard(ctx context.Context, kb EntryLoader, c *model.AdviceCard) error {
	if c == nil {
		return nil
	}
	m.Output.Conclusion, m.Output.Reasons = c.Conclusion, c.Reasons
	m.Output.SelectedProduct, m.Output.Numbers = c.SelectedProduct, c.Numbers
	texts, err := CitedKnowledge(ctx, kb, c.Citations)
	if err != nil {
		return err
	}
	m.Knowledge = texts
	for _, k := range texts {
		m.Output.Citations = append(m.Output.Citations, k.KBID+"@"+k.Version)
	}
	return nil
}

// CitedKnowledge 按引用顺序读知识原文（同一 kb_id@version 只读一次）；不存在的版本记 Missing。
func CitedKnowledge(ctx context.Context, kb EntryLoader, cites []model.Citation) ([]KBText, error) {
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
		if kb == nil {
			return nil, ErrNoKnowledge
		}
		e, err := kb.Load(ctx, ct.KBID, ct.KBVersion)
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
