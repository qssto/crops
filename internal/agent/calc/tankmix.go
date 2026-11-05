package calc

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"agri-steward/internal/agent/knowledge"
)

// 混用禁忌（P2 L4 下沉项 tank_mix，红线）：规则层按 common/safety/mixing 的三条「不混用」判定，
// output_validator 据此拒收模型结论中的禁忌混用（合入即常开，不受 rule_offload.tank_mix 开关影响）。
//
// 作用机理分组表是 Claude 草拟（author: claude-draft，review_status: pending_expert），待专家按 FRAC / HRAC / IRAC
// 编码核对；表外产品不归组，只计入单桶产品数。P3 L5 conflict_checker 复用本表与 TankMix，不另起一套。

// TankMixRuleVer 是混用禁忌规则版本（rule_shadow_diff.rule_ver，≤16 字符）。
const TankMixRuleVer = "tank-mix-v1"

// 禁忌规则名（MixPair.Rule）。
const (
	MixRuleCyhalofopHormone = "cyhalofop_hormone" // 氰氟草酯与激素类阔叶除草剂
	MixRuleAlkaline         = "alkaline_labile"   // 碱性药剂与遇碱分解的药剂
	MixRuleSameMoA          = "same_moa"          // 同一作用机理的两种药剂
)

// mixGroup 是作用机理 / 酸碱性分组。
type mixGroup string

const (
	groupDMI          mixGroup = "dmi"           // 三唑类等甾醇脱甲基抑制剂
	groupALS          mixGroup = "als"           // 乙酰乳酸合成酶抑制剂（磺酰脲、三唑并嘧啶）
	groupHormone      mixGroup = "hormone"       // 激素类阔叶除草剂
	groupCyhalofop    mixGroup = "cyhalofop"     // 芳氧苯氧丙酸酯类（氰氟草酯）
	groupAlkaline     mixGroup = "alkaline"      // 碱性药剂
	groupAlkaliLabile mixGroup = "alkali_labile" // 遇碱分解的药剂
	groupDiamide      mixGroup = "diamide"       // 双酰胺类
	groupNeonic       mixGroup = "neonic"        // 新烟碱类
)

// sameMoAGroups 是「同组两种不同产品即禁忌」的作用机理组（酸碱性分组不在内）。
var sameMoAGroups = []mixGroup{groupDMI, groupALS, groupDiamide, groupNeonic}

// forbiddenGroups 是跨组禁忌（无序）。
var forbiddenGroups = []struct {
	a, b mixGroup
	rule string
}{
	{groupCyhalofop, groupHormone, MixRuleCyhalofopHormone},
	{groupAlkaline, groupAlkaliLabile, MixRuleAlkaline},
}

// mixProduct 是分组表的一行：通用名、组与常见写法（商品名 / 俗称，归一到通用名）。
type mixProduct struct {
	name    string
	aliases []string
	group   mixGroup
}

// mixProducts 是分组表。
var mixProducts = []mixProduct{
	{name: "己唑醇", group: groupDMI},
	{name: "戊唑醇", group: groupDMI},
	{name: "肟菌·戊唑醇", aliases: []string{"肟菌酯·戊唑醇", "拿敌稳"}, group: groupDMI},
	{name: "咪鲜胺", group: groupDMI},
	{name: "丙环唑", group: groupDMI},
	{name: "苯醚甲环唑", group: groupDMI},
	{name: "五氟磺草胺", group: groupALS},
	{name: "苄嘧磺隆", group: groupALS},
	{name: "吡嘧磺隆", group: groupALS},
	{name: "二甲四氯", group: groupHormone},
	{name: "二甲四氯钠", group: groupHormone},
	{name: "氯氟吡氧乙酸", group: groupHormone},
	{name: "麦草畏", group: groupHormone},
	{name: "氰氟草酯", group: groupCyhalofop},
	{name: "石硫合剂", group: groupAlkaline},
	{name: "波尔多液", group: groupAlkaline},
	{name: "敌敌畏", group: groupAlkaliLabile},
	{name: "毒死蜱", group: groupAlkaliLabile},
	{name: "辛硫磷", group: groupAlkaliLabile},
	{name: "乐果", group: groupAlkaliLabile},
	{name: "氯虫苯甲酰胺", aliases: []string{"康宽"}, group: groupDiamide},
	{name: "溴氰虫酰胺", group: groupDiamide},
	{name: "呋虫胺", group: groupNeonic},
	{name: "吡虫啉", group: groupNeonic},
	{name: "噻虫嗪", group: groupNeonic},
}

func groupOf(name string) (mixGroup, bool) {
	i := slices.IndexFunc(mixProducts, func(p mixProduct) bool { return p.name == name })
	if i < 0 {
		return "", false
	}
	return mixProducts[i].group, true
}

// MixPair 是一对禁忌混用的产品（A < B，按通用名排序）。
type MixPair struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Rule string `json:"rule"`
}

// MixVerdict 是一次桶混判定：Products 为去重排序后的通用名；Max 为 0 表示不查产品数上限。
type MixVerdict struct {
	Products  []string  `json:"products"`
	Forbidden []MixPair `json:"forbidden,omitempty"`
	OverLimit bool      `json:"over_limit,omitempty"`
	Max       int       `json:"max,omitempty"`
}

// OK 表示无禁忌组合且不超过产品数上限。
func (v MixVerdict) OK() bool { return len(v.Forbidden) == 0 && !v.OverLimit }

// TankMix 判定一桶混配的产品（通用名）；结果与入参顺序无关。max 取知识库 max_products_in_tank，≤0 不查。
func TankMix(products []string, max int) MixVerdict {
	ps := slices.Clone(products)
	slices.Sort(ps)
	ps = slices.Compact(ps)
	v := MixVerdict{Products: ps, Max: max, OverLimit: max > 0 && len(ps) > max}
	for i, a := range ps {
		for _, b := range ps[i+1:] {
			if rule, bad := forbiddenRule(a, b); bad {
				v.Forbidden = append(v.Forbidden, MixPair{A: a, B: b, Rule: rule})
			}
		}
	}
	return v
}

// forbiddenRule 判定两种不同产品能否同桶。
func forbiddenRule(a, b string) (string, bool) {
	ga, oka := groupOf(a)
	gb, okb := groupOf(b)
	if !oka || !okb {
		return "", false
	}
	if ga == gb && slices.Contains(sameMoAGroups, ga) {
		return MixRuleSameMoA, true
	}
	for _, f := range forbiddenGroups {
		if (ga == f.a && gb == f.b) || (ga == f.b && gb == f.a) {
			return f.rule, true
		}
	}
	return "", false
}

// ForbiddenPartners 列出分组表中不得与 name 同桶的产品（通用名，表序）；表外产品返回空。
func ForbiddenPartners(name string) []string {
	var out []string
	for _, p := range mixProducts {
		if p.name == name {
			continue
		}
		if _, bad := forbiddenRule(name, p.name); bad {
			out = append(out, p.name)
		}
	}
	return out
}

// NormalizeText 是药名匹配的文本归一化：NFKC（全角字母数字、全角空格转半角，兼容字符归一），再删去空白与
// 格式字符（零宽空格 / 连接符、BOM、软连字符等 Unicode Cf 类），使「氰 氟草酯」「氰\u200b氟草酯」「氰\u3000氟草酯」
// 都按「氰氟草酯」匹配。幂等。
func NormalizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, norm.NFKC.String(s))
}

// Mention 是文本中的一处药名（Surface 为归一化后的写法，Canonical 为通用名，Pos 为 NormalizeText(text) 中的字节偏移）。
type Mention struct {
	Surface   string
	Canonical string
	Pos       int
}

// Lexicon 是药名写法 → 通用名：分组表名称与别名，并入登记表的通用名与别名。构造后只读，并发安全。
type Lexicon struct {
	surfaces []string // 按字数从长到短（最长匹配）
	canon    map[string]string
}

// NewLexicon 构造药名词表；reg 可空（只用分组表）。
func NewLexicon(reg *knowledge.Registry) Lexicon {
	l := Lexicon{canon: map[string]string{}}
	add := func(surface, name string) {
		surface = NormalizeText(surface)
		if surface == "" {
			return
		}
		if _, ok := l.canon[surface]; !ok {
			l.canon[surface] = name
			l.surfaces = append(l.surfaces, surface)
		}
	}
	for _, p := range mixProducts {
		add(p.name, p.name)
		for _, a := range p.aliases {
			add(a, p.name)
		}
	}
	if reg != nil {
		for _, p := range reg.Products {
			add(p.CommonName, p.CommonName)
			for _, a := range p.Aliases {
				add(a, p.CommonName)
			}
		}
	}
	slices.SortStableFunc(l.surfaces, func(a, b string) int {
		return cmp.Compare(utf8.RuneCountInString(b), utf8.RuneCountInString(a))
	})
	return l
}

// Canonical 把写法（先归一化）归一为通用名。
func (l Lexicon) Canonical(surface string) (string, bool) {
	c, ok := l.canon[NormalizeText(surface)]
	return c, ok
}

// Prefix 返回 s（已归一化）开头最长药名写法的字节长度，没有返回 0。
func (l Lexicon) Prefix(s string) int {
	for _, sf := range l.surfaces {
		if strings.HasPrefix(s, sf) {
			return len(sf)
		}
	}
	return 0
}

// Find 在 NormalizeText(text) 中按最长匹配、不重叠找出药名（按出现顺序）。
func (l Lexicon) Find(text string) []Mention {
	text = NormalizeText(text)
	var out []Mention
	for i := 0; i < len(text); {
		if n := l.Prefix(text[i:]); n > 0 {
			s := text[i : i+n]
			out = append(out, Mention{Surface: s, Canonical: l.canon[s], Pos: i})
			i += n
			continue
		}
		_, w := utf8.DecodeRuneInString(text[i:])
		i += w
	}
	return out
}
