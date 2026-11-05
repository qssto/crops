package guardrail

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/model"
)

// ---- P2 L4 下沉项拒收（红线，合入即常开）：规则层结论作为输入，拒收模型原文里与之冲突的结论 ----
//
// 这些函数不读任何开关：graph 在 off / shadow / on 三态下都把 RuleOutputs 交给 Validate；shadow 比对复用同一实现
// （CheckRules），P3 L5 conflict_checker 复用 TankMixConflict 并扩展，不另起一套。分句、否定作用域与历史记录豁免
// 见 claim.go。

// 下沉项校验项（VALIDATION_FAILED 事件 payload 的 check 字段，与下沉项名一致）。
const (
	CheckOperationWindow Check = Check(model.RuleItemOperationWindow)
	CheckTankMix         Check = Check(model.RuleItemTankMix)
	CheckPestPriority    Check = Check(model.RuleItemPestPriority)
)

// RuleOutputs 是本轮规则层对下沉项的结论；字段为 nil 表示该项本轮不适用（不校验、不比对）。
type RuleOutputs struct {
	Window   *WindowRule
	TankMix  *TankMixRule
	Priority *PriorityRule
}

// Conflict 是模型原文与规则结论的一处冲突：Snippet 是冲突所在的分句（归一化后的原文，调用方落库前须脱敏）。
type Conflict struct {
	Item    model.RuleOffloadItem
	Kind    model.RuleDiffKind
	Detail  string
	Snippet string
}

// ItemResult 是一个适用下沉项的比对结果：Conflict 为 nil 表示一致。
type ItemResult struct {
	Item     model.RuleOffloadItem
	Conflict *Conflict
}

// CheckRules 按 RuleOffloadItems 顺序比对每个适用项；r 为 nil 时返回空。history 是本轮注入的记忆摘要
// （模型原文里逐字回显的一条不算本次主张，混用禁忌除外）。
func CheckRules(r *RuleOutputs, texts, history []string) []ItemResult {
	if r == nil {
		return nil
	}
	var out []ItemResult
	if r.Window != nil {
		out = append(out, ItemResult{Item: model.RuleItemOperationWindow, Conflict: WindowConflict(*r.Window, texts, history)})
	}
	if r.TankMix != nil {
		out = append(out, ItemResult{Item: model.RuleItemTankMix, Conflict: TankMixConflict(*r.TankMix, texts, history)})
	}
	if r.Priority != nil {
		out = append(out, ItemResult{Item: model.RuleItemPestPriority, Conflict: PriorityConflict(*r.Priority, texts, history)})
	}
	return out
}

// offload 是 Validate 的下沉项拒收：返回第一处冲突。
func offload(in Input) *Violation {
	for _, res := range CheckRules(in.Rules, in.ModelText, in.History) {
		if c := res.Conflict; c != nil {
			return violation(Check(c.Item), "%s: %s (%q)", c.Kind, c.Detail, c.Snippet)
		}
	}
	return nil
}

// ---- 混用禁忌（tank_mix）----

// TankMixRule 是混用禁忌的规则输入：Selected 是规则层候选内的所选药剂通用名（可空），Max 是知识库单桶产品数上限
// （≤0 不查），Lexicon 是药名词表（分组表 + 登记表）。
type TankMixRule struct {
	Selected string
	Max      int
	Lexicon  calc.Lexicon
}

// mixPattern 是混配用语。
const mixPattern = `混用|混配|桶混|混合|混施|混喷|现混|复配|同桶|同喷|搭配|一桶|一起(?:打|喷|用|施)|同时(?:打|喷|用|施)|兑在一起|加在一起|混在一起|掺`

var (
	mixWords = regexp.MustCompile(mixPattern)
	// separateWords 是分开施用 / 另作备选的写法：分句没有肯定的混配用语时，其中的药名不计入同桶。
	separateWords = regexp.MustCompile(`也可以|也可|亦可|还可以|还可|也能|另行|单独|分开|分别|错开|间隔|隔开|相隔|备选|替代|换用|改用`)
	// chooseOneWords 是多选一的列举（「可在 A、B、C 中选用」「A、B 任选其一」「A 或 B」）：只用其中一种，不是同桶。
	chooseOneWords = regexp.MustCompile(`(?:当中|之中|中)(?:任选|选用|选择|挑选|选)|任选|择一|其一|之一|其中一种|任意一种|一种即可|或者|或`)
	// pickManyWords 是列举里点明要选几种（「中选用两种」「全部」）：不再是多选一。
	pickManyWords = regexp.MustCompile(`(?:两|二|三|四|几|多|数)(?:种|个|样|款)|全部|一并`)
	// rotateWords 是轮换 / 交替使用（「A 与 B 轮换使用」）：先后分次施用，不是同桶。
	rotateWords = regexp.MustCompile(`轮换|交替|轮用|轮流`)
	// mixAdviceWords 是不点名药剂的混配建议里的推荐用语（「建议一桶打」「可以桶混」）；「如需桶混请先小样试配」不算。
	mixAdviceWords = regexp.MustCompile(recommendPattern + `|可以|可|能|就行|即可`)
	// describeWords 是归类陈述（「二甲四氯属激素类」「石硫合剂是碱性药剂」）：分句没有肯定的作业 / 推荐 / 混配用语时
	// 其中的药名不计入同桶。
	describeWords = regexp.MustCompile(`同属|属于|属|(?:是|为)\S{0,12}?(?:类|剂)`)
	// tankActWords 是归类陈述里仍算主张的用语。
	tankActWords = regexp.MustCompile(actionPattern + `|` + recommendPattern + `|` + singleVerbs)
)

// tankRole 是分句里不被否定的药名对同桶判定的作用。
type tankRole int

const (
	tankOut tankRole = iota // 不计入
	tankIn                  // 计入
	tankAlt                 // 多选一 / 轮换的列举：卡片另有不点名药剂的混配建议时计入，否则不计入
)

// tankClaim 判定分句里不被否定的药名是否计入同桶：以否定评价结尾、混配用语全被否定、只陈述既往、分开 / 备选写法、
// 没有作业 / 推荐用语的归类陈述都不计入；多选一（未点明选几种）与轮换列举为 tankAlt；注入记忆的回显只在含肯定的
// 混配用语时计入（混用禁忌不豁免回显里的混配建议）。其余拿不准的都计入。
func (c clause) tankClaim() tankRole {
	switch {
	case c.negTail:
		return tankOut
	case c.affirmed(mixWords) != nil:
		return tankIn
	case describeWords.MatchString(c.text) && c.affirmed(tankActWords) == nil:
		return tankOut
	case c.history || mixWords.MatchString(c.text) || c.pastOnly():
		return tankOut
	case rotateWords.MatchString(c.text) || chooseOneWords.MatchString(c.text) && !pickManyWords.MatchString(c.text):
		return tankAlt
	case separateWords.MatchString(c.text):
		return tankOut
	}
	return tankIn
}

// mixAdvice 表示分句是不点名药剂的混配建议（「建议一桶打」）：其所指只能是卡片上别处列出的药剂。
func (c clause) mixAdvice(l calc.Lexicon) bool {
	return !c.history && !c.negTail && c.affirmed(mixWords) != nil && c.affirmed(mixAdviceWords) != nil &&
		len(l.Find(c.text)) == 0
}

// TankMixConflict：整张卡片模型原文里肯定提到的药名（不依赖混配用语，见 tankClaim）合并所选药剂，按 calc.TankMix
// 判定一次，有禁忌组合或超过单桶产品数上限即 FORBIDDEN_MIX。多选一 / 轮换列举里的药名只在卡片另有不点名药剂的
// 混配建议时计入（「可在 A、B 中选用，建议一桶打」）。
func TankMixConflict(r TankMixRule, texts, history []string) *Conflict {
	var products []string
	from := map[string]string{} // 通用名 → 首次出现的分句
	if r.Selected != "" {
		products = append(products, r.Selected)
	}
	clauses := parseClauses(texts, history, lexiconLinker(r.Lexicon))
	altIn := slices.ContainsFunc(clauses, func(c clause) bool { return c.mixAdvice(r.Lexicon) })
	for _, c := range clauses {
		if role := c.tankClaim(); role == tankOut || role == tankAlt && !altIn {
			continue
		}
		for _, m := range c.mentions(r.Lexicon) {
			if _, seen := from[m.Canonical]; !seen {
				from[m.Canonical] = c.text
			}
			products = append(products, m.Canonical)
		}
	}
	v := calc.TankMix(products, r.Max)
	switch {
	case len(v.Forbidden) > 0:
		p := v.Forbidden[0]
		snippet := from[p.B]
		if p.B == r.Selected || snippet == "" {
			snippet = from[p.A]
		}
		return &Conflict{Item: model.RuleItemTankMix, Kind: model.DiffForbiddenMix,
			Detail: "forbidden mix " + p.A + " + " + p.B + " (" + p.Rule + ")", Snippet: snippet}
	case v.OverLimit:
		return &Conflict{Item: model.RuleItemTankMix, Kind: model.DiffForbiddenMix,
			Detail: "tank mix of " + strings.Join(v.Products, "/") + " exceeds max products in tank", Snippet: joinSources(from)}
	}
	return nil
}

// joinSources 把同桶药名的来源分句去重、排序后拼接（超上限时药名可能分散在多个分句）。
func joinSources(from map[string]string) string {
	srcs := make([]string, 0, len(from))
	for _, s := range from {
		srcs = append(srcs, s)
	}
	slices.Sort(srcs)
	return strings.Join(slices.Compact(srcs), " / ")
}

// ---- 作业窗口（operation_window）----

// WindowRule 是作业窗口的规则输入：Window 由规则层已判定的窗口硬约束汇成，Loc 是本地日历日的时区。
type WindowRule struct {
	Window calc.Window
	Loc    *time.Location
}

// actionPattern 是作业用语（施药 / 施肥类，含单字「喷 / 施 / 撒」）。
const actionPattern = `施药|打药|喷药|用药|喷雾|喷施|飞防|防治|预防|追肥|施肥|撒施|追施|补施|作业|下药|打(?:一[遍次下]|掉)|喷|施|撒`

// numPattern 是日期里的数（阿拉伯数字或「一」到「九十九」的中文数字）。
const numPattern = `[0-9]+|[零〇一二两三四五六七八九十]+`

var (
	actionWords = regexp.MustCompile(actionPattern)
	// actNowWords 是「立即作业」用语：没有日期时按今天算。
	actNowWords = regexp.MustCompile(`立即|马上|立刻|立马|即刻|当即|现在|尽快|尽早|趁早|抓紧|赶紧|赶快|及时|迅速|第一时间|当下|眼下|目前|当前|此时`)
	// partOfDay 是一天中的时段：没有日期时按今天算（「傍晚施药」）。
	partOfDay = regexp.MustCompile(`上午|下午|中午|午后|傍晚|黄昏|晚上|晚间|夜间|早上|早晨|清晨`)
	// dateExpr 是日期表达式，按序尝试（长的在前：「下周三」不读成「下周」，「三天后」不读成「三日」）。
	dateExpr = regexp.MustCompile(`(?P<mon>` + numPattern + `)月(?P<mday>` + numPattern + `)[日号]` +
		`|(?P<after>` + numPattern + `)个?(?:天|日)[之以]?后` +
		`|过(?P<pass>` + numPattern + `)个?(?:天|日)` +
		`|(?P<within>` + numPattern + `)个?(?:天|日)[之以]?内` +
		`|(?P<weeks>` + numPattern + `)个?(?:星期|礼拜|周)[之以]?后` +
		`|(?P<wpre>下|本|这)?个?(?:星期|礼拜|周)(?P<wday>[一二三四五六日天七1-7])` +
		`|(?P<rel>大后天|明后天|今明两天|今明|后天|明天|明日|明早|明晚|明晨|明儿|次日|翌日|今天|今日|今晚|今夜|今早|今晨|今儿|` +
		`当天|当日|当晚|下周|下星期|下礼拜|本周|这周|这两天|这几天|近两天|近几天)` +
		`|(?P<mdom>` + numPattern + `)[日号]`)
	// relOffset 是相对日期词距今天的天数。
	relOffset = map[string]int{"大后天": 3, "明后天": 1, "今明两天": 0, "今明": 0, "后天": 2, "明天": 1, "明日": 1, "明早": 1,
		"明晚": 1, "明晨": 1, "明儿": 1, "次日": 1, "翌日": 1, "今天": 0, "今日": 0, "今晚": 0, "今夜": 0, "今早": 0, "今晨": 0,
		"今儿": 0, "当天": 0, "当日": 0, "当晚": 0, "下周": 7, "下星期": 7, "下礼拜": 7, "本周": 0, "这周": 0, "这两天": 0,
		"这几天": 0, "近两天": 0, "近几天": 0}
	// weekdayNum 是星期几（周一为 1，周日为 7）。
	weekdayNum = map[string]int{"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "日": 7, "天": 7, "七": 7,
		"1": 1, "2": 2, "3": 3, "4": 4, "5": 5, "6": 6, "7": 7}
)

// injectDays 是 on 模式注入的日期词（按相对天数升序）。
var injectDays = []struct {
	word   string
	offset int
}{{"今天", 0}, {"明天", 1}, {"后天", 2}, {"大后天", 3}}

// AdmittedDays 是窗口打开时可安排作业的日期词（on 模式注入用，与 WindowConflict 同一判定）；
// 窗口关闭或未判定时为空。
func (r WindowRule) AdmittedDays() []string {
	w := r.Window
	if !w.Evaluated || !w.Open || r.Loc == nil {
		return nil
	}
	var out []string
	for _, d := range injectDays {
		if w.AdmitsDay(d.offset, r.Loc) {
			out = append(out, d.word)
		}
	}
	return out
}

// scheduled 取分句里本次肯定安排的作业日（相对 From 所在本地日的天数）：须有不被否定的作业用语且不是注入记忆的
// 回显；日期表达式全取，没有日期时有立即用语或时段词按今天。
func (r WindowRule) scheduled(c clause) []int {
	if c.history || c.affirmed(actionWords) == nil {
		return nil
	}
	if days := r.days(c.text); len(days) > 0 {
		return days
	}
	if actNowWords.MatchString(c.text) || partOfDay.MatchString(c.text) {
		return []int{0}
	}
	return nil
}

// days 解析分句里的全部日期表达式（相对 From 所在本地日的天数）；无法解析的写法（如「十三月」）跳过。
func (r WindowRule) days(text string) []int {
	from := r.Window.From.In(r.Loc)
	var out []int
	for _, m := range dateExpr.FindAllStringSubmatch(text, -1) {
		if d, ok := dayOf(m, from); ok {
			out = append(out, d)
		}
	}
	return out
}

// dayOf 把 dateExpr 的一处匹配换成相对 from 所在本地日的天数。
func dayOf(m []string, from time.Time) (int, bool) {
	g := func(name string) string { return m[dateExpr.SubexpIndex(name)] }
	switch {
	case g("mon") != "":
		mo, ok1 := parseNum(g("mon"))
		d, ok2 := parseNum(g("mday"))
		if !ok1 || !ok2 {
			return 0, false
		}
		off, ok := dateOffset(from, from.Year(), time.Month(mo), d)
		if ok && off < -31 { // 跨年：年底说「一月二日」指来年
			return dateOffset(from, from.Year()+1, time.Month(mo), d)
		}
		return off, ok
	case g("after") != "":
		return parseNum(g("after"))
	case g("pass") != "":
		return parseNum(g("pass"))
	case g("within") != "":
		return 0, true // 「三天内」含今天
	case g("weeks") != "":
		n, ok := parseNum(g("weeks"))
		return 7 * n, ok
	case g("wday") != "":
		return weekdayOffset(from, g("wpre"), weekdayNum[g("wday")]), true
	case g("rel") != "":
		return relOffset[g("rel")], true
	case g("mdom") != "":
		d, ok := parseNum(g("mdom"))
		if !ok {
			return 0, false
		}
		mo := from.Month()
		if d < from.Day() { // 已过的日子指下个月
			mo++
		}
		return dateOffset(from, from.Year(), mo, d)
	}
	return 0, false
}

// dateOffset 是本地日期 y-mo-d 距 from 所在本地日的天数；日期不存在（如 2 月 30 日）时返回 false。
func dateOffset(from time.Time, y int, mo time.Month, d int) (int, bool) {
	loc := from.Location()
	t := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	if t.Day() != d || (mo >= time.January && mo <= time.December && t.Month() != mo) {
		return 0, false
	}
	base := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	return int(t.Sub(base).Round(24*time.Hour) / (24 * time.Hour)), true
}

// weekdayOffset 是星期 wd（1-7）距 from 的天数：「下周 X」为下周的那天，「本周 / 这周 X」可为负，
// 不带前缀为从今天起最近的那天（含今天）。
func weekdayOffset(from time.Time, prefix string, wd int) int {
	today := int(from.Weekday())
	if today == 0 {
		today = 7
	}
	switch prefix {
	case "下":
		return 7 - today + wd
	case "本", "这":
		return wd - today
	}
	return (wd - today + 7) % 7
}

// cnDigits 是中文数字。
var cnDigits = map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

// parseNum 解析阿拉伯数字或「一」到「九十九」的中文数字（十、十五、二十、二十三）。
func parseNum(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	rs := []rune(s)
	i := slices.Index(rs, '十')
	if i < 0 {
		if len(rs) != 1 {
			return 0, false
		}
		n, ok := cnDigits[rs[0]]
		return n, ok
	}
	tens, ones := 1, 0
	if i > 1 || len(rs) > i+2 {
		return 0, false
	}
	if i == 1 {
		n, ok := cnDigits[rs[0]]
		if !ok {
			return 0, false
		}
		tens = n
	}
	if len(rs) == i+2 {
		n, ok := cnDigits[rs[i+1]]
		if !ok {
			return 0, false
		}
		ones = n
	}
	return tens*10 + ones, true
}

// WindowConflict：窗口关闭时，模型肯定地安排落在判定时段内的作业即冲突；窗口打开时，安排到与 [From, To] 不相交的
// 本地日历日即冲突（WINDOW_OUTSIDE）。未判定任何窗口约束时不给结论。
func WindowConflict(r WindowRule, texts, history []string) *Conflict {
	w := r.Window
	if !w.Evaluated || r.Loc == nil {
		return nil
	}
	for _, c := range parseClauses(texts, history) {
		for _, day := range r.scheduled(c) {
			switch {
			case !w.Open && w.Covers(day, r.Loc):
				return &Conflict{Item: model.RuleItemOperationWindow, Kind: model.DiffWindowOutside,
					Detail: "window closed by " + strings.Join(w.Blocked, "/"), Snippet: c.text}
			case w.Open && !w.AdmitsDay(day, r.Loc):
				return &Conflict{Item: model.RuleItemOperationWindow, Kind: model.DiffWindowOutside,
					Detail: "scheduled day outside window", Snippet: c.text}
			}
		}
	}
	return nil
}

// ---- 多病虫害优先级（pest_priority）----

// PriorityRule 是优先级排序的规则输入：Order 是 calc.PestPriority 的排序（可空：无达标对象），
// Names 是防治对象 id → 可识别名称（calc.TargetNames）。
type PriorityRule struct {
	Order []string
	Names map[string][]string
}

// priorityPattern 是「优先防治」用语（「首先」在单字「先」组合之前；单字「先」只认「先防 / 先治 / 先打」这类，
// 「先观察稻飞虱」「先查田」不算主张）。
const priorityPattern = `优先|首先|首要|重点|主攻|先(?:防|治|打|喷|施|用|处理|控|压|杀|除)`

var priorityWords = regexp.MustCompile(priorityPattern)

// PriorityConflict：模型肯定地主张优先防治某对象（分句里第一个不被否定的优先用语之后第一个出现的防治对象）而它
// 不是规则排序首位（或规则无达标对象）时 ORDER_MISMATCH。注入记忆的回显不算主张。
func PriorityConflict(r PriorityRule, texts, history []string) *Conflict {
	for _, c := range parseClauses(texts, history, r.nameLinker) {
		if c.history {
			continue
		}
		loc := c.affirmed(priorityWords)
		if loc == nil {
			continue
		}
		claimed := r.firstTarget(c.text[loc[1]:])
		if claimed == "" || (len(r.Order) > 0 && claimed == r.Order[0]) {
			continue
		}
		return &Conflict{Item: model.RuleItemPestPriority, Kind: model.DiffOrderMismatch,
			Detail: "model prioritizes " + claimed + ", rule order [" + strings.Join(r.Order, " ") + "]", Snippet: c.text}
	}
	return nil
}

// nameLinker 让防治对象名可链接（「不建议优先防治稻纵卷叶螟」）。
func (r PriorityRule) nameLinker(s string) int {
	best := 0
	for _, ns := range r.Names {
		for _, n := range ns {
			if n = calc.NormalizeText(n); n != "" && strings.HasPrefix(s, n) {
				best = max(best, len(n))
			}
		}
	}
	return best
}

// firstTarget 返回 text 里最先出现的防治对象 id（同一位置取较长名称，id 升序保证确定性）。
func (r PriorityRule) firstTarget(text string) string {
	ids := make([]string, 0, len(r.Names))
	for id := range r.Names {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	best, bestPos, bestLen := "", -1, 0
	for _, id := range ids {
		for _, n := range r.Names[id] {
			n = calc.NormalizeText(n)
			p := strings.Index(text, n)
			if n == "" || p < 0 {
				continue
			}
			if bestPos < 0 || p < bestPos || (p == bestPos && len(n) > bestLen) {
				best, bestPos, bestLen = id, p, len(n)
			}
		}
	}
	return best
}
