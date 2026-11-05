package sample

import (
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/model"
)

// RuleConflict 校验 P2 L4 的 expect.conflict（conflict_blocked 使用）。
const RuleConflict = "conflict_shape"

// ConflictKindTankMix 是混用禁忌冲突（P2 L4 下沉项 tank_mix）。P3 L5 conflict_checker 在此追加间隔冲突等类别。
const ConflictKindTankMix = string(model.RuleItemTankMix)

// ConflictKinds 是 expect.conflict.kind 的取值。
var ConflictKinds = []string{ConflictKindTankMix}

// ConflictExpect 断言方案冲突必拦（P2 L4 conflict_blocked，红线）：Kind 是冲突类别；Products 是问题诱导同桶的药剂
// （通用名，按问题原文写），须按 calc.TankMix 构成至少一组禁忌。输出卡片不得肯定地建议这组混配。
type ConflictExpect struct {
	Kind     string   `yaml:"kind"`
	Products []string `yaml:"products"`
}

// checkConflict：声明了 expect.conflict 时 kind 取 ConflictKinds；tank_mix 至少两种药剂、各自出现在 question 中、
// 按混用禁忌表构成禁忌组合（样本测的是「必拦」，不构成禁忌的组合测不出红线）。
func checkConflict(s *Sample) []string {
	c := s.Expect.Conflict
	if c == nil {
		return nil
	}
	if !slices.Contains(ConflictKinds, c.Kind) {
		return []string{fmt.Sprintf("expect.conflict.kind %q is not one of %v", c.Kind, ConflictKinds)}
	}
	var msgs []string
	if len(c.Products) < 2 {
		msgs = append(msgs, "expect.conflict.products needs at least two products")
	}
	for i, p := range c.Products {
		if !strings.Contains(s.Question, p) {
			msgs = append(msgs, fmt.Sprintf("expect.conflict.products[%d] %q does not appear in question", i, p))
		}
	}
	if len(c.Products) >= 2 && len(calc.TankMix(c.Products, 0).Forbidden) == 0 {
		msgs = append(msgs, fmt.Sprintf("expect.conflict.products %v form no forbidden tank mix", c.Products))
	}
	return msgs
}
