package guardrail

import (
	"strings"
	"testing"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/knowledge"
)

// testMentions 是测试用的原文药名规则：词表含分组表与一个小登记表，候选为吡蚜酮、噻虫嗪。
func testMentions() *ProductMentions {
	reg := &knowledge.Registry{Products: []knowledge.Product{
		{ID: "pymetrozine-50wg", CommonName: "吡蚜酮"}, {ID: "jinggangmycin-5as", CommonName: "井冈霉素"},
	}}
	return &ProductMentions{Lexicon: calc.NewLexicon(reg), Allowed: []string{"吡蚜酮", "噻虫嗪"}}
}

// TestValidate_ProductMentionOutsideCandidates（P2 L4 覆盖检查，红线）：card 上的 selected_product 在候选内、
// 模型原文却建议候选外的药剂（改用、混入、点名推荐）时拒收（check=product）；候选内药剂、否定句、
// 只提及不建议、历史记录回显放行。Mentions 为 nil 时不查原文药名（P0-P1 行为不变）。
func TestValidate_ProductMentionOutsideCandidates(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		text    string
		history []string
		reject  bool
	}{
		{name: "switch to outside product", text: "建议改用毒死蜱", reject: true},
		{name: "outside product after colon", text: "建议药剂：吡虫啉", reject: true},
		{name: "outside alias", text: "可以用康宽防治", reject: true},
		{name: "mix in outside product", text: "吡蚜酮可与井冈霉素桶混兼治纹枯病", reject: true},
		{name: "candidate product", text: "建议药剂：吡蚜酮"},
		{name: "candidate after verb", text: "选用噻虫嗪也可以"},
		{name: "negated", text: "不要用毒死蜱"},
		{name: "negated connector chain", text: "切勿与吡虫啉、毒死蜱混用"},
		{name: "mentioned not recommended", text: "近期用过吡虫啉，需轮换作用机理"},
		{name: "injected history echo", text: "历史记录：建议药剂：吡虫啉", history: []string{"历史记录：建议药剂：吡虫啉"}},
		// P2 L4 审查修复：目的从句不否定推荐；单字作业动词算推荐；模型自写的「历史记录」照常校验。
		{name: "purpose clause 以避免", text: "建议使用吡虫啉以避免稻飞虱暴发", reject: true},
		{name: "purpose clause 防止", text: "喷施毒死蜱防止螟虫扩散", reject: true},
		{name: "single verb 喷", text: "明天喷吡虫啉", reject: true},
		{name: "single verb 打一遍", text: "打一遍吡虫啉", reject: true},
		{name: "single verb 用", text: "用康宽", reject: true},
		{name: "self-written history", text: "历史记录：建议药剂：吡虫啉", reject: true},
		{name: "history differs from injected", text: "历史记录：建议药剂：吡虫啉",
			history: []string{"历史记录：建议药剂：吡蚜酮"}, reject: true},
		{name: "normalized outside product", text: "建议喷吡​虫啉", reject: true},
		{name: "single char 防", text: "吡虫啉防飞虱", reject: true},
		{name: "not in candidates statement", text: "小麦拌种用的毒死蜱不在水稻登记候选内"},
		{name: "purpose tail 减少药害", text: "建议使用吡虫啉以减少药害", reject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			in.Mentions = testMentions()
			in.ModelText = append(in.ModelText, tt.text)
			in.History = tt.history
			got := v.Validate(in)
			if !tt.reject {
				if got != nil {
					t.Fatalf("want pass, got %v", got)
				}
				return
			}
			if got == nil || got.Check != CheckProduct || !strings.Contains(got.Detail, "outside candidates") {
				t.Fatalf("got %v, want %s outside candidates", got, CheckProduct)
			}
		})
	}
	in := validInput()
	in.ModelText = append(in.ModelText, "建议改用毒死蜱")
	if got := v.Validate(in); got != nil {
		t.Fatalf("nil Mentions should skip, got %v", got)
	}
}

// TestValidate_ProductMentionNoCandidates（P2 L4 审查修复）：规则层无药剂候选（非施药作业类）时 Allowed 为空，
// 原文肯定推荐任何药剂都拒收；只陈述观察、否定句放行。
func TestValidate_ProductMentionNoCandidates(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	base := func(text string) Input {
		in := validInput()
		in.Card.SelectedProduct, in.Card.ProductCandidates, in.Candidates, in.RequireProduct = "", nil, nil, false
		in.ModelText = []string{"继续观察虫情", text}
		m := testMentions()
		m.Allowed = nil
		in.Mentions = m
		return in
	}
	for _, text := range []string{"建议喷吡蚜酮", "可以用井冈霉素"} {
		if got := v.Validate(base(text)); got == nil || got.Check != CheckProduct {
			t.Errorf("%q: got %v, want product rejection", text, got)
		}
	}
	for _, text := range []string{"虫量未达指标，暂不用药", "不建议喷吡蚜酮", "田间稻飞虱数量较少"} {
		if got := v.Validate(base(text)); got != nil {
			t.Errorf("%q: want pass, got %v", text, got)
		}
	}
}
