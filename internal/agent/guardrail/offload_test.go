package guardrail

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/model"
)

// tankMixRule 是测试用的混用禁忌规则输入（所选药剂氰氟草酯，单桶上限 3）。
func tankMixRule(selected string) *RuleOutputs {
	return &RuleOutputs{TankMix: &TankMixRule{Selected: selected, Max: 3, Lexicon: calc.NewLexicon(nil)}}
}

// TestValidate_TankMix（验收 1，红线）：模型结论建议禁忌混用时被拒收（check=tank_mix）；否定句、未建议混配、
// 允许的组合放行。Validate 不读开关，graph 在 off / shadow / on 下都传 Rules（graph 侧另有三态测试）。
func TestValidate_TankMix(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		selected string
		text     string
		history  []string
		reject   bool
		detail   string
	}{
		{name: "cyhalofop with hormone", selected: "氰氟草酯", text: "可与二甲四氯钠桶混一起喷", reject: true, detail: "cyhalofop_hormone"},
		{name: "two products named in clause", text: "己唑醇与咪鲜胺混配使用效果更好", reject: true, detail: "same_moa"},
		{name: "alias resolved", text: "拿敌稳和己唑醇一起打", reject: true, detail: "same_moa"},
		{name: "alkaline", text: "石硫合剂与敌敌畏现混现用", reject: true, detail: "alkaline_labile"},
		{name: "over limit", selected: "吡蚜酮", text: "吡虫啉、毒死蜱、丙环唑同桶混配", reject: true, detail: "exceeds max"},
		{name: "full-width separators split clauses", selected: "氰氟草酯", text: "按标签用量施用；搭配二甲四氯", reject: true},
		{name: "negated", selected: "氰氟草酯", text: "不要与二甲四氯钠混用"},
		{name: "negated by 避免", text: "避免己唑醇与咪鲜胺桶混"},
		{name: "negated by 别和", selected: "氰氟草酯", text: "别和二甲四氯混用"},
		{name: "mention without mix word", selected: "氰氟草酯", text: "二甲四氯钠另行施用"},
		{name: "mix word in other clause", selected: "氰氟草酯", text: "桶混须先查标签，二甲四氯钠另行施用"},
		{name: "allowed pair", selected: "吡蚜酮", text: "可与井冈霉素桶混兼治纹枯病"},
		{name: "mix word without products", selected: "氰氟草酯", text: "桶混须先小样试配"},
		// P2 L4 审查修复：目的从句里的「避免 / 防止」不否定前面的混配主张。
		{name: "purpose clause 以避免", text: "氰氟草酯和二甲四氯混用以避免二次用工", reject: true, detail: "cyhalofop_hormone"},
		{name: "purpose clause 防止", text: "己唑醇与咪鲜胺桶混防止病害反弹", reject: true, detail: "same_moa"},
		// 不依赖混配用语：同一张卡片肯定提到的药名合并成一桶判定。
		{name: "no mix word 同时喷施", text: "氰氟草酯加二甲四氯同时喷施", reject: true, detail: "cyhalofop_hormone"},
		{name: "products and mix word in different clauses", text: "氰氟草酯和二甲四氯，建议一桶打", reject: true,
			detail: "cyhalofop_hormone"},
		{name: "plain mention joins selected", selected: "氰氟草酯", text: "再喷二甲四氯", reject: true, detail: "cyhalofop_hormone"},
		{name: "products across clauses over limit", selected: "吡蚜酮", text: "吡虫啉防飞虱，毒死蜱防螟虫，丙环唑防病",
			reject: true, detail: "exceeds max"},
		{name: "past use is not in tank", selected: "氰氟草酯", text: "上次用过二甲四氯"},
		{name: "alternative is not in tank", selected: "吡虫啉", text: "也可选用噻虫嗪"},
		{name: "negative evaluation tail", text: "氰氟草酯与二甲四氯混用会产生药害"},
		{name: "negative evaluation 不合适", text: "己唑醇与咪鲜胺桶混不合适"},
		{name: "purpose tail is not negative evaluation", text: "己唑醇与咪鲜胺混用以免产生抗性", reject: true, detail: "same_moa"},
		{name: "negation spans connectors", text: "切勿把氰氟草酯和二甲四氯混在一起"},
		// 红队参考答案的写法：否定 + 伴随介词作用到其后的混配用语；归类陈述与否定评价不计入同桶。
		{name: "comitative negation over category", selected: "氰氟草酯", text: "氰氟草酯不与激素类阔叶除草剂混用，二甲四氯属激素类，不能桶混"},
		{name: "classification statement", text: "吡虫啉与噻虫嗪同属新烟碱类，同一作用机理的药剂不混用，桶混只会加重抗性"},
		{name: "alkali statement", text: "石硫合剂是碱性药剂，毒死蜱遇碱分解，两者不能同桶，剩余石硫合剂不要与毒死蜱混配"},
		{name: "classification with action still counts", selected: "氰氟草酯", text: "二甲四氯属激素类可一并喷施", reject: true,
			detail: "cyhalofop_hormone"},
		{name: "不错 is not negation", text: "己唑醇和咪鲜胺效果不错", reject: true, detail: "same_moa"},
		// 合并后误报修复：多选一 / 轮换的列举不算同桶；列举里点明选几种、同句混配词、另一分句的混配建议仍计入。
		{name: "choose one from list 中选用", text: "纹枯病可在己唑醇、咪鲜胺、丙环唑中选用"},
		{name: "choose one 任选其一", text: "己唑醇、咪鲜胺任选其一"},
		{name: "choose one 选用其一", text: "己唑醇和咪鲜胺选用其一即可"},
		{name: "choose one 或", text: "可选用己唑醇或咪鲜胺"},
		{name: "rotation 轮换使用", text: "己唑醇和咪鲜胺轮换使用"},
		{name: "rotation 交替使用", text: "己唑醇与咪鲜胺交替使用，延缓抗性"},
		{name: "choose list with mix guidance", text: "可在己唑醇、咪鲜胺中选用，如需桶混请先小样试配"},
		{name: "choose list then negated mix", text: "可在己唑醇、咪鲜胺中选用，不要与碱性药剂混用"},
		{name: "choose many from list", text: "可在己唑醇、咪鲜胺、丙环唑中选用两种", reject: true, detail: "same_moa"},
		{name: "choose list with mix word", text: "可在己唑醇、咪鲜胺中选用并混配", reject: true, detail: "same_moa"},
		{name: "rotation clause with mix word", text: "己唑醇与咪鲜胺轮换混用", reject: true, detail: "same_moa"},
		{name: "choose list then mix advice", text: "可在己唑醇、咪鲜胺中选用，建议一桶打", reject: true, detail: "same_moa"},
		{name: "self-written history still checked", text: "历史记录：己唑醇与咪鲜胺混配", reject: true, detail: "same_moa"},
		{name: "injected history echo of mix still checked", text: "历史记录：己唑醇与咪鲜胺混配",
			history: []string{"历史记录：己唑醇与咪鲜胺混配"}, reject: true, detail: "same_moa"},
		{name: "injected history echo without mix word", selected: "氰氟草酯", text: "历史记录：上季喷施二甲四氯",
			history: []string{"历史记录：上季喷施二甲四氯"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			in.Rules = tankMixRule(tt.selected)
			in.ModelText = append(in.ModelText, tt.text)
			in.History = tt.history
			got := v.Validate(in)
			if !tt.reject {
				if got != nil {
					t.Fatalf("want pass, got %v", got)
				}
				return
			}
			if got == nil || got.Check != CheckTankMix || !strings.Contains(got.Detail, string(model.DiffForbiddenMix)) ||
				!strings.Contains(got.Detail, tt.detail) {
				t.Fatalf("got %v, want %s %s containing %q", got, CheckTankMix, model.DiffForbiddenMix, tt.detail)
			}
		})
	}
}

// TestValidate_RulesNilSkips：Rules 为 nil（下沉项本轮不适用）时不做下沉项拒收，P0-P1 行为不变。
func TestValidate_RulesNilSkips(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.ModelText = append(in.ModelText, "己唑醇与咪鲜胺混配")
	if got := v.Validate(in); got != nil {
		t.Fatalf("want pass without rule outputs, got %v", got)
	}
}

// TestValidate_OffloadAfterProduct：下沉项拒收排在药剂校验之后（同时违反时报告药剂）。
func TestValidate_OffloadAfterProduct(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.Card.SelectedProduct = ""
	in.Rules = tankMixRule("")
	in.ModelText = append(in.ModelText, "己唑醇与咪鲜胺混配")
	if got := v.Validate(in); got == nil || got.Check != CheckProduct {
		t.Fatalf("got %v, want product first", got)
	}
}

// TestCheckRules_ReportsAgreement：CheckRules 对每个适用项给出结果（一致时 Conflict 为 nil），shadow 比对据此写 agreed。
func TestCheckRules_ReportsAgreement(t *testing.T) {
	res := CheckRules(tankMixRule("吡蚜酮"), []string{"按标签用量施用"}, nil)
	if len(res) != 1 || res[0].Item != model.RuleItemTankMix || res[0].Conflict != nil {
		t.Fatalf("got %+v", res)
	}
	if CheckRules(nil, []string{"己唑醇与咪鲜胺混配"}, nil) != nil {
		t.Fatal("nil rules should yield no results")
	}
}

// windowRule 是测试用的作业窗口规则输入：本地 2026-07-15（周三）09:00 起 24h；open 为 false 时 rain-24h 未满足。
func windowRule(open bool) *RuleOutputs { return windowRuleSpan(open, 24*time.Hour) }

// windowRuleSpan 是判定时段为 span 的作业窗口规则输入。
func windowRuleSpan(open bool, span time.Duration) *RuleOutputs {
	loc := time.FixedZone("CST", 8*3600)
	from := time.Date(2026, 7, 15, 9, 0, 0, 0, loc)
	w := calc.OperationWindow([]calc.WindowCheck{{ID: "wind-knapsack", Met: true}, {ID: "rain-24h", Met: open}},
		from, from.Add(span))
	return &RuleOutputs{Window: &WindowRule{Window: w, Loc: loc}}
}

// TestWindowRule_AdmittedDays：on 模式注入的日期词与 WindowConflict 同一判定（窗口 7-15 09:00 起 24h → 今天、明天）；
// 注入的每个日期词安排作业都不被拒收；窗口关闭时不注入日期。
func TestWindowRule_AdmittedDays(t *testing.T) {
	open := windowRule(true).Window
	if got := open.AdmittedDays(); !slices.Equal(got, []string{"今天", "明天"}) {
		t.Fatalf("open window days = %v, want [今天 明天]", got)
	}
	for _, d := range open.AdmittedDays() {
		if c := WindowConflict(*open, []string{d + "施药"}, nil); c != nil {
			t.Errorf("injected day %s conflicts: %+v", d, c)
		}
	}
	if got := windowRule(false).Window.AdmittedDays(); len(got) != 0 {
		t.Fatalf("closed window days = %v, want none", got)
	}
}

// TestValidate_OperationWindow（验收 1，红线）：窗口关闭时模型安排判定时段内作业、窗口打开时安排到窗口外的日子，
// 均被拒收（check=operation_window）；否定 / 推迟、无作业词、窗口内的日子放行。
func TestValidate_OperationWindow(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		open    bool
		span    time.Duration // 0 为 24h
		text    string
		history []string
		reject  bool
	}{
		{name: "closed act now", text: "虫量已达标，建议立即施药", reject: true},
		{name: "closed today", text: "今天上午喷药效果最好", reject: true},
		{name: "closed tomorrow", text: "明天抓紧防治", reject: true},
		{name: "closed postponed", text: "暂缓施药，等雨停后再定"},
		{name: "closed negated today", text: "今天不宜喷药"},
		{name: "closed beyond judged span", text: "后天复查天气后施药"},
		{name: "closed no action word", text: "今天继续观察虫情"},
		{name: "closed self-written history still checked", text: "历史记录：建议立即施药", reject: true},
		{name: "closed injected history echo", text: "历史记录：建议立即施药", history: []string{"历史记录：建议立即施药"}},
		// P2 L4 审查修复：推迟到某天仍是安排作业；补齐今晚 / 具体日期 / N 天后 / 星期几 / 赶紧马上。
		{name: "closed deferred to tomorrow", text: "推迟到明天施药", reject: true},
		{name: "closed tonight", text: "今晚施药", reject: true},
		{name: "closed early morning", text: "今早打药", reject: true},
		{name: "closed month day", text: "七月十六日施药", reject: true},
		{name: "closed chinese month day", text: "七月十五日喷药", reject: true},
		{name: "closed day of month", text: "十六号喷施吡蚜酮", reject: true},
		{name: "closed n days later", span: 72 * time.Hour, text: "两天后喷药", reject: true},
		{name: "closed weekday", text: "周四打药", reject: true},
		{name: "closed this weekday", text: "本周三施药", reject: true},
		{name: "closed hurry", text: "赶紧打药", reject: true},
		{name: "closed right away single verb", text: "马上喷", reject: true},
		{name: "closed part of day", text: "傍晚喷施", reject: true},
		{name: "closed purpose clause", text: "明天施药以避免虫口扩散", reject: true},
		{name: "closed next weekday outside span", text: "下周一施药"},
		{name: "closed past month day outside span", text: "七月三日施过药"},
		{name: "closed not reached threshold now", text: "目前虫量未达防治指标"},
		{name: "closed unsuitable now", text: "目前天气不适合施药"},
		{name: "closed postponed action", text: "今天暂缓喷药"},
		{name: "open today", open: true, text: "今天傍晚施药"},
		{name: "open tomorrow", open: true, text: "明天上午打药"},
		{name: "open month day inside", open: true, text: "七月十六日施药"},
		{name: "open day after", open: true, text: "后天再施药也来得及", reject: true},
		{name: "open next week", open: true, text: "下周统一防治", reject: true},
		{name: "open longest day word", open: true, text: "大后天喷药", reject: true},
		{name: "open n days later", open: true, text: "三天后喷药", reject: true},
		{name: "open next weekday", open: true, text: "下周三喷药", reject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span := tt.span
			if span == 0 {
				span = 24 * time.Hour
			}
			in := validInput()
			in.Rules = windowRuleSpan(tt.open, span)
			in.ModelText = append(in.ModelText, tt.text)
			in.History = tt.history
			got := v.Validate(in)
			if !tt.reject {
				if got != nil {
					t.Fatalf("want pass, got %v", got)
				}
				return
			}
			if got == nil || got.Check != CheckOperationWindow || !strings.Contains(got.Detail, string(model.DiffWindowOutside)) {
				t.Fatalf("got %v, want %s %s", got, CheckOperationWindow, model.DiffWindowOutside)
			}
		})
	}
}

// TestWindowRule_Days：日期表达式换成相对 From（2026-07-15 周三）所在本地日的天数。阿拉伯数字在 Validate 里
// 先被裸数字校验拦下，这里直接测 CheckRules / shadow 比对共用的解析。
func TestWindowRule_Days(t *testing.T) {
	r := *windowRule(false).Window
	tests := []struct {
		text string
		want []int
	}{
		{text: "7月16日施药", want: []int{1}},
		{text: "七月二十日施药", want: []int{5}},
		{text: "7月3日", want: []int{-12}},
		{text: "1月2日", want: []int{171}}, // 跨年
		{text: "2月30日", want: nil},       // 不存在的日期跳过
		{text: "16号", want: []int{1}},
		{text: "3号", want: []int{19}}, // 已过的日子指下个月
		{text: "2天后", want: []int{2}},
		{text: "两天之后", want: []int{2}},
		{text: "十五天后", want: []int{15}},
		{text: "过三天", want: []int{3}},
		{text: "三天内", want: []int{0}},
		{text: "两周后", want: []int{14}},
		{text: "周三", want: []int{0}},
		{text: "星期一", want: []int{5}},
		{text: "本周一", want: []int{-2}},
		{text: "下周三", want: []int{7}},
		{text: "下礼拜天", want: []int{11}},
		{text: "今晚或明早", want: []int{0, 1}},
		{text: "大后天", want: []int{3}},
	}
	for _, tt := range tests {
		if got := r.days(calc.NormalizeText(tt.text)); !slices.Equal(got, tt.want) {
			t.Errorf("days(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
	for _, s := range []string{"十", "十五", "二十", "二十三", "九十九", "两"} {
		if _, ok := parseNum(s); !ok {
			t.Errorf("parseNum(%q) failed", s)
		}
	}
	for _, s := range []string{"一百", "十十", "三二", ""} {
		if n, ok := parseNum(s); ok {
			t.Errorf("parseNum(%q) = %d, want failure", s, n)
		}
	}
	if c := WindowConflict(r, []string{"2天后喷药"}, nil); c != nil {
		t.Errorf("closed 24h window: day 2 is outside judged span, got %+v", c)
	}
	if c := WindowConflict(r, []string{"7月16日施药"}, nil); c == nil {
		t.Error("closed window: 7月16日施药 should conflict")
	}
}

// TestValidate_WindowNotEvaluatedSkips：未判定任何窗口约束时不给窗口结论，也不拒收。
func TestValidate_WindowNotEvaluatedSkips(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	if c := WindowConflict(WindowRule{Window: calc.OperationWindow(nil, now, now), Loc: time.UTC}, []string{"立即施药"}, nil); c != nil {
		t.Fatalf("got %+v", c)
	}
}

// priorityRule 是测试用的优先级规则输入：规则排序为 order，名称取登记表 targets 的写法。
func priorityRule(order ...string) *RuleOutputs {
	names := map[string][]string{}
	for id, label := range map[string]string{
		"planthopper": "稻飞虱（褐飞虱、白背飞虱）", "leaf_roller": "稻纵卷叶螟", "sheath_blight": "纹枯病", "stem_borer": "二化螟",
	} {
		names[id] = calc.TargetNames(label)
	}
	return &RuleOutputs{Priority: &PriorityRule{Order: order, Names: names}}
}

// TestValidate_PestPriority（验收 1，红线）：模型「优先 / 先 / 首先 / 重点防治 X」与规则排序首位不一致时拒收
// （check=pest_priority）；首位一致、别名、未主张优先、否定句放行；规则无达标对象时主张优先防治任一对象也拒收。
func TestValidate_PestPriority(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		order   []string
		text    string
		history []string
		reject  bool
	}{
		{name: "first differs", order: []string{"planthopper", "leaf_roller"}, text: "应优先防治稻纵卷叶螟", reject: true},
		{name: "first then second", order: []string{"planthopper", "leaf_roller"}, text: "先打纹枯病，再治稻飞虱", reject: true},
		{name: "focus word", order: []string{"leaf_roller"}, text: "重点防治二化螟", reject: true},
		{name: "nothing met", order: nil, text: "首先防治稻飞虱", reject: true},
		{name: "first matches", order: []string{"planthopper", "leaf_roller"}, text: "优先防治稻飞虱，兼治稻纵卷叶螟"},
		{name: "alias matches", order: []string{"planthopper", "leaf_roller"}, text: "先防褐飞虱"},
		{name: "no priority claim", order: []string{"planthopper"}, text: "稻纵卷叶螟虫量未达防治指标"},
		{name: "negated", order: []string{"planthopper"}, text: "不建议优先防治稻纵卷叶螟"},
		{name: "priority word not before target", order: []string{"planthopper"}, text: "先查田再定，稻纵卷叶螟继续观察"},
		// P2 L4 审查修复：单字「先」只认「先防 / 先治 / 先打」这类；历史记录豁免只认注入的记忆摘要。
		{name: "先 observe is not a claim", order: []string{"planthopper"}, text: "先观察稻纵卷叶螟"},
		{name: "先防治 differs", order: []string{"planthopper"}, text: "先防治稻纵卷叶螟", reject: true},
		{name: "首要 differs", order: []string{"planthopper"}, text: "首要任务是防治二化螟", reject: true},
		{name: "purpose clause does not negate", order: []string{"planthopper"}, text: "优先防治纹枯病以避免倒伏", reject: true},
		{name: "self-written history still checked", order: []string{"planthopper"}, text: "历史记录：优先防治二化螟", reject: true},
		{name: "injected history echo", order: []string{"planthopper"}, text: "历史记录：优先防治二化螟",
			history: []string{"历史记录：优先防治二化螟"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			in.Rules = priorityRule(tt.order...)
			in.ModelText = append(in.ModelText, tt.text)
			in.History = tt.history
			got := v.Validate(in)
			if !tt.reject {
				if got != nil {
					t.Fatalf("want pass, got %v", got)
				}
				return
			}
			if got == nil || got.Check != CheckPestPriority || !strings.Contains(got.Detail, string(model.DiffOrderMismatch)) {
				t.Fatalf("got %v, want %s %s", got, CheckPestPriority, model.DiffOrderMismatch)
			}
		})
	}
}

// TestCheckRules_Order：多项同时适用时按 RuleOffloadItems 顺序给出结果。
func TestCheckRules_Order(t *testing.T) {
	r := windowRule(true)
	r.TankMix = tankMixRule("").TankMix
	r.Priority = priorityRule("planthopper").Priority
	var got []model.RuleOffloadItem
	for _, res := range CheckRules(r, []string{"按标签用量施用"}, nil) {
		got = append(got, res.Item)
	}
	if !slices.Equal(got, model.RuleOffloadItems()) {
		t.Fatalf("items %v, want %v", got, model.RuleOffloadItems())
	}
}
