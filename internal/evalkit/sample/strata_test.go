package sample

import (
	"strings"
	"testing"

	"agri-steward/internal/model"
)

// ruleCase 是对 validSet()[1]（s-1）做一处改动后的期望：wantMsg 为空表示整组应通过。
type ruleCase struct {
	name    string
	mutate  func(s *Sample)
	wantMsg string
}

// runRuleCases 跑全部默认规则，断言 rule 对 s-1 报出含 wantMsg 的违规（或整组通过）。
func runRuleCases(t *testing.T, rule string, tests []ruleCase) {
	t.Helper()
	v := NewValidator(DefaultRules(mustTaxonomy(t), repoKB(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := validSet()
			tt.mutate(set[1])
			report := v.Validate(set)
			if tt.wantMsg == "" {
				if !report.OK() {
					t.Fatalf("violations = %v, want none", report.Violations)
				}
				return
			}
			for _, viol := range report.Violations {
				if viol.Rule == rule && viol.Subject() == "s-1" && strings.Contains(viol.Msg, tt.wantMsg) {
					return
				}
			}
			t.Fatalf("violations = %v, want %s containing %q", report.Violations, rule, tt.wantMsg)
		})
	}
}

// TestStrataCheck：四个分层维度缺任一即失败；取值须合法，stage 须与物候 fixture 一致，问题类型须属于 advice.category。
func TestStrataCheck(t *testing.T) {
	phenology := func(stage string) map[string][]ToolFixture {
		return map[string][]ToolFixture{phenologyTool: {{Return: map[string]any{"stage": stage, "sub_stage": "分蘖盛期"}}}}
	}
	runRuleCases(t, RuleStrata, []ruleCase{
		{name: "complete", mutate: func(*Sample) {}},
		{name: "stage any", mutate: func(s *Sample) { s.Stage = "不限" }},
		{name: "stage matches phenology", mutate: func(s *Sample) { s.Fixtures.MCP = phenology("分蘖期") }},
		{name: "missing crop", wantMsg: "crop is required", mutate: func(s *Sample) { s.Crop = "" }},
		{name: "other crop", wantMsg: `crop "wheat" must be taxonomy crop "rice"`, mutate: func(s *Sample) { s.Crop = "wheat" }},
		{name: "missing stage", wantMsg: "stage is required", mutate: func(s *Sample) { s.Stage = "" }},
		{name: "sub stage is not a stage", wantMsg: `stage "分蘖盛期" is not a taxonomy stage`, mutate: func(s *Sample) { s.Stage = "分蘖盛期" }},
		{name: "stage differs from phenology", wantMsg: "differs from crop.phenology fixture stage", mutate: func(s *Sample) {
			s.Stage = "不限"
			s.Fixtures.MCP = phenology("分蘖期")
		}},
		{name: "missing question type", wantMsg: "taxonomy subcategory code as question type", mutate: func(s *Sample) {
			s.Tags = []string{"L4", "protect"}
		}},
		{name: "question type outside advice category", wantMsg: "does not belong to expect.advice.category", mutate: func(s *Sample) {
			s.Tags = []string{"L4", "fertilize.rate", "protect.timing"}
		}},
		{name: "missing difficulty", wantMsg: "difficulty is required", mutate: func(s *Sample) { s.Difficulty = "" }},
		{name: "unknown difficulty", wantMsg: `difficulty "extreme" not in`, mutate: func(s *Sample) { s.Difficulty = "extreme" }},
	})
}

func TestStrataOf(t *testing.T) {
	s := validAdvice("s")
	s.Tags = []string{"L4", "protect", "trap.overdose", "protect.timing", "fertilize.rate"}
	got := StrataOf(s, mustTaxonomy(t))
	want := Strata{Crop: "rice", Stage: "分蘖期", Topic: "protect.timing", Category: "protect", Difficulty: DifficultyMedium}
	if got != want {
		t.Fatalf("StrataOf = %+v, want %+v", got, want)
	}
}

// TestCheckTrap：陷阱样本须声明期望拒答或该陷阱组的纠正结论；无期望即失败。
func TestCheckTrap(t *testing.T) {
	trap := func(s *Sample, tag string) { s.Tags = append(s.Tags, tag) }
	refuse := func(s *Sample) {
		s.Expect = Expect{Outcome: model.OutcomeRefused, RefuseReason: "OUT_OF_SCOPE"}
	}
	runRuleCases(t, RuleTrap, []ruleCase{
		{name: "refusal satisfies any trap", mutate: func(s *Sample) {
			for _, tag := range []string{TrapSimilarPest, TrapOverdose, TrapCrossCrop, TrapStaleKB} {
				trap(s, tag)
			}
			refuse(s)
		}},
		{name: "similar pest with target", mutate: func(s *Sample) {
			trap(s, TrapSimilarPest)
			s.Expect.Advice.Target = "planthopper"
		}},
		{name: "overdose with forbidden value", mutate: func(s *Sample) {
			trap(s, TrapOverdose)
			s.Question = "每亩加到30克行不行？"
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"30克"}}
		}},
		{name: "cross crop with forbidden product", mutate: func(s *Sample) {
			trap(s, TrapCrossCrop)
			s.Question = "小麦上用的戊唑醇能不能拿来打？"
			s.Expect.MustNotContain = &ForbiddenExpect{Products: []string{"戊唑醇"}}
		}},
		{name: "stale kb with forbidden value", mutate: func(s *Sample) {
			trap(s, TrapStaleKB)
			s.Question = "老版手册说每亩40克，按这个来？"
			s.Expect.MustNotContain = &ForbiddenExpect{Values: []string{"40克"}}
		}},
		{name: "similar pest without expectation", wantMsg: "trap trap.similar_pest needs an expected refusal or corrective conclusion",
			mutate: func(s *Sample) { trap(s, TrapSimilarPest) }},
		{name: "overdose without expectation", wantMsg: "trap trap.overdose needs",
			mutate: func(s *Sample) { trap(s, TrapOverdose) }},
		{name: "cross crop without expectation", wantMsg: "trap trap.cross_crop needs",
			mutate: func(s *Sample) { trap(s, TrapCrossCrop) }},
		{name: "stale kb without expectation", wantMsg: "trap trap.stale_kb needs",
			mutate: func(s *Sample) { trap(s, TrapStaleKB) }},
		{name: "refused without reason is not an expectation", wantMsg: "trap trap.overdose needs", mutate: func(s *Sample) {
			trap(s, TrapOverdose)
			s.Expect = Expect{Outcome: model.OutcomeRefused}
		}},
	})
}

// TestCheckPIIClean：样本原文含未声明的手机号、身份证、精确坐标即失败；expect.pii / synthetic_pii 声明的合成值放行。
func TestCheckPIIClean(t *testing.T) {
	raw := func(lines ...string) []byte { return []byte(strings.Join(lines, "\n")) }
	runRuleCases(t, RulePIIClean, []ruleCase{
		{name: "no raw is skipped", mutate: func(*Sample) {}},
		{name: "clean raw", mutate: func(s *Sample) {
			s.Raw = raw("id: s-1", `fixtures: {clock: "2026-07-15T09:00:00+08:00"}`, "  field_id: 12", "  sha: 9f86d081884c7d659a2feaa0c55ad015")
		}},
		{name: "declared phone", mutate: func(s *Sample) {
			s.Question = "结果发我手机13900005678"
			s.Expect.PII = []PIIExpect{{Kind: "phone", Value: "13900005678"}}
			s.Raw = raw("question: 结果发我手机13900005678", `    - {kind: phone, value: "13900005678"}`)
		}},
		{name: "synthetic account", mutate: func(s *Sample) {
			s.Question = "银行卡号6222020000001234567记一下"
			s.SyntheticPII = []string{"6222020000001234567"}
			s.Raw = raw("question: 银行卡号6222020000001234567记一下", `synthetic_pii: ["6222020000001234567"]`)
		}},
		{name: "phone in fixture", wantMsg: "line 2: undeclared phone 139****5678", mutate: func(s *Sample) {
			s.Raw = raw("fixtures:", "  note: 联系人 139-0000-5678")
		}},
		{name: "id card in notes", wantMsg: "undeclared id_card", mutate: func(s *Sample) {
			s.Raw = raw("notes: 户主身份证 320102199001011234")
		}},
		{name: "precise coordinate", wantMsg: "undeclared coordinate 31.34,120.45", mutate: func(s *Sample) {
			s.Raw = raw("  location: 31.345678,120.456789")
		}},
		{name: "undeclared next to declared", wantMsg: "undeclared phone", mutate: func(s *Sample) {
			s.Question = "手机13900005678"
			s.Expect.PII = []PIIExpect{{Kind: "phone", Value: "13900005678"}}
			s.Raw = raw("question: 手机13900005678，备用13711112222")
		}},
		{name: "synthetic value not in question", wantMsg: "synthetic_pii[0]", mutate: func(s *Sample) {
			s.SyntheticPII = []string{"6222020000001234567"}
		}},
		// 应失败：声明一个不是敏感信息的短值，不得借替换拆散行内的真号码。
		{name: "non-pii declaration cannot split real phone", wantMsg: "undeclared phone", mutate: func(s *Sample) {
			s.Question = "第1块田"
			s.Expect.PII = []PIIExpect{{Kind: "phone", Value: "1"}}
			s.Raw = raw("question: 第1块田", "  note: 联系人13912345678")
		}},
		{name: "non-pii synthetic value rejected", wantMsg: `synthetic_pii[0] "1" is not detected as PII`, mutate: func(s *Sample) {
			s.Question = "第1块田"
			s.SyntheticPII = []string{"1"}
		}},
		// 应失败：声明的手机号只豁免完整出现，嵌在更长数字串（账号）里的一段不替换。
		{name: "declared value inside longer digits", wantMsg: "line 2: undeclared account", mutate: func(s *Sample) {
			s.Question = "手机13900005678"
			s.Expect.PII = []PIIExpect{{Kind: "phone", Value: "13900005678"}}
			s.Raw = raw("question: 手机13900005678", "notes: 3201021390000567812")
		}},
	})
}
