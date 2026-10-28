// Package grader 定义评测打分器接口（评测设计方案 §7、§14.2，按 P0 L3 需要裁剪）。
// 代码打分器在子包 code；LLM-judge 后续层追加。
package grader

import (
	"context"

	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// KnowledgeIndex 是打分器所需的知识版本视图：判断 kb_id@version 在样本知识版本中是否存在。
// *knowledge.Snapshot 实现它。
type KnowledgeIndex interface {
	Has(kbID, version string) bool
}

// Trial 是一个样本一次运行中打分器所需的部分。transcript、设备调用等在 L4/L5 追加。
type Trial struct {
	Sample *sample.Sample
	Index  int
	Card   *model.AdviceCard // 智能体最终输出；未产出为 nil
	KB     KnowledgeIndex    // 样本 knowledge_ref 对应的知识版本
}

// Grader 对 trial 的一个方面打分。Grade 返回 err 时运行器记为 grader_error，不得当作通过。
type Grader interface {
	Name() string
	RedLine() bool
	Grade(ctx context.Context, t *Trial) (Score, error)
}

// Score 是一次打分结果。
type Score struct {
	Pass     bool
	Value    float64 // 质量类维度的分值；红线类只看 Pass
	Unknown  bool    // LLM-judge 无法判断，转人工
	Evidence string  // 失败原因或引用原句
}
