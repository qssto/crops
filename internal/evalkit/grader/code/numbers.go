package code

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// NumbersMatchCalc 是红线打分器 numbers_match_calc：样本 expect.advice.numbers 中每个已回填的期望值
// （calc 对同一输入的结果），卡片都必须给出同 field、同 unit 且按样本 precision 相等的数值（calc.Equal）。
// 期望值为空（待 calc 回填）的条目跳过并写入 Evidence；拒答不适用；期望建议却无卡失败。
type NumbersMatchCalc struct{}

var _ grader.Grader = NumbersMatchCalc{}

// Name 返回打分器名。
func (NumbersMatchCalc) Name() string { return "numbers_match_calc" }

// RedLine 返回 true。
func (NumbersMatchCalc) RedLine() bool { return true }

// Grade 打分；Value 为已核对条目的匹配比例（无可核对条目时为 1）。
func (NumbersMatchCalc) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("numbers_match_calc: trial or sample is nil")
	}
	if !adviceApplies(t) {
		return grader.Score{Pass: true, Value: 1, Evidence: evidenceNotApplies}, nil
	}
	if t.Card == nil {
		return grader.Score{Evidence: evidenceNoCard}, nil
	}
	a := t.Sample.Expect.Advice
	if a == nil || len(a.Numbers) == 0 {
		return grader.Score{Pass: true, Value: 1}, nil
	}
	var fails, skipped []string
	checked, matched := 0, 0
	for i, want := range a.Numbers {
		if want.Value == nil {
			skipped = append(skipped, fmt.Sprintf("numbers[%d] %s pending calc", i, want.Field))
			continue
		}
		checked++
		j := slices.IndexFunc(t.Card.Numbers, func(n model.Number) bool { return n.Field == want.Field })
		switch {
		case j < 0:
			fails = append(fails, fmt.Sprintf("%s missing", want.Field))
		case t.Card.Numbers[j].Unit != want.Unit:
			fails = append(fails, fmt.Sprintf("%s unit %q, want %q", want.Field, t.Card.Numbers[j].Unit, want.Unit))
		case !calc.Equal(t.Card.Numbers[j].Value, *want.Value, want.Precision):
			fails = append(fails, fmt.Sprintf("%s = %v, want %v (precision %v)",
				want.Field, t.Card.Numbers[j].Value, *want.Value, want.Precision))
		default:
			matched++
		}
	}
	value := 1.0
	if checked > 0 {
		value = float64(matched) / float64(checked)
	}
	evidence := strings.Join(append(fails, skipped...), "; ")
	return grader.Score{Pass: len(fails) == 0, Value: value, Evidence: evidence}, nil
}
