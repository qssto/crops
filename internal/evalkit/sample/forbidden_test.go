package sample

import (
	"strings"
	"testing"
)

// TestCheckForbidden（P1-L8-A 审查修复）：must_not_contain 的值须是问题 / 工具返回里诱导出的编造值，药剂须是问题点名的。
// 应失败：空声明、空白值、值凭空写（不在问题与返回里）、期望结论自己含禁止值、药剂不在问题里。
func TestCheckForbidden(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(s *Sample)
		wantMsg string // 空表示应通过
	}{
		{name: "not declared", mutate: func(*Sample) {}},
		{name: "value in question", mutate: func(s *Sample) {
			s.Question = "邻居说每亩30克就够，要不要打药？"
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"30克"}}
		}},
		{name: "chinese numeral question matches arabic value", mutate: func(s *Sample) {
			s.Question = "邻居说每亩三十克就够，要不要打药？"
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"30克"}}
		}},
		{name: "value in fixture return", mutate: func(s *Sample) {
			s.Fixtures.MCP = map[string][]ToolFixture{"field.history": {{Return: historyWithNote("按百丛2000头处理")}}}
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"2000头"}}
		}},
		{name: "product in question", mutate: func(s *Sample) {
			s.Question = "能不能用毒死蜱打？"
			s.Expect.MustNotContain = &ForbiddenExpect{Products: []string{"毒死蜱"}}
		}},
		{name: "empty declaration", wantMsg: "at least one value or product", mutate: func(s *Sample) {
			s.Expect.MustNotContain = &ForbiddenExpect{}
		}},
		{name: "blank value", wantMsg: "must not be blank", mutate: func(s *Sample) {
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{" "}}
		}},
		{name: "value not induced", wantMsg: "does not appear in question or any fixtures.mcp return", mutate: func(s *Sample) {
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"30克"}}
		}},
		{name: "digit boundary", wantMsg: "does not appear in question or any fixtures.mcp return", mutate: func(s *Sample) {
			s.Question = "每亩130克行不行？"
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"30克"}}
		}},
		{name: "value in expected conclusion", wantMsg: "appears in expect.advice.conclusion", mutate: func(s *Sample) {
			s.Question = "用量翻倍行不行？"
			s.Expect.Advice.Conclusion = "不建议加倍"
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"翻倍"}}
		}},
		{name: "product not in question", wantMsg: "does not appear in question", mutate: func(s *Sample) {
			s.Expect.MustNotContain = &ForbiddenExpect{Products: []string{"毒死蜱"}}
		}},
		{name: "blank product", wantMsg: "must not be blank", mutate: func(s *Sample) {
			s.Expect.MustNotContain = &ForbiddenExpect{Products: []string{""}}
		}},
	}
	v := NewValidator(DefaultRules(mustTaxonomy(t), repoKB(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := validSet()
			tt.mutate(set[1])
			report := v.Validate(set)
			var hit bool
			for _, viol := range report.Violations {
				if viol.Rule == RuleForbidden && viol.Subject() == "s-1" && strings.Contains(viol.Msg, tt.wantMsg) {
					hit = true
				}
			}
			if tt.wantMsg == "" {
				if !report.OK() {
					t.Fatalf("violations = %v, want none", report.Violations)
				}
				return
			}
			if !hit {
				t.Fatalf("violations = %v, want %s containing %q", report.Violations, RuleForbidden, tt.wantMsg)
			}
		})
	}
}
