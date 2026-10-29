package pii

import (
	"bytes"
	"encoding/json"
	"strings"
)

// coordKeys 是按坐标处理的 JSON 键 / 日志属性名（其下的数值，含数组元素，如 "location":[120.56789,31.23456]）。
var coordKeys = map[string]bool{
	"lat": true, "lng": true, "lon": true, "latitude": true, "longitude": true, "coordinates": true,
	"location": true, "position": true, "point": true, "geo": true,
}

// IsCoordKey 判断键名是否为坐标字段（大小写不敏感）。
func IsCoordKey(k string) bool { return coordKeys[strings.ToLower(k)] }

// jsonMode 是一种出口口径：文本怎么掩，坐标数值替换成什么。
type jsonMode struct {
	text  func(string) string
	coord func(json.Number) any
}

var (
	logMode   = jsonMode{text: Mask, coord: func(n json.Number) any { return json.Number(TruncateCoord(n.String())) }}
	modelMode = jsonMode{text: ForModelText, coord: func(json.Number) any { return FieldRef }}
)

// MaskJSON 是落库摘要口径（tool_call.result_summary、run_event.payload）：按结构处理字符串值与键名、
// 坐标键下的数值截断到 0.01°、形似手机号 / 身份证 / 账号的整数改为掩码字符串。
// 输入不是合法 JSON 时返回掩码后文本的 JSON 字符串（保证仍是合法 JSON）；空输入原样返回。
func MaskJSON(b json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(b)) == 0 {
		return b
	}
	out, ok := walkJSON(b, logMode)
	if !ok {
		out, _ = json.Marshal(Mask(string(b))) // string 的 Marshal 不会失败
	}
	return out
}

// ForModel 是模型入参口径：s 是 JSON 时按结构处理（坐标键下的数值替换为 FieldRef），否则按纯文本
// （ForModelText）。手机号 / 身份证 / 账号掩码，精确坐标替换为 FieldRef。
func ForModel(s string) string {
	if out, ok := walkJSON([]byte(s), modelMode); ok {
		return string(out)
	}
	return ForModelText(s)
}

// walkJSON 解码（数字保留为 json.Number）、逐值处理并重新编码；不是单个合法 JSON 值时返回 false。
func walkJSON(b []byte, m jsonMode) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(walk(v, "", m)); err != nil {
		return nil, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

func walk(v any, key string, m jsonMode) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[m.text(k)] = walk(val, k, m)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = walk(val, key, m)
		}
		return out
	case string:
		return m.text(x)
	case json.Number:
		return maskNumber(x, key, m)
	default:
		return v
	}
}

// maskNumber 处理 JSON 数值：坐标键下的精确小数按口径替换；形似敏感号码的整数改为掩码字符串。
func maskNumber(n json.Number, key string, m jsonMode) any {
	s := n.String()
	if IsCoordKey(key) && decimals(s) >= minCoordDecimals {
		return m.coord(n)
	}
	if digitsOnly(s) && sensitiveDigits(s) {
		return maskDigits(s)
	}
	return n
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
