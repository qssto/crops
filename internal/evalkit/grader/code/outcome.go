package code

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// OutcomeMatch 是终态打分器 outcome_match（非红线）：τ-bench 式终态比对，忽略 ID 与时间戳。
// 比对 outcome；拒答再比 refuse_reason；建议再比 category、advice_type（样本写了才比）。
// 有卡以卡为准，无卡时用 agent.Answer 的 outcome / refuse_reason。
type OutcomeMatch struct{}

var _ grader.Grader = OutcomeMatch{}

// Name 返回打分器名。
func (OutcomeMatch) Name() string { return "outcome_match" }

// RedLine 返回 false。
func (OutcomeMatch) RedLine() bool { return false }

// Grade 打分；Value 为已比对字段中一致的比例。
func (OutcomeMatch) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("outcome_match: trial or sample is nil")
	}
	exp := t.Sample.Expect
	outcome, reason := t.Outcome, t.RefuseReason
	var category, adviceType string
	if c := t.Card; c != nil {
		outcome, reason, category, adviceType = c.Outcome, c.RefuseReason, string(c.Category), string(c.AdviceType)
	}
	var fails []string
	checked := 0
	cmp := func(field, got, want string) {
		if want == "" {
			return
		}
		checked++
		if got != want {
			fails = append(fails, fmt.Sprintf("%s = %q, want %q", field, got, want))
		}
	}
	cmp("outcome", string(outcome), string(exp.Outcome))
	if outcome == exp.Outcome {
		switch exp.Outcome {
		case model.OutcomeRefused:
			cmp("refuse_reason", reason, exp.RefuseReason)
		case model.OutcomeAdvice:
			if a := exp.Advice; a != nil {
				cmp("category", category, a.Category)
				cmp("advice_type", adviceType, a.AdviceType)
			}
		}
	}
	value := 1.0
	if checked > 0 {
		value = float64(checked-len(fails)) / float64(checked)
	}
	return grader.Score{Pass: len(fails) == 0, Value: value, Evidence: strings.Join(fails, "; ")}, nil
}
