package backflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"agri-steward/internal/piiclean"
)

// 脱敏（§6.9 技术实现 4）：身份 / 手机号 / 证件号换随机合成值，坐标粗化到 0.01°，farm_id / field_id / user_id 等数值 ID
// 换随机合成码（同一草稿内同值同码，run 的田块即 context.field）。合成值只含小写字母与连字符，pii_clean 不会误报。
// 判定「是不是敏感值」复用 internal/piiclean.Detect，不另写一套规则；脱敏后由 pii_clean 再兜底扫一遍（gate.go）。

// identityKeys 是其下标量整体换合成值的 JSON 键（小写）→ 合成值种类。
var identityKeys = map[string]string{
	"name": "name", "owner": "name", "owner_name": "name", "farmer": "name", "farmer_name": "name",
	"contact": "name", "contact_name": "name", "operator_name": "name", "address": "addr",
	"phone": "phone", "mobile": "phone", "tel": "phone", "telephone": "phone", "contact_phone": "phone",
	"id_card": "id", "idcard": "id", "id_no": "id", "id_number": "id",
}

// coordKeys 是其下数值按坐标粗化的 JSON 键（小写，同 piiclean 的坐标键，另加 gps）。
var coordKeys = map[string]bool{"lat": true, "lng": true, "lon": true, "latitude": true, "longitude": true,
	"coordinates": true, "location": true, "position": true, "point": true, "geo": true, "gps": true}

var (
	digitGroupRe = regexp.MustCompile(`[0-9]+(?:[- ][0-9]+)*[Xx]?`)
	coordPairRe  = regexp.MustCompile(`-?[0-9]{1,3}\.[0-9]{3,}(?:\s*[,，]\s*|\s+)-?[0-9]{1,3}\.[0-9]{3,}`)
	coordLabelRe = regexp.MustCompile(`(?:北纬|南纬|东经|西经|纬度|经度)\s*[:：]?\s*-?[0-9]{1,3}\.[0-9]{3,}`)
	preciseNumRe = regexp.MustCompile(`-?[0-9]{1,3}\.[0-9]{3,}`)
	cnMobileRe   = regexp.MustCompile(`[一幺][三四五六七八九][〇零一幺二两三四五六七八九]{9}`)
)

// 合成码前缀与随机段长度。
const (
	codeLetters = 8
	refLetters  = 16
	refPrefix   = "bf-"
)

// ErrRandom 表示随机源读取失败（合成码无法生成）。
var ErrRandom = errors.New("backflow: random source failed")

// scrubber 是一份草稿的脱敏器：真实 ID → 合成码的对应只在内存里，用完即弃（草稿与评测侧都不留映射）。
type scrubber struct {
	rnd               io.Reader
	codes             map[string]string // 前缀 + 真实值 → 合成码
	farm, field, user string
	err               error // ReplaceAllStringFunc 回调内的随机源错误
}

// newScrubber 为本 run 的农场 / 田块 / 提问人生成合成码。
func newScrubber(rnd io.Reader, farmID, fieldID, userID uint64) (*scrubber, error) {
	s := &scrubber{rnd: rnd, codes: map[string]string{}}
	var err error
	if s.farm, err = s.idCode("farm_id", strconv.FormatUint(farmID, 10)); err != nil {
		return nil, err
	}
	if s.field, err = s.idCode("field_id", strconv.FormatUint(fieldID, 10)); err != nil {
		return nil, err
	}
	if s.user, err = s.idCode("user_id", strconv.FormatUint(userID, 10)); err != nil {
		return nil, err
	}
	return s, nil
}

// letters 读 n 个随机小写字母。
func (s *scrubber) letters(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.rnd, buf); err != nil {
		return "", fmt.Errorf("%w: %w", ErrRandom, err)
	}
	for i, b := range buf {
		buf[i] = 'a' + b%26
	}
	return string(buf), nil
}

// draftRef 是草稿 ID（bf- + 16 个随机字母，符合样本 id 写法）。
func (s *scrubber) draftRef() (string, error) {
	l, err := s.letters(refLetters)
	if err != nil {
		return "", err
	}
	return refPrefix + l, nil
}

func idPrefix(key string) string {
	switch key {
	case "farm_id":
		return "farm-bf-"
	case "field_id":
		return "field-bf-"
	case "user_id":
		return "u-bf-"
	}
	return "id-bf-"
}

// idCode 返回 key 下真实值 v 的合成码（同前缀同值同码）。
func (s *scrubber) idCode(key, v string) (string, error) {
	prefix := idPrefix(key)
	if c, ok := s.codes[prefix+v]; ok {
		return c, nil
	}
	l, err := s.letters(codeLetters)
	if err != nil {
		return "", err
	}
	s.codes[prefix+v] = prefix + l
	return prefix + l, nil
}

// synthetic 返回一个随机合成值 syn-{kind}-xxxxxxxx。
func (s *scrubber) synthetic(kind string) (string, error) {
	l, err := s.letters(codeLetters)
	if err != nil {
		return "", err
	}
	return "syn-" + kind + "-" + l, nil
}

// isIDKey：id 或 *_id 键（只换数值，字符串 ID 如 product_id 是业务编码，保留）。
func isIDKey(k string) bool { return k == "id" || strings.HasSuffix(k, "_id") }

// numericID 返回数值 ID 的十进制写法（json.Number 整数或纯数字字符串）。
func numericID(v any) (string, bool) {
	switch x := v.(type) {
	case json.Number:
		if _, err := strconv.ParseUint(x.String(), 10, 64); err == nil {
			return x.String(), true
		}
	case string:
		if _, err := strconv.ParseUint(x, 10, 64); err == nil {
			return x, true
		}
	}
	return "", false
}

// value 脱敏一个 json.Decoder(UseNumber) 解出的值；coord 表示处在坐标键之下。json.Number 转回 int64 / float64，
// 使 YAML 草稿里仍是数字。
func (s *scrubber) value(v any, coord bool) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			r, err := s.member(k, val, coord)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			r, err := s.value(val, coord)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case json.Number:
		return number(x, coord), nil
	case string:
		if coord {
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return strconv.FormatFloat(round2(f), 'f', 2, 64), nil
			}
		}
		return s.text(x)
	}
	return v, nil
}

// member 按键名脱敏一个对象成员：身份键整体换合成值，数值 ID 换合成码，坐标键下粗化，其余递归。
// 键名本身不改（语义未知）：键里夹带的敏感信息交给 pii_clean 兜底判 PII_FAILED。
func (s *scrubber) member(k string, val any, coord bool) (any, error) {
	lk := strings.ToLower(k)
	if kind, ok := identityKeys[lk]; ok && scalar(val) {
		return s.synthetic(kind)
	}
	if isIDKey(lk) {
		if id, ok := numericID(val); ok {
			return s.idCode(lk, id)
		}
	}
	return s.value(val, coord || coordKeys[lk])
}

func scalar(v any) bool {
	switch v.(type) {
	case nil, map[string]any, []any:
		return false
	}
	return true
}

// number 把 json.Number 转回 int64 / float64；坐标键下粗化到两位小数。
func number(n json.Number, coord bool) any {
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, err := n.Float64()
	if err != nil {
		return n.String()
	}
	if coord {
		return round2(f)
	}
	return f
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func round2Text(num string) string {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return num
	}
	return strconv.FormatFloat(round2(f), 'f', 2, 64)
}

// text 脱敏自由文本：带标签坐标与精确坐标对粗化到两位小数；手机号（含中文数字写法）、身份证、账号换合成值。
func (s *scrubber) text(str string) (string, error) {
	t := halfWidthDigits(str)
	t = coordLabelRe.ReplaceAllStringFunc(t, func(m string) string { return preciseNumRe.ReplaceAllStringFunc(m, round2Text) })
	t = coordPairRe.ReplaceAllStringFunc(t, func(m string) string {
		if l, ok := piiclean.Detect(m); ok && l.Kind == "coordinate" {
			return preciseNumRe.ReplaceAllStringFunc(m, round2Text)
		}
		return m
	})
	t = cnMobileRe.ReplaceAllStringFunc(t, func(string) string { return s.mustSynthetic("phone") })
	t = digitGroupRe.ReplaceAllStringFunc(t, func(m string) string {
		l, ok := piiclean.Detect(m)
		if !ok || l.Kind == "coordinate" {
			return m
		}
		return s.mustSynthetic(l.Kind)
	})
	if s.err != nil {
		return "", s.err
	}
	return t, nil
}

// mustSynthetic 供正则替换回调用：随机源失败记到 s.err，由 text 返回。
func (s *scrubber) mustSynthetic(kind string) string {
	v, err := s.synthetic(strings.ReplaceAll(kind, "_", "-"))
	if err != nil && s.err == nil {
		s.err = err
	}
	return v
}

// halfWidthDigits 把全角数字换成半角（与 piiclean 判定口径一致）。
func halfWidthDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return '0' + r - '０'
		}
		return r
	}, s)
}
