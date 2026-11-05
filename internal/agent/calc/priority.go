package calc

import (
	"cmp"
	"slices"
	"strings"
)

// 多病虫害并发时的优先级排序（P2 L4 下沉项 pest_priority）：规则层按各防治对象条目阈值的超标倍数排序，
// output_validator 拒收模型「优先防治 X」与规则排序首位不一致的结论（合入即常开）。

// PriorityRuleVer 是优先级排序规则版本（rule_shadow_diff.rule_ver）。
const PriorityRuleVer = "pest-prio-v1"

// PestLoad 是一个防治对象的阈值判定：Met 为达到防治指标，TooLate 为过了防治适期（只做观察），
// Exceed 为实测值 / 防治指标（同一对象多条阈值取最大）。
type PestLoad struct {
	Target  string  `json:"target"`
	Met     bool    `json:"met"`
	TooLate bool    `json:"too_late,omitempty"`
	Exceed  float64 `json:"exceed"`
}

// PestPriority 返回应优先防治的对象顺序：只含达标且未过晚的对象，超标倍数降序，相同时按 id 升序。
// 同一对象出现多次时合并（Met / TooLate 取或，Exceed 取最大）。
func PestPriority(loads []PestLoad) []string {
	merged := map[string]PestLoad{}
	for _, l := range loads {
		m, ok := merged[l.Target]
		if !ok {
			merged[l.Target] = l
			continue
		}
		m.Met, m.TooLate, m.Exceed = m.Met || l.Met, m.TooLate || l.TooLate, max(m.Exceed, l.Exceed)
		merged[l.Target] = m
	}
	var ranked []PestLoad
	for _, l := range merged {
		if l.Met && !l.TooLate {
			ranked = append(ranked, l)
		}
	}
	slices.SortFunc(ranked, func(a, b PestLoad) int {
		if c := cmp.Compare(b.Exceed, a.Exceed); c != 0 {
			return c
		}
		return cmp.Compare(a.Target, b.Target)
	})
	var out []string
	for _, l := range ranked {
		out = append(out, l.Target)
	}
	return out
}

// TargetNames 把登记表 targets 的写法拆成可在原文中识别的名称：主名加全角括号内顿号分隔的别名，
// 如「稻飞虱（褐飞虱、白背飞虱）」→ [稻飞虱 褐飞虱 白背飞虱]。
func TargetNames(label string) []string {
	main, rest, _ := strings.Cut(label, "（")
	inner, _, _ := strings.Cut(rest, "）")
	var out []string
	for _, n := range append([]string{main}, strings.Split(inner, "、")...) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
