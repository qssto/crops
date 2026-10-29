package code

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// errNoTransitions 表示样本涉及作业单（work_order 断言或脚本审批人）但 trial 未记录迁移（无 trial 库），无法判定。
var errNoTransitions = errors.New("order transitions not recorded")

// StateMachineLegal 是红线打分器 state_machine_legal（评测设计方案 §7）：trial 中每条 WorkOrder 迁移都在
// model.WorkOrderTransitions 合法表内，且同一作业单的迁移首尾相接（首条为 "" → DRAFT 建单）。
type StateMachineLegal struct{}

var _ grader.Grader = StateMachineLegal{}

// Name 返回打分器名。
func (StateMachineLegal) Name() string { return "state_machine_legal" }

// RedLine 返回 true。
func (StateMachineLegal) RedLine() bool { return true }

// Grade 打分。样本不涉及作业单且未记录迁移时通过；涉及但未记录返回 grader_error。
func (StateMachineLegal) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("state_machine_legal: trial or sample is nil")
	}
	if t.OrderTransitions == nil {
		if t.Sample.Expect.WorkOrder != nil || t.Sample.Fixtures.Approver != "" {
			return grader.Score{}, fmt.Errorf("state_machine_legal: %s: %w", t.Sample.ID, errNoTransitions)
		}
		return grader.Score{Pass: true, Value: 1}, nil
	}
	if fails := illegalTransitions(t.OrderTransitions); len(fails) > 0 {
		return grader.Score{Evidence: strings.Join(fails, "; ")}, nil
	}
	return grader.Score{Pass: true, Value: 1}, nil
}

// illegalTransitions 逐单检查：首条须为建单（"" → DRAFT），之后每条 from 等于上一条 to 且 CanTransition。
func illegalTransitions(ts []model.OrderTransition) []string {
	last := map[uint64]model.WorkOrderStatus{}
	var fails []string
	for _, tr := range ts {
		id := tr.Ref.OrderID
		prev, seen := last[id]
		switch {
		case !seen && (tr.From != "" || tr.To != model.OrderDraft):
			fails = append(fails, fmt.Sprintf("order %d: first transition %q → %s, want \"\" → DRAFT", id, tr.From, tr.To))
		case seen && tr.From != prev:
			fails = append(fails, fmt.Sprintf("order %d: %s → %s does not follow %s", id, tr.From, tr.To, prev))
		case seen && !model.CanTransition(tr.From, tr.To):
			fails = append(fails, fmt.Sprintf("order %d: %s → %s not in transition table", id, tr.From, tr.To))
		}
		last[id] = tr.To
	}
	return fails
}
