package calc

import (
	"slices"
	"testing"

	"agri-steward/internal/agent/knowledge"
)

// TestTankMix：禁忌组合（氰氟草酯 × 激素类、碱性 × 遇碱分解、同一作用机理两药）与单桶产品数上限。
func TestTankMix(t *testing.T) {
	tests := []struct {
		name      string
		products  []string
		max       int
		forbidden []string // MixPair.Rule
		overLimit bool
	}{
		{name: "cyhalofop with hormone", products: []string{"氰氟草酯", "二甲四氯钠"}, forbidden: []string{MixRuleCyhalofopHormone}},
		{name: "hormone first", products: []string{"氯氟吡氧乙酸", "氰氟草酯"}, forbidden: []string{MixRuleCyhalofopHormone}},
		{name: "alkaline with alkali-labile", products: []string{"石硫合剂", "敌敌畏"}, forbidden: []string{MixRuleAlkaline}},
		{name: "same moa dmi", products: []string{"己唑醇", "咪鲜胺"}, forbidden: []string{MixRuleSameMoA}},
		{name: "same moa via compound", products: []string{"肟菌·戊唑醇", "己唑醇"}, forbidden: []string{MixRuleSameMoA}},
		{name: "same product twice is not a pair", products: []string{"己唑醇", "己唑醇"}},
		{name: "different groups allowed", products: []string{"吡蚜酮", "井冈霉素"}},
		{name: "unknown products allowed", products: []string{"某药", "另一药"}},
		{name: "within limit", products: []string{"吡蚜酮", "井冈霉素", "三环唑"}, max: 3},
		{name: "over limit", products: []string{"吡蚜酮", "井冈霉素", "三环唑", "茚虫威"}, max: 3, overLimit: true},
		{name: "no limit when max 0", products: []string{"吡蚜酮", "井冈霉素", "三环唑", "茚虫威"}},
		{name: "forbidden and over limit", products: []string{"氰氟草酯", "二甲四氯", "吡蚜酮", "三环唑"}, max: 3,
			forbidden: []string{MixRuleCyhalofopHormone}, overLimit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := TankMix(tt.products, tt.max)
			var rules []string
			for _, p := range v.Forbidden {
				rules = append(rules, p.Rule)
			}
			if !slices.Equal(rules, tt.forbidden) || v.OverLimit != tt.overLimit {
				t.Fatalf("got forbidden=%v overLimit=%v, want %v %v", rules, v.OverLimit, tt.forbidden, tt.overLimit)
			}
			if v.OK() != (len(tt.forbidden) == 0 && !tt.overLimit) {
				t.Fatalf("OK() = %v inconsistent", v.OK())
			}
		})
	}
}

// TestTankMix_Deterministic：结果与入参顺序无关（影子比对 rule_output 稳定）。
func TestTankMix_Deterministic(t *testing.T) {
	a := TankMix([]string{"咪鲜胺", "氰氟草酯", "己唑醇", "二甲四氯"}, 3)
	b := TankMix([]string{"二甲四氯", "己唑醇", "氰氟草酯", "咪鲜胺"}, 3)
	if !slices.Equal(a.Products, b.Products) || !slices.Equal(a.Forbidden, b.Forbidden) {
		t.Fatalf("order dependent: %+v vs %+v", a, b)
	}
}

// TestLexicon_Find：最长匹配、不重叠，别名归一到通用名；登记表名称与别名并入。
func TestLexicon_Find(t *testing.T) {
	reg := &knowledge.Registry{Products: []knowledge.Product{
		{ID: "x-1", CommonName: "肟菌·戊唑醇", Aliases: []string{"拿敌稳"}},
		{ID: "x-2", CommonName: "吡蚜酮"},
	}}
	lex := NewLexicon(reg)
	tests := []struct {
		text string
		want []string
	}{
		{text: "拿敌稳与己唑醇桶混", want: []string{"肟菌·戊唑醇", "己唑醇"}},
		{text: "肟菌·戊唑醇", want: []string{"肟菌·戊唑醇"}}, // 不再拆出「戊唑醇」
		{text: "二甲四氯钠", want: []string{"二甲四氯钠"}},
		{text: "康宽加吡蚜酮", want: []string{"氯虫苯甲酰胺", "吡蚜酮"}},
		{text: "吡蚜酮吡蚜酮", want: []string{"吡蚜酮", "吡蚜酮"}},
		{text: "无药名", want: nil},
		// 归一化：空格、全角空格、零宽字符、NFKC 兼容字符都不影响匹配。
		{text: "氰 氟草酯", want: []string{"氰氟草酯"}},
		{text: "氰\u3000氟草酯", want: []string{"氰氟草酯"}},
		{text: "氰\u200b氟\u200d草\ufeff酯", want: []string{"氰氟草酯"}},
		{text: "二甲\u00ad四氯", want: []string{"二甲四氯"}},
	}
	for _, tt := range tests {
		var got []string
		for _, m := range lex.Find(tt.text) {
			got = append(got, m.Canonical)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("Find(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
	if c, ok := lex.Canonical("康宽"); !ok || c != "氯虫苯甲酰胺" {
		t.Errorf("Canonical(康宽) = %q %v", c, ok)
	}
	if c, ok := lex.Canonical("康 宽"); !ok || c != "氯虫苯甲酰胺" {
		t.Errorf("Canonical(康 宽) = %q %v", c, ok)
	}
	if got := NormalizeText("ＡＢ１ 2\u200b"); got != "AB12" {
		t.Errorf("NormalizeText = %q, want AB12", got)
	}
	if NewLexicon(nil).Find("氰氟草酯") == nil {
		t.Error("table-only lexicon misses 氰氟草酯")
	}
}

// TestForbiddenPartners：注入用的「所选药剂不得与谁混用」只列表内同组 / 禁忌组的其他产品。
func TestForbiddenPartners(t *testing.T) {
	got := ForbiddenPartners("氰氟草酯")
	for _, want := range []string{"二甲四氯", "二甲四氯钠", "氯氟吡氧乙酸", "麦草畏"} {
		if !slices.Contains(got, want) {
			t.Errorf("ForbiddenPartners(氰氟草酯) missing %s: %v", want, got)
		}
	}
	if got := ForbiddenPartners("己唑醇"); slices.Contains(got, "己唑醇") || !slices.Contains(got, "咪鲜胺") {
		t.Errorf("ForbiddenPartners(己唑醇) = %v", got)
	}
	if got := ForbiddenPartners("吡蚜酮"); len(got) != 0 {
		t.Errorf("ForbiddenPartners(吡蚜酮) = %v, want none", got)
	}
}
