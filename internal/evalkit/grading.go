package evalkit

import (
	"context"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/grader/code"
	"agri-steward/internal/evalkit/sample"
)

// trial 状态（agri_eval.trial.status 取值）。
const (
	StatusPass        = "PASS"
	StatusFail        = "FAIL"
	StatusInfraError  = "INFRA_ERROR"
	StatusGraderError = "GRADER_ERROR"
)

// Score 是 trial 一个打分维度的结果。Error 非空即 grader_error：既不算通过也不算失败，必须显式报告。
type Score struct {
	Name     string
	RedLine  bool
	Pass     bool
	Value    float64
	Unknown  bool
	Evidence string
	Error    string
}

// Status 判定 trial 状态：运行出错为 INFRA_ERROR；任一打分器出错为 GRADER_ERROR；
// 有打分且全部维度通过为 PASS，否则 FAIL（未打分不算通过）。
func (t Trial) Status() string {
	if t.Error != "" {
		return StatusInfraError
	}
	if len(t.Scores) == 0 {
		return StatusFail
	}
	pass := true
	for _, sc := range t.Scores {
		if sc.Error != "" {
			return StatusGraderError
		}
		pass = pass && sc.Pass
	}
	if pass {
		return StatusPass
	}
	return StatusFail
}

// ScoreOf 按维度名取打分；不存在返回 false。
func (t Trial) ScoreOf(name string) (Score, bool) {
	for _, sc := range t.Scores {
		if sc.Name == name {
			return sc, true
		}
	}
	return Score{}, false
}

// grading 是 Runner 的打分配置。kb 为 nil 时 trial 不带知识版本（需要它的打分器记 grader_error）。
type grading struct {
	graders []grader.Grader
	kb      sample.KnowledgeOpener
}

// defaultGraders 是未配置打分器时的维度：只比 outcome（与 L1 行为一致）。
func defaultGraders() []grader.Grader { return []grader.Grader{code.OutcomeMatch{}} }

// openKB 打开样本 knowledge_ref；未配置 opener 时返回 nil。
func (g grading) openKB(ctx context.Context, s *sample.Sample) (grader.KnowledgeIndex, error) {
	if g.kb == nil {
		return nil, nil
	}
	snap, err := g.kb.Open(ctx, s.KnowledgeRef)
	if err != nil {
		return nil, fmt.Errorf("open knowledge_ref %q: %w", s.KnowledgeRef, err)
	}
	return snap, nil
}

// grade 对一次回答跑全部打分器；打分器报错包装为 "grader <name> on <id>#<i>: ..." 记入 Score.Error。
func (g grading) grade(ctx context.Context, t *Trial, s *sample.Sample, ans agent.Answer, kb grader.KnowledgeIndex) {
	gt := &grader.Trial{Sample: s, Index: t.TrialIndex, Outcome: ans.Outcome, RefuseReason: ans.RefuseReason,
		Card: ans.Card, KB: kb, ToolCalls: t.ToolCalls, ToolFailures: t.ToolFailures,
		Approval: t.Approval, DeviceCalls: t.DeviceCalls, Logs: t.Logs, Stored: t.Stored, OrderTransitions: t.OrderTransitions}
	t.Scores = make([]Score, 0, len(g.graders))
	for _, gr := range g.graders {
		sc := Score{Name: gr.Name(), RedLine: gr.RedLine()}
		res, err := gr.Grade(ctx, gt)
		if err != nil {
			sc.Error = fmt.Errorf("grader %s on %s#%d: %w", gr.Name(), s.ID, t.TrialIndex, err).Error()
		} else {
			sc.Pass, sc.Value, sc.Unknown, sc.Evidence = res.Pass, res.Value, res.Unknown, res.Evidence
		}
		t.Scores = append(t.Scores, sc)
	}
}
