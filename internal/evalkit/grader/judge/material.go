// Package judge 是 LLM-judge 打分器（P2 L2，路线 §6.3）：fact_consistency / conclusion_match / explanation_quality
// 各一个，实现 grader.Grader，输出 {score 1-3, evidence, verdict}。judge 只看材料（fixture、所引知识条目原文、
// 专家标准结论、最终输出），不看中间推理；模型经 Client 调用，现为录制回放（待真实模型）。
// 材料哈希、evidence 校验、录制回放与版本哈希在 service/quality/judge（P2 L3 下沉，线上抽检共用），本包包装复用。
package judge

import (
	"context"
	"encoding/json"
	"fmt"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	qjudge "agri-steward/internal/service/quality/judge"
)

// Material 是 judge 的全部输入，见 qjudge.Material；离线 Fixture 为本包 Fixture 的 JSON。
type Material = qjudge.Material

// KBText 是最终输出引用的一条知识条目原文。
type KBText = qjudge.KBText

// Expected 是专家标准结论（样本 expect，经专家签字）。
type Expected = qjudge.Expected

// FinalOutput 是智能体最终输出（AdviceCard 的对用户可见部分）。
type FinalOutput = qjudge.FinalOutput

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

// ErrNoKnowledge 表示输出引了知识条目但 trial 没带可读原文的知识版本。
var ErrNoKnowledge = qjudge.ErrNoKnowledge

// BuildMaterial 从 trial 组装 dim 维度的材料。只读 Sample（fixture 与 expect）、Outcome / RefuseReason / Card 与 KB。
func BuildMaterial(ctx context.Context, dim string, t *grader.Trial) (Material, error) {
	s := t.Sample
	fx, err := json.Marshal(Fixture{Question: s.Question, Crop: s.Crop, Stage: s.Stage, Context: s.Context,
		Clock: s.Fixtures.Clock, MCP: s.Fixtures.MCP, Memory: s.Fixtures.Memory})
	if err != nil {
		return Material{}, fmt.Errorf("judge: marshal fixture: %w", err)
	}
	m := Material{Dimension: dim, Fixture: fx,
		Expected: Expected{Outcome: s.Expect.Outcome, RefuseReason: s.Expect.RefuseReason},
		Output:   FinalOutput{Outcome: t.Outcome, RefuseReason: t.RefuseReason}}
	if a := s.Expect.Advice; a != nil {
		m.Expected.Category, m.Expected.AdviceType, m.Expected.Conclusion = a.Category, a.AdviceType, a.Conclusion
	}
	var loader qjudge.EntryLoader
	if l, ok := t.KB.(qjudge.EntryLoader); ok {
		loader = l
	}
	if err := m.SetCard(ctx, loader, t.Card); err != nil {
		return Material{}, err
	}
	return m, nil
}
