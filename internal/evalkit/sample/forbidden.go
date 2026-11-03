package sample

import (
	"fmt"
	"strings"

	"agri-steward/internal/evalkit/textnorm"
)

// RuleForbidden 校验 P1-L8-A 审查修复的 expect.must_not_contain（forbidden_text 使用）。
const RuleForbidden = "must_not_contain_shape"

// checkForbidden：声明了就至少一条且不得为空白；values 须（按数字归一）出现在 question 或某个 fixtures.mcp 返回中
// （是被诱导的编造值，而不是随手写的字符串），且不得出现在 expect.advice.conclusion 中（期望不能自相矛盾）；
// products 须出现在 question 中（问题点名的候选外药剂）。
func checkForbidden(s *Sample) []string {
	f := s.Expect.MustNotContain
	if f == nil {
		return nil
	}
	var msgs []string
	if len(f.Values)+len(f.Products) == 0 {
		msgs = append(msgs, "expect.must_not_contain needs at least one value or product")
	}
	for i, v := range f.Values {
		where := fmt.Sprintf("expect.must_not_contain.values[%d]", i)
		switch {
		case strings.TrimSpace(v) == "":
			msgs = append(msgs, where+" must not be blank")
			continue
		case !textnorm.ContainsValue(s.Question, v) && !valueInFixtureReturns(s, v):
			msgs = append(msgs, fmt.Sprintf("%s %q does not appear in question or any fixtures.mcp return", where, v))
		}
		if a := s.Expect.Advice; a != nil && textnorm.ContainsValue(a.Conclusion, v) {
			msgs = append(msgs, fmt.Sprintf("%s %q appears in expect.advice.conclusion", where, v))
		}
	}
	for i, p := range f.Products {
		where := fmt.Sprintf("expect.must_not_contain.products[%d]", i)
		switch {
		case strings.TrimSpace(p) == "":
			msgs = append(msgs, where+" must not be blank")
		case !strings.Contains(s.Question, p):
			msgs = append(msgs, fmt.Sprintf("%s %q does not appear in question", where, p))
		}
	}
	return msgs
}

// valueInFixtureReturns 报告 v 是否（按数字归一、带边界）出现在某个桩返回的字符串叶子中。
func valueInFixtureReturns(s *Sample, v string) bool {
	for _, tool := range fixtureTools(s) {
		for _, f := range s.Fixtures.MCP[tool] {
			for _, nv := range fixtureValues(f) {
				if anyString(nv.v, func(str string) bool { return textnorm.ContainsValue(str, v) }) {
					return true
				}
			}
		}
	}
	return false
}

// anyString 递归遍历 YAML 解码值的字符串叶子（不含 map 键），有一个满足 pred 即返回 true。
func anyString(v any, pred func(string) bool) bool {
	switch x := v.(type) {
	case string:
		return pred(x)
	case map[string]any:
		for _, e := range x {
			if anyString(e, pred) {
				return true
			}
		}
	case map[any]any:
		for _, e := range x {
			if anyString(e, pred) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if anyString(e, pred) {
				return true
			}
		}
	}
	return false
}
