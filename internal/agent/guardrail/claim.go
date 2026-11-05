package guardrail

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"agri-steward/internal/agent/calc"
)

// ---- 分句与否定作用域（P2 L4 审查修复）：下沉项拒收与原文药名校验共用 ----
//
// 否定不按整句判定：否定 / 禁止词只作用于紧随其后、相邻的一串可链接词（药名、混配词、作业词、推荐词、优先词、
// 时间词与「与 / 和 / 把」等连接词），遇到其他字即止。「以避免二次用工」「防止稻飞虱暴发」这类目的从句因此不否定
// 前面的主张；推迟类词（暂缓 / 推迟）只在直接修饰作业词时算否定，「推迟到明天施药」仍是安排作业。分句以否定评价
// 结尾（「混用会产生药害」「今天施药不合适」）时整句不算主张。拿不准时按主张处理（拒收）。
//
// 历史记录豁免只认本轮注入的记忆摘要：模型原文里与某条注入摘要逐字相同（归一化后）的一条才算回显，
// 模型自己写的「历史记录：…」照常校验。

var (
	// clauseSep 是分句符：主张与否定都在分句内判定。
	clauseSep = regexp.MustCompile(`[，,。；;！!？?\n]`)
	// hardNegation 是否定 / 禁止词（长词在前；单字「不」只在其后紧邻可链接词时才否定到东西，「不错」「不断」不受影响）。
	hardNegation = regexp.MustCompile(`不要|不可以|不可|不能|不得|不宜|不应该|不应|不建议|不推荐|不准|不必|不用|不需要|不需|` +
		`无需|无须|没必要|不是|未达到|未达|没达到|没达|达不到|未到|没到|不到|不适合|不适宜|不合适|不利于|不混|不与|不和|不跟|别和|别与|别跟|别混|别用|别喷|别打|别施|别再|` +
		`切勿|切忌|请勿|勿|避免|防止|禁止|严禁|禁用|禁忌|忌|停止|暂停|暂不|先不|不`)
	// deferral 是推迟类词：只否定紧随其后的作业词（「暂缓施药」），其后跟时间（「推迟到明天施药」）时不否定。
	deferral = regexp.MustCompile(`暂缓|推迟|延后|延迟|推后|缓`)
	// connectWords 是否定作用域内可跨过的连接词与状语。
	connectWords = `以及|或者|一起|同时|进行|继续|急于|急着|着急|盲目|随意|擅自|自行|过早|过晚|与|和|跟|同|及|或|、|再|去|把|将|在|对|给|:`
	// singleVerbs 是单字作业 / 用药动词（「明天喷吡虫啉」「打一遍吡虫啉」）。
	singleVerbs = `喷|打|施|用|撒|拌|泼|浸`
	// timeWords 是否定作用域内可跨过的时间词（「不宜今天喷药」）。
	timeWords = `大后天|后天|明天|明日|明早|明晚|今天|今日|今晚|今夜|今早|当天|当日|下周|现在|立即|马上|立刻|尽快|赶紧|及时|` +
		`上午|下午|中午|傍晚|晚上|早上|清晨|夜间`
	// hardLink 是硬否定作用域可链接的词（不含药名与防治对象，由调用方的 linker 补充）。
	hardLink = regexp.MustCompile(`^(?:` + mixPattern + `|` + actionPattern + `|` + recommendPattern + `|` + priorityPattern +
		`|` + timeWords + `|` + connectWords + `|` + singleVerbs + `)`)
	// deferLink 是推迟类词作用域可链接的词：只有作业词。
	deferLink = regexp.MustCompile(`^(?:` + actionPattern + `|` + singleVerbs + `)`)
	// negTail 是分句末尾的否定评价（其前为否定字或目的用语时不算：「不会产生药害」「混用以免产生抗性」仍是主张）。
	negTail = regexp.MustCompile(`(?:不(?:太)?(?:合适|适宜|适合|可行|妥当|妥|安全|划算|可取|行|可以|能|可|允许|推荐|建议|宜)|` +
		`禁止|禁用|为禁忌|是禁忌|属禁忌|有禁忌|(?:会|易|容易|可能)(?:产生|发生|出现|造成|引起)?药害|有药害风险|` +
		`药害风险(?:较)?(?:大|高)|(?:会|只会|易|容易)?(?:加重|加快|产生)抗性|抗性风险(?:较)?(?:大|高)?|` +
		`(?:会|易|容易)?(?:降低药效|药效(?:降低|下降)|失效)|(?:遇碱|遇酸|易|会|容易)分解|不在\S*(?:候选|登记|范围|名单)(?:内|中|之内|里)?)(?:的|了|啊|呢|哦)?$`)
	// pastWords 是既往用药的写法（「近期用过吡虫啉」）：分句只陈述既往、没有肯定的推荐 / 混配用语时不算推荐。
	pastWords = regexp.MustCompile(`(?:用|打|喷|施)(?:过|了)|已(?:经)?(?:用|打|喷|施)|之前|此前|上次|上回|前次|曾经|曾`)
	// comitative 是伴随介词：「不与 / 不要和 / 切勿跟 X 混用」里否定作用到 X 之后的混配用语。
	comitative = regexp.MustCompile(`^(?:与|和|跟|同)`)
	// notNegation 是含「不」但不表否定的常用词（「效果不错」「不断扩散」）。
	notNegation = regexp.MustCompile(`^不(?:错|断|少|久|仅|但|过|一样|同(?:于|的|类|作用|机理))`)
	// tailCancel 是使末尾否定评价失效的前缀：否定字与目的 / 减轻用语。
	tailCancel = regexp.MustCompile(`(?:不|没|无|未|非|防止|避免|以免|免得|防|减少|减轻|延缓|降低|抑制)$`)
)

// linker 返回 s 开头可链接词的字节长度（0 表示不可链接）；s 已归一化。
type linker func(s string) int

// span 是分句内的字节区间 [start, end)。
type span struct{ start, end int }

// clause 是一个归一化后的分句及其否定作用域。
type clause struct {
	text    string
	neg     []span // 被否定的区间（含否定词本身）
	negTail bool   // 以否定评价结尾：整句不算主张
	history bool   // 所在的一条原文与本轮注入的某条记忆摘要逐字相同
}

// parseClauses 把模型原文切成归一化分句（calc.NormalizeText）并标出否定作用域；history 是本轮注入的记忆摘要。
// links 补充可链接词（药名、防治对象名）。
func parseClauses(texts, history []string, links ...linker) []clause {
	injected := make([]string, 0, len(history))
	for _, h := range history {
		injected = append(injected, calc.NormalizeText(h))
	}
	var out []clause
	for _, t := range texts {
		echo := slices.Contains(injected, calc.NormalizeText(t))
		for _, raw := range clauseSep.Split(t, -1) {
			for _, c := range clauseSep.Split(calc.NormalizeText(raw), -1) {
				if c != "" {
					out = append(out, analyze(c, echo, links))
				}
			}
		}
	}
	return out
}

// analyze 标出分句的否定作用域。否定词紧跟伴随介词（「不与」「不要和」）时，作用域延伸到其后第一个混配用语
// （「不与激素类阔叶除草剂混用」整段被否定）。
func analyze(text string, history bool, links []linker) clause {
	c := clause{text: text, history: history}
	hard := append([]linker{reLinker(hardLink)}, links...)
	for _, m := range hardNegation.FindAllStringIndex(text, -1) {
		if notNegation.MatchString(text[m[0]:]) {
			continue
		}
		s := chain(text, m, hard)
		if r, _ := utf8.DecodeLastRuneInString(text[m[0]:m[1]]); strings.ContainsRune("与和跟同", r) || comitative.MatchString(text[m[1]:]) {
			if loc := mixWords.FindStringIndex(text[s.end:]); loc != nil {
				s.end += loc[1]
			}
		}
		c.neg = append(c.neg, s)
	}
	for _, m := range deferral.FindAllStringIndex(text, -1) {
		if s := chain(text, m, []linker{reLinker(deferLink)}); s.end > m[1] {
			c.neg = append(c.neg, s)
		}
	}
	if loc := negTail.FindStringIndex(text); loc != nil {
		c.negTail = !tailCancel.MatchString(text[:loc[0]])
	}
	return c
}

// chain 从否定词 m 起向后吞并相邻的可链接词，返回整个否定作用域。
func chain(text string, m []int, links []linker) span {
	end := m[1]
	for end < len(text) {
		n := 0
		for _, l := range links {
			n = max(n, l(text[end:]))
		}
		if n == 0 {
			break
		}
		end += n
	}
	return span{start: m[0], end: end}
}

// reLinker 把以 ^ 锚定的正则转为 linker。
func reLinker(re *regexp.Regexp) linker {
	return func(s string) int {
		if loc := re.FindStringIndex(s); loc != nil {
			return loc[1]
		}
		return 0
	}
}

// lexiconLinker 让药名可链接（「不要与二甲四氯钠混用」）。
func lexiconLinker(l calc.Lexicon) linker { return l.Prefix }

// negated 表示区间 [start, end) 落在否定作用域内，或分句以否定评价结尾。
func (c clause) negated(start, end int) bool {
	if c.negTail {
		return true
	}
	return slices.ContainsFunc(c.neg, func(s span) bool { return start < s.end && s.start < end })
}

// affirmed 返回 re 在分句里第一处不被否定的匹配位置；没有返回 nil。
func (c clause) affirmed(re *regexp.Regexp) []int {
	for _, loc := range re.FindAllStringIndex(c.text, -1) {
		if !c.negated(loc[0], loc[1]) {
			return loc
		}
	}
	return nil
}

// pastOnly 表示分句只陈述既往用药：含既往写法且没有肯定的推荐 / 混配用语。
func (c clause) pastOnly() bool {
	return pastWords.MatchString(c.text) && c.affirmed(recommendWords) == nil && c.affirmed(mixWords) == nil
}

// mentions 返回分句里不被否定的药名。
func (c clause) mentions(l calc.Lexicon) []calc.Mention {
	var out []calc.Mention
	for _, m := range l.Find(c.text) {
		if !c.negated(m.Pos, m.Pos+len(m.Surface)) {
			out = append(out, m)
		}
	}
	return out
}
