package kit

import "encoding/json"

// 各 Server 的 JSON Schema 共用片段（拼接成常量字符串，避免每个工具重复书写）。
const (
	// FieldIDProp 是 field_id 入参：取 field_profile.id，模型入参不带坐标。
	FieldIDProp = `"field_id":{"type":"integer","minimum":1,"description":"地块 ID（field_profile.id）"}`
	// FieldIDOut 是输出里回显的 field_id。
	FieldIDOut = `"field_id":{"type":"integer","minimum":1,"description":"地块 ID"}`
	// SourceOut 是输出里的数据源字段。
	SourceOut = `"source":{"type":"string","minLength":1,"description":"数据源"}`
	// DateTimeType 是 RFC3339 UTC 时刻。
	DateTimeType = `{"type":"string","format":"date-time"}`
	// DateType 是 YYYY-MM-DD 日期。
	DateType = `{"type":"string","format":"date"}`
)

// StageEnum 是水稻生育期大类（与 knowledge/README §3 一致），Stages 是其 Go 形式，单测保证二者一致。
const StageEnum = `["秧苗期","返青期","分蘖期","拔节孕穗期","抽穗扬花期","灌浆结实期","成熟期"]`

// Stages 是生育期大类（按先后顺序）。
var Stages = []string{"秧苗期", "返青期", "分蘖期", "拔节孕穗期", "抽穗扬花期", "灌浆结实期", "成熟期"}

// FieldOnlyInput 是只带 field_id 的入参 schema。
const FieldOnlyInput = `{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` + FieldIDProp + `}}`

// Schema 把常量 schema 字符串转成 json.RawMessage。
func Schema(s string) json.RawMessage { return json.RawMessage(s) }
