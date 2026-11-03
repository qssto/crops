package repository

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// 审计哈希用的 JSON 规范化（RFC 8785 JCS 的本项目子集，P1 §6.8 技术实现第 1 条）：
//   - 对象键按 UTF-16 码元升序，无任何空白；
//   - 字符串只转义 " \ 与 U+0000-U+001F（\b \f \n \r \t 用短形式，其余 \u00xx 小写十六进制），其余字符原样输出；
//   - 数字一律按 IEEE-754 双精度解析后按 ES6 Number.prototype.toString 输出（-0 → 0，1e2 → 100，1E30 → 1e+30）；
//     超出 2^53 的整数会按双精度舍入（RFC 8785 的已知限制）：原值与 MySQL 读回值舍入到同一个双精度，哈希仍一致；
//     代价是把这类整数改成舍入到同一双精度的另一个值（如 9007199254740993 → 9007199254740992）校验发现不了。
//     选择记录而非拒绝：拒绝会让含大整数的审计写入失败（封链回滚业务事务）；需要精确比对的大整数由发出方写成字符串；
//   - 重复键取最后一个（与 MySQL JSON 列一致；RFC 8785 要求拒绝，这里为保证写入值与读回值同口径而放宽）；
//   - 输入必须是单个合法 JSON 值，NaN / Infinity 不是合法 JSON，解析即失败。

// ErrNotCanonicalizable 表示输入不是单个合法 JSON 值。
var ErrNotCanonicalizable = errors.New("repository: json not canonicalizable")

// CanonicalJSON 返回 b 的规范化形式。
func CanonicalJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotCanonicalizable, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrNotCanonicalizable)
	}
	var out bytes.Buffer
	if err := encodeCanonical(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// jsonObject 是保留重复键语义（后者覆盖）的对象。
type jsonObject map[string]any

// decodeValue 按 token 读一个 JSON 值（对象解码为 jsonObject，数字保留为 json.Number）。
func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := jsonObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T", kt)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj[k] = val
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	default:
		return t, nil // string / json.Number / bool / nil
	}
}

// encodeCanonical 按规范化规则输出 v（v 是 decodeValue 的结果，或审计行组装时的 string / nil / jsonObject / rawCanonical）。
func encodeCanonical(w *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		w.WriteString("null")
	case bool:
		w.WriteString(strconv.FormatBool(x))
	case string:
		writeCanonicalString(w, x)
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		if err != nil {
			return fmt.Errorf("%w: number %s: %v", ErrNotCanonicalizable, x, err)
		}
		s, err := es6Number(f)
		if err != nil {
			return err
		}
		w.WriteString(s)
	case rawCanonical:
		w.Write(x)
	case []any:
		w.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := encodeCanonical(w, e); err != nil {
				return err
			}
		}
		w.WriteByte(']')
	case jsonObject:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		w.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				w.WriteByte(',')
			}
			writeCanonicalString(w, k)
			w.WriteByte(':')
			if err := encodeCanonical(w, x[k]); err != nil {
				return err
			}
		}
		w.WriteByte('}')
	default:
		return fmt.Errorf("%w: unsupported %T", ErrNotCanonicalizable, v)
	}
	return nil
}

// rawCanonical 是已规范化的 JSON 片段（审计行里的 summary），原样嵌入。
type rawCanonical []byte

// compareUTF16 按 UTF-16 码元比较（RFC 8785 §3.2.3：U+10000 以上字符按代理对排序，与按码点排序不同）。
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// writeCanonicalString 按 RFC 8785 §3.2.2.2 输出字符串。
func writeCanonicalString(w *bytes.Buffer, s string) {
	w.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			w.WriteString(`\"`)
		case '\\':
			w.WriteString(`\\`)
		case '\b':
			w.WriteString(`\b`)
		case '\f':
			w.WriteString(`\f`)
		case '\n':
			w.WriteString(`\n`)
		case '\r':
			w.WriteString(`\r`)
		case '\t':
			w.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(w, `\u%04x`, r)
				continue
			}
			w.WriteRune(r) // 非法 UTF-8 已在解码时替换为 U+FFFD
		}
	}
	w.WriteByte('"')
}

// es6Number 按 ECMAScript Number.prototype.toString（RFC 8785 §3.2.2.3）格式化有限双精度数。
func es6Number(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("%w: non-finite number", ErrNotCanonicalizable)
	}
	if f == 0 {
		return "0", nil // 含 -0
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// 'e' 格式给出最短可往返的有效数字：d.ddde±x
	mant, expStr, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, err := strconv.Atoi(expStr)
	if err != nil {
		return "", fmt.Errorf("%w: format %v: %v", ErrNotCanonicalizable, f, err)
	}
	k, n := len(digits), exp+1 // n：小数点位置（ES6 记号）
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		e := n - 1
		es := "+" + strconv.Itoa(e)
		if e < 0 {
			es = strconv.Itoa(e)
		}
		if k == 1 {
			s = digits + "e" + es
		} else {
			s = digits[:1] + "." + digits[1:] + "e" + es
		}
	}
	return sign + s, nil
}
