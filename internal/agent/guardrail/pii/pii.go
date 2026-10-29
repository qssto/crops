// Package pii 是脱敏规则的唯一实现（P0 §6.6 功能任务 4）：手机号、身份证、账号掩码，精确经纬度降精度。
// 四个出口共用：应用日志（Handler）、OTel span 属性（MaskAttrs）、模型入参（ForModel）、落库摘要
// （Mask / MaskJSON，service / agent 在写 run_event、tool_call、audit_log 前调用，repository 不脱敏）。
//
// 识别规则（只按形态识别，宁可多掩不漏；全角数字 ０-９ 先按半角识别，命中处输出半角掩码）：
//   - 手机号：11 位 1[3-9] 开头的数字串（可带 86 前缀，或 3-4-4 以空格 / 连字符分隔），以及中文数字写法
//     （「一三八……」11 个中文数字，〇 / 零 / 幺 / 两 均识别）；
//   - 身份证：18 位（末位可为 X）或 15 位数字串，也识别 6-8-4 / 6-6-3 以空格 / 连字符分隔的写法；
//   - 账号：16-19 位数字串，也识别每 4 位以空格 / 连字符分隔的写法（18 位身份证按 4 位分组也落在这里）；
//   - 坐标：两个小数构成的「纬度,经度」或「经度,纬度」对（逗号分隔两数都有 ≥3 位小数，仅空白分隔要求 ≥4 位，
//     且至少一个数的整数部分 ≥10，避免把 pH、比值当坐标）；带「北纬 / 南纬 / 东经 / 西经 / 纬度 / 经度」标签的
//     单个 ≥3 位小数；JSON / 日志属性中坐标键（见 coordKeys）下的数值。
//
// 数字串必须前后不是数字才算命中；小数点后的数字串（如 0.70833333333333334 的小数部分）不当号码；
// 长度恰为 32 / 40 / 64 且含 a-f 的十六进制串（md5 / sha1 / sha256 摘要）整体跳过。
package pii

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// FieldRef 是模型入参中替换精确坐标的地块级引用（地块 ID 已在请求上下文中，坐标不再入模）。
const FieldRef = "[FIELD_REF]"

// logDecimals 是日志 / 摘要中坐标保留的小数位（0.01°）。
const logDecimals = 2

// minCoordDecimals 是视为「精确坐标」的最少小数位；更粗的坐标不处理。
// spacedCoordDecimals 是仅以空白分隔的坐标对要求的小数位（空白分隔的两个小数更常见于普通数值）。
// minCoordMagnitude 是坐标对中至少一个数的整数部分下限（两数都 <10° 的点在几内亚湾海面上）。
const (
	minCoordDecimals    = 3
	spacedCoordDecimals = 4
	minCoordMagnitude   = 10
)

// digestLens 是整体跳过的十六进制摘要长度（md5 / sha1 / sha256）。
var digestLens = []int{32, 40, 64}

var (
	coordNumRe   = regexp.MustCompile(`-?\d{1,3}\.\d+`)
	commaSepRe   = regexp.MustCompile(`^\s*[,，]\s*$`)
	spaceSepRe   = regexp.MustCompile(`^\s+$`)
	labelCoordRe = regexp.MustCompile(`(北纬|南纬|东经|西经|纬度|经度)\s*[:：]?\s*(-?\d{1,3}\.\d+)`)
	// splitRes 是带分隔符的号码：手机号 3-4-4、身份证 6-8-4 / 6-6-3、账号每 4 位一组（16-19 位）。
	splitRes = []*regexp.Regexp{
		regexp.MustCompile(`1[3-9]\d[- ]\d{4}[- ]\d{4}`),
		regexp.MustCompile(`\d{6}[- ]\d{8}[- ]\d{3}[0-9Xx]`),
		regexp.MustCompile(`\d{6}[- ]\d{6}[- ]\d{3}`),
		regexp.MustCompile(`\d{4}(?:[- ]\d{4}){3}(?:[- ](?:\d{1,3}|\d{0,2}[Xx]))?`),
	}
	cnMobileRe = regexp.MustCompile(`[一幺][三四五六七八九][〇零一幺二两三四五六七八九]{9}`)
	alnumRe    = regexp.MustCompile(`[0-9A-Za-z]+`)
	digitsRe   = regexp.MustCompile(`[0-9]+`)
)

// cnDigits 是中文数字到半角数字（手机号口语写法）。
var cnDigits = map[rune]byte{'〇': '0', '零': '0', '一': '1', '幺': '1', '二': '2', '两': '2', '三': '3',
	'四': '4', '五': '5', '六': '6', '七': '7', '八': '8', '九': '9'}

// Mask 是日志与落库摘要口径：手机号 / 身份证 / 账号掩码，精确坐标截断到 0.01°。
func Mask(s string) string {
	return mask(s, func(span string) string { return truncateNums(span) })
}

// ForModelText 是模型入参的纯文本口径：同 Mask，但精确坐标整体替换为 FieldRef。JSON 用 ForModel。
func ForModelText(s string) string {
	return mask(s, func(string) string { return FieldRef })
}

// edit 是对规范化文本 [start, end) 的一处替换。
type edit struct {
	start, end int
	rep        string
}

// edits 收集互不重叠的替换（先收的规则优先）。
type edits []edit

func (es *edits) add(start, end int, rep string) {
	if !es.free(start, end) {
		return
	}
	*es = append(*es, edit{start, end, rep})
}

func (es edits) free(start, end int) bool {
	for _, e := range es {
		if start < e.end && e.start < end {
			return false
		}
	}
	return true
}

// mask 在规范化文本（全角数字转半角）上依次识别坐标、带分隔号码、中文数字手机号、连续数字串，
// 再把替换映射回原文；coord 决定坐标片段（含两数之间的分隔符）替换成什么。
func mask(s string, coord func(span string) string) string {
	if s == "" {
		return s
	}
	t, off := normalize(s)
	var es edits
	labelledCoords(t, coord, &es)
	coordPairs(t, coord, &es)
	for _, re := range splitRes {
		splitNumbers(t, re, &es)
	}
	cnMobiles(t, &es)
	digitTokens(t, &es)
	if len(es) == 0 {
		return s
	}
	slices.SortFunc(es, func(a, b edit) int { return a.start - b.start })
	var b strings.Builder
	last := 0
	for _, e := range es {
		b.WriteString(s[last:off[e.start]])
		b.WriteString(e.rep)
		last = off[e.end]
	}
	b.WriteString(s[last:])
	return b.String()
}

// normalize 把全角数字 ０-９ 换成半角；off[i] 是规范化文本第 i 字节在原文中的字节偏移（off[len(t)] = len(s)）。
func normalize(s string) (string, []int) {
	if !strings.ContainsFunc(s, isFullWidthDigit) {
		off := make([]int, len(s)+1)
		for i := range off {
			off[i] = i
		}
		return s, off
	}
	var b strings.Builder
	off := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if isFullWidthDigit(r) {
			b.WriteByte(byte('0' + r - '０'))
			off = append(off, i)
		} else {
			// 原样写回原字节（含非法 UTF-8），保证规范化文本与偏移逐字节对应。
			b.WriteString(s[i : i+n])
			for k := 0; k < n; k++ {
				off = append(off, i+k)
			}
		}
		i += n
	}
	return b.String(), append(off, len(s))
}

func isFullWidthDigit(r rune) bool { return r >= '０' && r <= '９' }

// labelledCoords 识别「北纬 31.234567」等带标签的单个精确坐标（只替换数值）。
func labelledCoords(t string, coord func(string) string, es *edits) {
	for _, m := range labelCoordRe.FindAllStringSubmatchIndex(t, -1) {
		label, start, end := t[m[2]:m[3]], m[4], m[5]
		num := t[start:end]
		limit := 180
		if strings.Contains(label, "纬") {
			limit = 90
		}
		if afterNum(t, end) && decimals(num) >= minCoordDecimals && intPart(num) <= limit {
			es.add(start, end, coord(num))
		}
	}
}

// coordPairs 识别坐标对：相邻两个小数之间只有逗号或空白、且满足坐标条件即为一对；
// 一个数同时和左右两数成对时整段一起处理（宁可多掩），模型口径整段替换为一个 FieldRef。
func coordPairs(t string, coord func(string) string, es *edits) {
	var nums [][]int
	for _, m := range coordNumRe.FindAllStringIndex(t, -1) {
		if beforeNum(t, m[0]) && afterNum(t, m[1]) {
			nums = append(nums, m)
		}
	}
	for i := 0; i+1 < len(nums); {
		j := i
		for j+1 < len(nums) && isCoordPair(t, nums[j], nums[j+1]) {
			j++
		}
		if j == i {
			i++
			continue
		}
		start, end := nums[i][0], nums[j][1]
		es.add(start, end, coord(t[start:end]))
		i = j + 1
	}
}

// isCoordPair 判断 a、b 两个小数（及其间分隔符）是否构成精确坐标对（纬经或经纬顺序均可）。
func isCoordPair(t string, a, b []int) bool {
	sep := t[a[1]:b[0]]
	need := 0
	switch {
	case commaSepRe.MatchString(sep):
		need = minCoordDecimals
	case spaceSepRe.MatchString(sep):
		need = spacedCoordDecimals
	default:
		return false
	}
	x, y := t[a[0]:a[1]], t[b[0]:b[1]]
	if decimals(x) < need || decimals(y) < need {
		return false
	}
	ix, iy := intPart(x), intPart(y)
	inRange := (ix <= 90 && iy <= 180) || (ix <= 180 && iy <= 90)
	return inRange && max(ix, iy) >= minCoordMagnitude
}

// beforeNum：数前不能紧邻字母、数字或小数点（pH6.512 的 6.512 不是坐标，1234.5678 不切出 234.5678）。
func beforeNum(t string, start int) bool {
	if start == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(t[:start])
	return !isASCIIAlnum(r) && r != '.'
}

// afterNum：数后不能紧邻数字或小数点。
func afterNum(t string, end int) bool {
	return end >= len(t) || !isNumChar(t[end])
}

// truncateNums 把片段中的每个小数截断到 logDecimals 位，保留分隔符。
func truncateNums(span string) string {
	return coordNumRe.ReplaceAllStringFunc(span, TruncateCoord)
}

// splitNumbers 识别带空格 / 连字符分隔的号码：去掉分隔符后仍须是敏感号码，输出不带分隔符的掩码。
func splitNumbers(t string, re *regexp.Regexp, es *edits) {
	for _, m := range re.FindAllStringIndex(t, -1) {
		start, end := m[0], m[1]
		if start > 0 && isNumChar(t[start-1]) || end < len(t) && isAlnumOrDot(t[end]) {
			continue
		}
		digits := strings.NewReplacer("-", "", " ", "").Replace(t[start:end])
		if sensitiveDigits(digits) {
			es.add(start, end, maskDigits(digits))
		}
	}
}

// cnMobiles 识别中文数字写的手机号（前后不能紧邻中文数字或半角数字）。
func cnMobiles(t string, es *edits) {
	for _, m := range cnMobileRe.FindAllStringIndex(t, -1) {
		start, end := m[0], m[1]
		if r, _ := utf8.DecodeLastRuneInString(t[:start]); start > 0 && isAnyDigit(r) {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(t[end:]); end < len(t) && isAnyDigit(r) {
			continue
		}
		var b strings.Builder
		for _, r := range t[start:end] {
			b.WriteByte(cnDigits[r])
		}
		es.add(start, end, maskDigits(b.String()))
	}
}

func isAnyDigit(r rune) bool { _, ok := cnDigits[r]; return ok || r >= '0' && r <= '9' }

// digitTokens 逐个字母数字串处理：摘要整体跳过，其余逐个数字串判断。
func digitTokens(t string, es *edits) {
	for _, m := range alnumRe.FindAllStringIndex(t, -1) {
		start, end := m[0], m[1]
		if !es.free(start, end) {
			continue
		}
		maskToken(t, start, end, es)
	}
}

// maskToken 处理 t[start:end] 这个字母数字串中的数字串；紧挨小数点的数字串（小数部分 / 带小数的整数部分）跳过。
func maskToken(t string, start, end int, es *edits) {
	tok := t[start:end]
	if isDigest(tok) || !strings.ContainsAny(tok, "0123456789") {
		return
	}
	for _, m := range digitsRe.FindAllStringIndex(tok, -1) {
		rs, re := start+m[0], start+m[1]
		if m[0] == 0 && decimalPoint(t, rs-1, -1) || m[1] == len(tok) && decimalPoint(t, re, 1) {
			continue
		}
		run := t[rs:re]
		// 17 位 + X 是身份证，X 并入。
		if len(run) == 17 && re < end && (t[re] == 'X' || t[re] == 'x') {
			re++
			run = t[rs:re]
		}
		if sensitiveDigits(run) {
			es.add(rs, re, maskDigits(run))
		}
	}
}

// decimalPoint 判断 t[i] 是小数点且其另一侧（dir 方向）紧邻数字。
func decimalPoint(t string, i, dir int) bool {
	j := i + dir
	return i >= 0 && i < len(t) && t[i] == '.' && j >= 0 && j < len(t) && t[j] >= '0' && t[j] <= '9'
}

// isDigest 判断是否为 md5 / sha1 / sha256 十六进制摘要（长度精确匹配且含 a-f，纯数字串不算）。
func isDigest(tok string) bool {
	return slices.Contains(digestLens, len(tok)) && isHex(tok) && strings.ContainsAny(strings.ToLower(tok), "abcdef")
}

// sensitiveDigits 判断一个数字串（身份证可带末位 X）是否为手机号 / 身份证 / 账号。
func sensitiveDigits(run string) bool {
	switch n := len(run); {
	case n == 11:
		return isMobile(run)
	case n == 13:
		return strings.HasPrefix(run, "86") && isMobile(run[2:])
	case n >= 15 && n <= 19:
		return true
	default:
		return false
	}
}

func isMobile(s string) bool { return len(s) == 11 && s[0] == '1' && s[1] >= '3' && s[1] <= '9' }

// maskDigits 保留前 3 位与后 4 位，其余替换为 *（长度不变；入参为半角数字串，身份证末位可为 X）。
func maskDigits(s string) string {
	const head, tail = 3, 4
	if len(s) <= head+tail {
		return strings.Repeat("*", len(s))
	}
	return s[:head] + strings.Repeat("*", len(s)-head-tail) + s[len(s)-tail:]
}

// TruncateCoord 把十进制数字串截断到 0.01°（不四舍五入，不经浮点）；非 x.y 形态原样返回。
func TruncateCoord(num string) string {
	i := strings.IndexByte(num, '.')
	if i < 0 || len(num)-i-1 <= logDecimals {
		return num
	}
	return num[:i+1+logDecimals]
}

func decimals(num string) int {
	i := strings.IndexByte(num, '.')
	if i < 0 {
		return 0
	}
	return len(num) - i - 1
}

// intPart 返回小数的整数部分绝对值（入参已由 coordNumRe 限定为 ≤3 位整数）。
func intPart(num string) int {
	ip, _, _ := strings.Cut(strings.TrimPrefix(num, "-"), ".")
	n := 0
	for _, c := range ip {
		n = n*10 + int(c-'0')
	}
	return n
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func isNumChar(c byte) bool { return c >= '0' && c <= '9' || c == '.' }

func isAlnumOrDot(c byte) bool { return isNumChar(c) || isASCIIAlnum(rune(c)) }

func isASCIIAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
