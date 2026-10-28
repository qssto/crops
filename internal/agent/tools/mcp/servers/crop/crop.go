// Package crop 是 crop MCP Server：生育期推算、品种、病虫害谱、防治阈值。
// 农技数据按日变化或静态，可缓存（cache:mcp 24 小时），不属时间敏感。
package crop

import (
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

// Name 是 Server 名。
const Name = "crop"

// 工具全名。
const (
	ToolPhenology    = "crop.phenology"
	ToolVariety      = "crop.variety"
	ToolPestSpectrum = "crop.pest_spectrum"
	ToolThreshold    = "crop.threshold"
)

// targetEnum 是防治对象编码（与 knowledge 阈值条目一致）。
const targetEnum = `["planthopper","leaf_roller","stem_borer","sheath_blight","blast","false_smut","bakanae","weeds"]`

const stageProp = `"stage":{"type":"string","enum":` + kit.StageEnum + `,"description":"生育期大类"}`

const indicatorSchema = `{"type":"object","additionalProperties":false,"required":["metric","op","value","unit"],"properties":{
"metric":{"type":"string","description":"指标名，如 density_per_100_hills"},
"op":{"type":"string","enum":[">=","<="]},"value":{"type":"number"},"unit":{"type":"string"}}}`

// Specs 返回 crop 的工具声明（mock / real 共用）。
func Specs() []mcp.ToolSpec {
	return []mcp.ToolSpec{
		{
			Name:        ToolPhenology,
			Description: "按播种日、有效积温（基温 10℃）与当前日期推算地块作物的生育期与子期。",
			InputSchema: kit.Schema(kit.FieldOnlyInput),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,
"required":["field_id","crop","variety","sowing_date","as_of","days_after_sowing","gdd_base_c","gdd_c_day","stage"],"properties":{` +
				kit.FieldIDOut + `,"crop":{"type":"string"},"variety":{"type":"string"},
"sowing_date":` + kit.DateType + `,"as_of":` + kit.DateType + `,
"days_after_sowing":{"type":"integer","minimum":0},"gdd_base_c":{"type":"number"},
"gdd_c_day":{"type":"number","minimum":0,"description":"播种至今有效积温（℃·d）"},` + stageProp + `,
"sub_stage":{"type":"string","description":"子期，如 分蘖盛期 / 孕穗期；秧苗期、返青期无子期"},
"next_stage":{"type":"string","description":"下一个子期或生育期，成熟末期无"},
"gdd_to_next":{"type":"number","minimum":0,"description":"距下一期尚需积温（℃·d）"}}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true,
		},
		{
			Name:        ToolVariety,
			Description: "查询地块种植品种的特性：类型、全生育期天数、抗性与栽培要点。",
			InputSchema: kit.Schema(kit.FieldOnlyInput),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,
"required":["field_id","crop","variety","type","growth_days","resistance","notes"],"properties":{` +
				kit.FieldIDOut + `,"crop":{"type":"string"},"variety":{"type":"string"},"type":{"type":"string","description":"如 中熟中粳"},
"growth_days":{"type":"integer","minimum":1,"description":"全生育期天数"},
"resistance":{"type":"object","additionalProperties":{"type":"string"},"description":"病害 → 抗性等级"},
"notes":{"type":"array","items":{"type":"string"}}}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true,
		},
		{
			Name:        ToolPestSpectrum,
			Description: "查询地块作物在指定生育期（缺省为当前生育期）需关注的病虫草害谱。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` +
				kit.FieldIDProp + `,` + stageProp + `}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","crop","stage","targets"],"properties":{` +
				kit.FieldIDOut + `,"crop":{"type":"string"},` + stageProp + `,
"targets":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["target","name","kind","risk"],"properties":{
"target":{"type":"string","enum":` + targetEnum + `},"name":{"type":"string"},
"kind":{"type":"string","enum":["pest","disease","weed"]},"risk":{"type":"string","enum":["高","中","低"]}}}}}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true,
		},
		{
			Name:        ToolThreshold,
			Description: "查询水稻某防治对象在某生育期的防治指标（达到任一/全部指标即建议防治）。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["target","stage"],"properties":{
"target":{"type":"string","enum":` + targetEnum + `,"description":"防治对象"},` + stageProp + `}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["crop","target","stage","logic","indicators"],"properties":{
"crop":{"type":"string"},"target":{"type":"string","enum":` + targetEnum + `},` + stageProp + `,
"logic":{"type":"string","enum":["any","all"],"description":"多指标的组合方式"},
"indicators":{"type":"array","minItems":1,"items":` + indicatorSchema + `}}}`),
			Level: mcp.LevelRead, Idempotent: true, Cacheable: true,
		},
	}
}

// NewReal 构造 real 骨架（未对接上游：待对接极飞 XGEO / 农技知识服务）。
func NewReal(cfg kit.RealConfig) *kit.RealServer { return kit.NewRealServer(Name, Specs(), cfg) }
