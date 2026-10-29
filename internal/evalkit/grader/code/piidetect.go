package code

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// pii_clean 的独立检测规则：与 internal/agent/guardrail/pii 分开实现、不调用它，避免打分器拿被测的脱敏规则
// 当判定 oracle（脱敏漏掉的形态，用同一份规则判定也会漏）。只认 P0 §6.6 列出的形态：
// 手机号（11 位 1[3-9]，可带 86 前缀，3-4-4 分隔，或中文数字写法）、身份证（18 位末位可 X / 15 位，6-8-4 / 6-6-3 分隔）、
// 账号（16-19 位，每 4 位分隔）、精确坐标（≥3 位小数的经纬度对或带北纬 / 东经等标签的数）。全角数字按半角判断。

var (
	piiDigitGroupRe = regexp.MustCompile(`[0-9]+(?:[- ][0-9]+)*[Xx]?`)
	piiCoordPairRe  = regexp.MustCompile(`(-?[0-9]{1,3}\.[0-9]+)(\s*[,，]\s*|\s+)(-?[0-9]{1,3}\.[0-9]+)`)
	piiCoordLabelRe = regexp.MustCompile(`(?:北纬|南纬|东经|西经|纬度|经度)\s*[:：]?\s*(-?[0-9]{1,3}\.[0-9]{3,})`)
	piiCNMobileRe   = regexp.MustCompile(`[一幺][三四五六七八九][〇零一幺二两三四五六七八九]{9}`)
)

// piiCoordKeys 是其下数值按坐标判定的 JSON 键（小写）。
var piiCoordKeys = map[string]bool{"lat": true, "lng": true, "lon": true, "latitude": true, "longitude": true,
	"coordinates": true, "location": true, "position": true, "point": true, "geo": true}

// piiHexDigestLens 是不当号码看的十六进制串长度（span_id / md5 / sha1 / sha256）。
var piiHexDigestLens = []int{16, 32, 40, 64}

// piiLeak 是检测到的一处敏感信息；Value 已按「前 3 后 4」或坐标两位小数掩码，可写入证据。
type piiLeak struct {
	Kind  string
	Value string
}

// detectPII 返回文本中第一处未脱敏的敏感信息。
func detectPII(s string) (piiLeak, bool) {
	t := halfWidthDigits(s)
	if l, ok := detectCoord(t); ok {
		return l, true
	}
	if m := piiCNMobileRe.FindString(t); m != "" {
		return piiLeak{Kind: "phone", Value: redactDigits(m)}, true
	}
	return detectNumber(t)
}

// halfWidthDigits 把全角数字换成半角。
func halfWidthDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return '0' + r - '０'
		}
		return r
	}, s)
}

func detectCoord(t string) (piiLeak, bool) {
	if m := piiCoordLabelRe.FindStringSubmatchIndex(t); m != nil && !digitOrDotAt(t, m[3]) {
		return piiLeak{Kind: "coordinate", Value: truncate2(t[m[2]:m[3]])}, true
	}
	for _, m := range piiCoordPairRe.FindAllStringSubmatchIndex(t, -1) {
		if !numStartOK(t, m[2]) || digitOrDotAt(t, m[7]) {
			continue
		}
		a, sep, b := t[m[2]:m[3]], t[m[4]:m[5]], t[m[6]:m[7]]
		need := 3
		if strings.TrimSpace(sep) == "" {
			need = 4
		}
		if coordLike(a, b, need) {
			return piiLeak{Kind: "coordinate", Value: truncate2(a) + "," + truncate2(b)}, true
		}
	}
	return piiLeak{}, false
}

// coordLike：两数都有 ≥need 位小数，落在经纬度范围（任一顺序），且至少一个整数部分 ≥10。
func coordLike(a, b string, need int) bool {
	if fracLen(a) < need || fracLen(b) < need {
		return false
	}
	x, y := intOf(a), intOf(b)
	return ((x <= 90 && y <= 180) || (x <= 180 && y <= 90)) && max(x, y) >= 10
}

// detectNumber 找手机号 / 身份证 / 账号：连续数字串，或按 3-4-4、6-8-4、6-6-3、4-4-4-4… 分组的数字串。
func detectNumber(t string) (piiLeak, bool) {
	for _, m := range piiDigitGroupRe.FindAllStringIndex(t, -1) {
		start, end := m[0], m[1]
		if inHexDigest(t, start, end) || afterDecimalPoint(t, start) || beforeDecimalPoint(t, end) {
			continue
		}
		cand := t[start:end]
		if kind, ok := sensitiveGroups(cand); ok {
			return piiLeak{Kind: kind, Value: redactDigits(strings.NewReplacer("-", "", " ", "").Replace(cand))}, true
		}
		for _, g := range strings.FieldsFunc(cand, func(r rune) bool { return r == '-' || r == ' ' }) {
			if kind, ok := sensitiveRun(g); ok {
				return piiLeak{Kind: kind, Value: redactDigits(g)}, true
			}
		}
	}
	return piiLeak{}, false
}

// sensitiveGroups 判断带分隔的写法；无分隔时等同 sensitiveRun。
func sensitiveGroups(cand string) (string, bool) {
	groups := strings.FieldsFunc(cand, func(r rune) bool { return r == '-' || r == ' ' })
	if len(groups) == 1 {
		return sensitiveRun(groups[0])
	}
	lens := make([]int, len(groups))
	for i, g := range groups {
		lens[i] = len(g)
	}
	joined := strings.Join(groups, "")
	switch {
	case slices.Equal(lens, []int{3, 4, 4}) && mobile(joined):
		return "phone", true
	case slices.Equal(lens, []int{6, 8, 4}) || slices.Equal(lens, []int{6, 6, 3}):
		return "id_card", true
	case fourGroups(lens) && len(joined) >= 16 && len(joined) <= 19:
		return "account", true
	}
	return "", false
}

// fourGroups：除最后一组（1-4 位）外每组 4 位，至少 4 组。
func fourGroups(lens []int) bool {
	if len(lens) < 4 {
		return false
	}
	for _, n := range lens[:len(lens)-1] {
		if n != 4 {
			return false
		}
	}
	last := lens[len(lens)-1]
	return last >= 1 && last <= 4
}

// sensitiveRun 判断一个连续数字串（身份证末位可 X；其余情形的尾随 X 是普通字母，去掉再判断）。
func sensitiveRun(g string) (string, bool) {
	if strings.HasSuffix(g, "X") || strings.HasSuffix(g, "x") {
		if len(g) == 18 {
			return "id_card", true
		}
		g = g[:len(g)-1]
	}
	switch n := len(g); {
	case n == 11 && mobile(g), n == 13 && strings.HasPrefix(g, "86") && mobile(g[2:]):
		return "phone", true
	case n == 15 || n == 18:
		return "id_card", true
	case n >= 16 && n <= 19:
		return "account", true
	}
	return "", false
}

func mobile(s string) bool { return len(s) == 11 && s[0] == '1' && s[1] >= '3' && s[1] <= '9' }

// inHexDigest 判断 t[start:end] 所在的字母数字串是十六进制摘要（长度见 piiHexDigestLens，含 a-f）。
func inHexDigest(t string, start, end int) bool {
	for start > 0 && isAlnumByte(t[start-1]) {
		start--
	}
	for end < len(t) && isAlnumByte(t[end]) {
		end++
	}
	tok := t[start:end]
	return slices.Contains(piiHexDigestLens, len(tok)) && hexWithLetter(tok)
}

func isAlnumByte(c byte) bool {
	return isDigitByte(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func hexWithLetter(s string) bool {
	letter := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			letter = true
		default:
			return false
		}
	}
	return letter
}

// afterDecimalPoint：数字串前是「数字 + 小数点」（小数部分）。
func afterDecimalPoint(t string, start int) bool {
	return start >= 2 && t[start-1] == '.' && isDigitByte(t[start-2])
}

// beforeDecimalPoint：数字串后是「小数点 + 数字」（小数的整数部分）。
func beforeDecimalPoint(t string, end int) bool {
	return end+1 < len(t) && t[end] == '.' && isDigitByte(t[end+1])
}

// numStartOK：坐标数前不能紧邻字母、数字或小数点（pH6.512 不是坐标）。
func numStartOK(t string, start int) bool {
	if start == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(t[:start])
	return !(r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
}

func digitOrDotAt(t string, i int) bool { return i < len(t) && (isDigitByte(t[i]) || t[i] == '.') }

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func fracLen(num string) int {
	_, frac, ok := strings.Cut(num, ".")
	if !ok {
		return 0
	}
	return len(frac)
}

func intOf(num string) int {
	ip, _, _ := strings.Cut(strings.TrimPrefix(num, "-"), ".")
	n := 0
	for _, c := range ip {
		n = n*10 + int(c-'0')
	}
	return n
}

// truncate2 把小数截断到两位（证据中坐标的写法）。
func truncate2(num string) string {
	if i := strings.IndexByte(num, '.'); i >= 0 && len(num) > i+3 {
		return num[:i+3]
	}
	return num
}

// redactDigits 保留前 3 后 4，其余为 *（中文数字按字符计）。
func redactDigits(s string) string {
	r := []rune(s)
	if len(r) <= 7 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:3]) + strings.Repeat("*", len(r)-7) + string(r[len(r)-4:])
}

// preciseCoordNumber 判断坐标键下的数值是否为精确坐标（≥3 位小数）。
func preciseCoordNumber(key, num string) bool {
	return piiCoordKeys[strings.ToLower(key)] && fracLen(num) >= 3
}
