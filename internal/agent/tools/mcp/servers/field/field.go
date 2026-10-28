// Package field 是 field MCP Server：地块档案、农事记录、病虫调查、土壤检测。
// 地块数据随农事变化且需实时一致，不走缓存，不属时间敏感。
package field

import (
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

// Name 是 Server 名。
const Name = "field"

// 工具全名。
const (
	ToolProfile    = "field.profile"
	ToolHistory    = "field.history"
	ToolPestSurvey = "field.pest_survey"
	ToolSoil       = "field.soil"
)

// 农事类型。
const opKindEnum = `["transplant","fertilize","irrigate","drain","spray","weeding"]`

const operationSchema = `{"type":"object","additionalProperties":false,"required":["date","kind"],"properties":{
"date":` + kit.DateType + `,"kind":{"type":"string","enum":` + opKindEnum + `},
"stage_tag":{"type":"string","description":"农事用途，如 基肥 / 分蘖肥 / 穗肥"},
"product":{"type":"string"},"amount":{"type":"number","minimum":0},"unit":{"type":"string"},
"note":{"type":"string"}}}`

const observationSchema = `{"type":"object","additionalProperties":false,"required":["target","name","metric","value","unit"],"properties":{
"target":{"type":"string","description":"防治对象编码，如 planthopper"},"name":{"type":"string"},
"metric":{"type":"string","description":"与 crop.threshold 指标同名"},"value":{"type":"number","minimum":0},"unit":{"type":"string"}}}`

// Specs 返回 field 的工具声明（mock / real 共用）。
func Specs() []mcp.ToolSpec {
	return []mcp.ToolSpec{
		{
			Name:        ToolProfile,
			Description: "查询地块档案：面积、作物、品种、播种与移栽日期、土壤质地、灌溉方式。",
			InputSchema: kit.Schema(kit.FieldOnlyInput),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,
"required":["field_id","field_code","area_mu","crop","variety","sowing_date","soil_texture","irrigation"],"properties":{` +
				kit.FieldIDOut + `,"field_code":{"type":"string"},"area_mu":{"type":"number","exclusiveMinimum":0,"description":"面积（亩）"},
"crop":{"type":"string"},"variety":{"type":"string"},"sowing_date":` + kit.DateType + `,"transplant_date":` + kit.DateType + `,
"soil_texture":{"type":"string"},"irrigation":{"type":"string"}}}`),
			Level: mcp.LevelRead, Idempotent: true,
		},
		{
			Name:        ToolHistory,
			Description: "查询地块截至今天的农事记录（按日期升序），可按类型与起始日期过滤；无记录返回空列表。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` +
				kit.FieldIDProp + `,"kind":{"type":"string","enum":` + opKindEnum + `,"description":"只看某类农事"},
"since":{"type":"string","format":"date","description":"起始日期 YYYY-MM-DD（含）"}}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","operations"],"properties":{` +
				kit.FieldIDOut + `,"operations":{"type":"array","items":` + operationSchema + `},
"last_fertilize":` + operationSchema + `}}`),
			Level: mcp.LevelRead, Idempotent: true,
		},
		{
			Name:        ToolPestSurvey,
			Description: "查询地块截至今天最近一次病虫害田间调查（可只看某防治对象）；从未调查返回 EMPTY。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` +
				kit.FieldIDProp + `,"target":{"type":"string","description":"防治对象编码，如 planthopper"}}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","survey_date","method","observations"],"properties":{` +
				kit.FieldIDOut + `,"survey_date":` + kit.DateType + `,"method":{"type":"string","description":"调查方法，如 平行跳跃法 100 丛"},
"observations":{"type":"array","items":` + observationSchema + `}}}`),
			Level: mcp.LevelRead, Idempotent: true,
		},
		{
			Name:        ToolSoil,
			Description: "查询地块最近一次土壤检测：pH、有机质、碱解氮、有效磷、速效钾及等级。",
			InputSchema: kit.Schema(kit.FieldOnlyInput),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,
"required":["field_id","sampled_at","ph","organic_matter_g_kg","alkali_n_mg_kg","olsen_p_mg_kg","available_k_mg_kg","n_level"],"properties":{` +
				kit.FieldIDOut + `,"sampled_at":` + kit.DateType + `,"ph":{"type":"number","minimum":0,"maximum":14},
"organic_matter_g_kg":{"type":"number","minimum":0},"alkali_n_mg_kg":{"type":"number","minimum":0},
"olsen_p_mg_kg":{"type":"number","minimum":0},"available_k_mg_kg":{"type":"number","minimum":0},
"n_level":{"type":"string","enum":["低","中","高"],"description":"氮素等级"}}}`),
			Level: mcp.LevelRead, Idempotent: true,
		},
	}
}

// NewReal 构造 real 骨架（未对接上游：待对接极飞 XGEO）。
func NewReal(cfg kit.RealConfig) *kit.RealServer { return kit.NewRealServer(Name, Specs(), cfg) }
