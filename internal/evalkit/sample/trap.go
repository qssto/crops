package sample

import (
	"fmt"
	"strings"
)

// RuleTrap 是陷阱组规则名（P2 L1-T）。
const RuleTrap = "trap"

// 陷阱组 tag（eval/taxonomy.yaml sample_tags）。
const (
	TrapSimilarPest = "trap.similar_pest" // 相似病虫害混淆
	TrapOverdose    = "trap.overdose"     // 超剂量诱导
	TrapCrossCrop   = "trap.cross_crop"   // 跨作物误用
	TrapStaleKB     = "trap.stale_kb"     // 过期知识引用
)

// trapPrefix 是陷阱组 tag 的公共前缀。
const trapPrefix = "trap."

// trapCorrection 是 outcome=ADVICE 时各陷阱组的「纠正结论」期望：返回空串表示已声明，否则为缺什么。
// 新增陷阱组时在这里追加一项即可（与 eval/taxonomy.yaml sample_tags 同步）。
var trapCorrection = map[string]func(e Expect) string{
	TrapSimilarPest: func(e Expect) string {
		if e.Advice != nil && strings.TrimSpace(e.Advice.Target) != "" {
			return ""
		}
		return "expect.advice.target (the corrected pest / disease)"
	},
	TrapOverdose: func(e Expect) string {
		if (e.MustNotContain != nil && len(e.MustNotContain.Values) > 0) || (e.Advice != nil && len(e.Advice.Numbers) > 0) {
			return ""
		}
		return "expect.must_not_contain.values (the induced dose) or expect.advice.numbers (the registered dose)"
	},
	TrapCrossCrop: func(e Expect) string {
		if (e.MustNotContain != nil && len(e.MustNotContain.Products) > 0) || (e.Advice != nil && len(e.Advice.ProductIn) > 0) {
			return ""
		}
		return "expect.must_not_contain.products (the other crop's product) or expect.advice.product_in"
	},
	TrapStaleKB: func(e Expect) string {
		if e.MustNotContain != nil && len(e.MustNotContain.Values) > 0 {
			return ""
		}
		return "expect.must_not_contain.values (the stale figure)"
	},
}

// TrapTags 返回样本的陷阱组 tag（按 tag 顺序）。
func TrapTags(s *Sample) []string {
	var out []string
	for _, tag := range s.Tags {
		if strings.HasPrefix(tag, trapPrefix) {
			out = append(out, tag)
		}
	}
	return out
}

// checkTrap：陷阱样本须声明期望拒答（outcome=REFUSED 且有 refuse_reason），或按陷阱组声明期望纠正结论；
// trap.* tag 须是已登记的陷阱组（未登记的 tag 由 tags 规则另报）。
func checkTrap(s *Sample) []string {
	var msgs []string
	refused := s.IsRefusal() && s.Expect.RefuseReason != ""
	for _, tag := range TrapTags(s) {
		correction, known := trapCorrection[tag]
		if !known {
			msgs = append(msgs, fmt.Sprintf("trap tag %q has no expectation rule", tag))
			continue
		}
		if refused {
			continue
		}
		if missing := correction(s.Expect); missing != "" {
			msgs = append(msgs, fmt.Sprintf("trap %s needs an expected refusal or corrective conclusion: %s", tag, missing))
		}
	}
	return msgs
}
