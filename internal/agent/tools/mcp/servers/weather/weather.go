// Package weather 是 weather MCP Server：实况、预报、预警、历史。四个工具输出统一的气象记录
// （气温、湿度、风速、降雨、数据时间、数据源），均为时间敏感、可缓存（cache:mcp 10 分钟）。
package weather

import (
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

// Name 是 Server 名。
const Name = "weather"

// 工具全名。
const (
	ToolRealtime = "weather.realtime"
	ToolForecast = "weather.forecast"
	ToolAlert    = "weather.alert"
	ToolHistory  = "weather.history"
)

// 入参边界。
const (
	DefaultForecastDays = 3
	MaxForecastDays     = 7
	DefaultHistoryDays  = 7
	MaxHistoryDays      = 31
	// AlertHorizonDays 是 weather.alert 覆盖的天数（今天起）。
	AlertHorizonDays = 3
)

// recordSchema 是统一气象记录；rain_prob_pct 只在预报中出现。
const recordSchema = `{"type":"object","additionalProperties":false,
"required":["time","temp_c","humidity_pct","wind_speed_ms","rain_mm","source"],"properties":{
"time":{"type":"string","format":"date-time","description":"数据时间（实况为观测时刻，预报/历史为当日 UTC 零点）"},
"temp_c":{"type":"number","description":"气温（℃，日数据为日均）"},
"humidity_pct":{"type":"number","minimum":0,"maximum":100,"description":"相对湿度（%）"},
"wind_speed_ms":{"type":"number","minimum":0,"description":"风速（m/s）"},
"rain_mm":{"type":"number","minimum":0,"description":"降雨量（mm，日数据为日累计）"},
"rain_prob_pct":{"type":"number","minimum":0,"maximum":100,"description":"降雨概率（%，仅预报）"},
` + kit.SourceOut + `}}`

const recordsSchema = `{"type":"array","items":` + recordSchema + `,"description":"按日期升序的日气象记录"}`

// Specs 返回 weather 的工具声明（mock / real 共用）。
func Specs() []mcp.ToolSpec {
	return []mcp.ToolSpec{
		{
			Name:        ToolRealtime,
			Description: "查询地块所在位置的气象实况（最近一次观测）。",
			InputSchema: kit.Schema(kit.FieldOnlyInput),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","observation"],"properties":{` +
				kit.FieldIDOut + `,"observation":` + recordSchema + `}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true, TimeSensitive: true,
		},
		{
			Name:        ToolForecast,
			Description: "查询地块未来若干天的逐日天气预报（喷药、施肥、灌溉窗口判断用）。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` +
				kit.FieldIDProp + `,"days":{"type":"integer","minimum":1,"maximum":7,"default":3,"description":"预报天数（含今天）"}}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","daily"],"properties":{` +
				kit.FieldIDOut + `,"daily":` + recordsSchema + `}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true, TimeSensitive: true,
		},
		{
			Name:        ToolAlert,
			Description: "查询地块今天起 3 天内生效的气象预警（台风、暴雨、高温、低温等），无预警返回空列表。",
			InputSchema: kit.Schema(kit.FieldOnlyInput),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","alerts"],"properties":{` +
				kit.FieldIDOut + `,"alerts":{"type":"array","description":"生效中的预警","items":{"type":"object","additionalProperties":false,
"required":["type","level","title","date","source"],"properties":{
"type":{"type":"string","description":"预警类型"},"level":{"type":"string","enum":["蓝色","黄色","橙色","红色"],"description":"预警等级"},
"title":{"type":"string","description":"预警标题"},"date":{"type":"string","format":"date","description":"生效日期"},` + kit.SourceOut + `}}}}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true, TimeSensitive: true,
		},
		{
			Name:        ToolHistory,
			Description: "查询地块过去一段日期的逐日气象与汇总（雨日数、累计降雨、均温、均湿），默认最近 7 天、最长 31 天。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` +
				kit.FieldIDProp + `,"start_date":{"type":"string","format":"date","description":"起始日期 YYYY-MM-DD（含），默认 end_date 前 6 天"},
"end_date":{"type":"string","format":"date","description":"结束日期 YYYY-MM-DD（含），默认昨天，不得晚于昨天"}}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","daily","summary"],"properties":{` +
				kit.FieldIDOut + `,"daily":` + recordsSchema + `,"summary":{"type":"object","additionalProperties":false,"description":"区间汇总",
"required":["days","rain_days","rain_mm_total","temp_avg_c","humidity_avg_pct"],"properties":{
"days":{"type":"integer","minimum":1},"rain_days":{"type":"integer","minimum":0,"description":"日降雨 >= 0.1mm 的天数"},
"rain_mm_total":{"type":"number","minimum":0},"temp_avg_c":{"type":"number"},"humidity_avg_pct":{"type":"number","minimum":0,"maximum":100}}}}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true, TimeSensitive: true,
		},
	}
}

// NewReal 构造 real 骨架（未对接上游：待对接气象源 D6）。
func NewReal(cfg kit.RealConfig) *kit.RealServer { return kit.NewRealServer(Name, Specs(), cfg) }
