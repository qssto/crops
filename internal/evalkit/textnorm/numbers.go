package textnorm

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Mul2 是倍数词归一后的记号：翻倍 / 加倍 / 双倍 / 翻番 / 两倍 / 二倍 / 独立的 2倍 都折成它，
// 禁止文本里写「翻倍」即可命中卡片里的任一说法。
const Mul2 = "×2"

// mul2Words 是不含数字的「×2」说法；两倍 / 二倍 先经中文数字折成 2倍，再由 arabicMul2 处理
// （先折数字可避免「十二倍」被拆成「十 + 二倍」）。
var mul2Words = []string{"翻倍", "加倍", "双倍", "翻番"}

// arabicMul2 匹配前面不是数字 / 小数点的「2倍」（「2000倍」稀释倍数不算）。
var arabicMul2 = regexp.MustCompile(`(^|[^0-9.])2倍`)

// cnDigits 是中文数字（含大写常用字）；cnUnits 是十进位单位。
var (
	cnDigits = map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	cnUnits  = map[rune]int{'十': 10, '百': 100, '千': 1000, '万': 10000}
)

const cnPoint = '点'

// FoldNumbers 做 NFKC、转小写、去空白，把倍数词折成 Mul2、中文数字折成阿拉伯数字（三十 → 30、一百二十 → 120、
// 零点五 → 0.5、二〇二六 → 2026）；标点保留，数字边界由 ContainsValue 判断。
func FoldNumbers(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(norm.NFKC.String(s)) {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	out := foldCNNumerals(b.String())
	for _, w := range mul2Words {
		out = strings.ReplaceAll(out, w, Mul2)
	}
	return arabicMul2.ReplaceAllString(out, "${1}"+Mul2)
}

// foldCNNumerals 把连续的中文数字串换成阿拉伯数字。
func foldCNNumerals(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		j := i
		for j < len(rs) && isCNNumeral(rs[j]) {
			j++
		}
		if j == i {
			b.WriteRune(rs[i])
			i++
			continue
		}
		b.WriteString(cnInt(rs[i:j]))
		// 「点」后紧跟中文数字才是小数点（「要点」「一点」不算）。
		if j+1 < len(rs) && rs[j] == cnPoint && isCNDigit(rs[j+1]) {
			k := j + 1
			b.WriteByte('.')
			for k < len(rs) && isCNDigit(rs[k]) {
				b.WriteString(strconv.Itoa(cnDigits[rs[k]]))
				k++
			}
			j = k
		}
		i = j
	}
	return b.String()
}

func isCNDigit(r rune) bool { _, ok := cnDigits[r]; return ok }

func isCNNumeral(r rune) bool {
	_, unit := cnUnits[r]
	return unit || isCNDigit(r)
}

// cnInt 把一个中文数字串转成十进制：不含单位时逐位拼接（二〇二六），否则按十 / 百 / 千 / 万进位（十二、三千零五、两万）。
func cnInt(rs []rune) string {
	hasUnit := false
	for _, r := range rs {
		if _, ok := cnUnits[r]; ok {
			hasUnit = true
			break
		}
	}
	if !hasUnit {
		var b strings.Builder
		for _, r := range rs {
			b.WriteString(strconv.Itoa(cnDigits[r]))
		}
		return b.String()
	}
	total, section, num := 0, 0, 0
	for _, r := range rs {
		if d, ok := cnDigits[r]; ok {
			num = d
			continue
		}
		u := cnUnits[r]
		if u == cnUnits['万'] {
			total += (section + num) * u
			section, num = 0, 0
			continue
		}
		if num == 0 {
			num = 1 // 「十二」「百」省略了前面的一
		}
		section += num * u
		num = 0
	}
	return strconv.Itoa(total + section + num)
}

// ContainsValue 报告 hay 是否含 value（两者都先 FoldNumbers）：value 以数字开头时命中处前一字符不得是数字或小数点，
// 以数字结尾时后一字符同样不得是数字（30克 不命中 130克，2000头 不命中 20000头）。
func ContainsValue(hay, value string) bool {
	h, v := FoldNumbers(hay), FoldNumbers(value)
	if v == "" {
		return false
	}
	vs := []rune(v)
	startDigit, endDigit := isASCIIDigit(vs[0]), isASCIIDigit(vs[len(vs)-1])
	for off := 0; ; {
		i := strings.Index(h[off:], v)
		if i < 0 {
			return false
		}
		i += off
		before, after := lastRune(h[:i]), firstRune(h[i+len(v):])
		okBefore := !startDigit || !(isASCIIDigit(before) || before == '.')
		okAfter := !endDigit || !(isASCIIDigit(after) || after == '.')
		if okBefore && okAfter {
			return true
		}
		off = i + len(v)
	}
}

// dosedWindow 是药剂名前后查数量的最大字数；clauseStops 截断窗口（换句、逗号后的数量不再算这个药的用量）。
const dosedWindow = 8

const clauseStops = "，,。；;！!？?、\n"

// quantityRe 是归一后的「数量 + 用药单位」或倍数记号。
var quantityRe = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?(?:克|g|毫升|ml|升|l|公斤|千克|kg|斤|袋|瓶|包|倍)|` + regexp.QuoteMeta(Mul2))

// DosedMention 报告 hay 中 product 的某次出现前后 dosedWindow 字内（不跨逗号 / 句号）是否带用量，返回该窗口文本。
// 只提及药名（如「毒死蜱不在登记候选中」）不算。
func DosedMention(hay, product string) (string, bool) {
	h, p := []rune(FoldNumbers(hay)), []rune(FoldNumbers(product))
	if len(p) == 0 {
		return "", false
	}
	for i := 0; i+len(p) <= len(h); i++ {
		if string(h[i:i+len(p)]) != string(p) {
			continue
		}
		lo := i
		for lo > 0 && i-lo < dosedWindow && !strings.ContainsRune(clauseStops, h[lo-1]) {
			lo--
		}
		end := i + len(p)
		hi := end
		for hi < len(h) && hi-end < dosedWindow && !strings.ContainsRune(clauseStops, h[hi]) {
			hi++
		}
		if win := string(h[lo:hi]); quantityRe.MatchString(win) {
			return win, true
		}
	}
	return "", false
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func lastRune(s string) rune {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1]
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}
