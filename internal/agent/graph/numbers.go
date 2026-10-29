package graph

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/guardrail"
	"agri-steward/internal/agent/knowledge"
)

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// factDecimals 是理由文本里实测值的显示小数位。派生事实（如由时间差折算的调查时效天数）
// 不做显示截断会写出 0.7083333333333334 这类长数字串，既难读，又会被脱敏规则当作账号形态。
const factDecimals = 2

// fmtFact 把实测值按 factDecimals 位小数显示（去掉末尾 0）；阈值判定仍用原值，只影响理由文本。
func fmtFact(v float64) string {
	return fmtNum(math.Round(v*math.Pow10(factDecimals)) / math.Pow10(factDecimals))
}

func joinNums(vals []float64, unit string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmtFact(v)
	}
	return "[" + strings.Join(parts, ", ") + "] " + unit
}

// noDigits 去掉文本中的数字（与 guardrail numbers 校验同一口径），保证交给 explain 的摘要不含数字。
func noDigits(s string) string {
	return strings.Join(strings.Fields(guardrail.RemoveDigits(s)), " ")
}

// addNumber 追加一个数值（Field 唯一，重复时保留先到的）并引用其知识条目。
func (s *state) addNumber(field, label, kb string, r calc.Result) {
	if slices.ContainsFunc(s.numbers, func(n number) bool { return n.Field == field }) {
		return
	}
	s.numbers = append(s.numbers, number{Field: field, Label: label, KB: kb, Result: r})
	s.dec.cite(kb)
}

// param 取条目的无条件参数，缺失即 KB_MISS。
func (e *env) param(en *knowledge.Entry, id string) (knowledge.Param, error) {
	p, ok := en.Param(id, nil)
	if !ok {
		return p, refuse(RefuseKBMiss, "param %s missing in %s", id, en.Ref())
	}
	return p, nil
}

// paramResult 把知识库参数值包装为带 trace 的数值（trace 指向参数出处）。
func paramResult(en *knowledge.Entry, p knowledge.Param, bound string, v, precision float64) calc.Result {
	return calc.Result{Value: v, Unit: p.Unit, Precision: precision, Trace: calc.Trace{
		Inputs:  map[string]any{"param": p.ID, "bound": bound},
		Formula: en.FormulaRef(p.ID),
	}}
}

// paramRange 把区间参数输出为 <field>_min / <field>_max 两个数值。
func (e *env) paramRange(s *state, en *knowledge.Entry, id, field, label string, precision float64) error {
	p, err := e.param(en, id)
	if err != nil {
		return err
	}
	if p.Min == nil || p.Max == nil {
		return refuse(RefuseKBMiss, "param %s in %s has no range", id, en.Ref())
	}
	s.addNumber(field+"_min", label+"下限", en.KBID, paramResult(en, p, "min", *p.Min, precision))
	s.addNumber(field+"_max", label+"上限", en.KBID, paramResult(en, p, "max", *p.Max, precision))
	return nil
}

// paramPoint 把定值参数输出为一个数值。
func (e *env) paramPoint(s *state, en *knowledge.Entry, id, field, label string, precision float64) error {
	p, err := e.param(en, id)
	if err != nil {
		return err
	}
	if p.Value == nil {
		return refuse(RefuseKBMiss, "param %s in %s has no value", id, en.Ref())
	}
	s.addNumber(field, label, en.KBID, paramResult(en, p, "value", *p.Value, precision))
	return nil
}

// scaled 把一个数值换算为另一个（如亩用量 × 面积），trace 记录来源数值与系数。
func scaled(src calc.Result, factor float64, unit, op string, precision float64) calc.Result {
	return calc.Result{Value: src.Value * factor, Unit: unit, Precision: precision, Trace: calc.Trace{
		Inputs:  map[string]any{"value": src.Value, "unit": src.Unit, "factor": factor},
		Formula: src.Trace.Formula + " " + op,
	}}
}
