// Package device 是 device MCP Server：设备清单与设备实时状态。
// 设备状态时间敏感（恢复/复检换 epoch 重取），不走缓存。
package device

import (
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

// Name 是 Server 名。
const Name = "device"

// 工具全名。
const (
	ToolList   = "device.list"
	ToolStatus = "device.status"
)

const kindEnum = `["drone","sprayer","pump","sensor"]`

const deviceSchema = `{"type":"object","additionalProperties":false,"required":["device_id","kind","model","capabilities"],"properties":{
"device_id":{"type":"string","minLength":1},"kind":{"type":"string","enum":` + kindEnum + `},
"model":{"type":"string"},"capabilities":{"type":"array","items":{"type":"string"},"description":"如 spray / spread / irrigate / soil_moisture"}}}`

// Specs 返回 device 的工具声明（mock / real 共用）。
func Specs() []mcp.ToolSpec {
	return []mcp.ToolSpec{
		{
			Name:        ToolList,
			Description: "列出可服务该地块的农机与传感设备（可按类型过滤），无设备返回空列表。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{` +
				kit.FieldIDProp + `,"kind":{"type":"string","enum":` + kindEnum + `,"description":"只看某类设备"}}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["field_id","devices"],"properties":{` +
				kit.FieldIDOut + `,"devices":{"type":"array","items":` + deviceSchema + `}}}`),
			Level: mcp.LevelRead, Idempotent: true,
		},
		{
			Name:        ToolStatus,
			Description: "查询单台设备的实时状态：在线、作业状态、电量/药箱余量、最近心跳时间。",
			InputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["device_id"],"properties":{
"device_id":{"type":"string","minLength":1,"description":"device.list 返回的设备 ID"}}}`),
			OutputSchema: kit.Schema(`{"type":"object","additionalProperties":false,"required":["device_id","online","state","last_seen_at"],"properties":{
"device_id":{"type":"string"},"online":{"type":"boolean"},
"state":{"type":"string","enum":["idle","working","charging","fault","offline"]},
"battery_pct":{"type":"number","minimum":0,"maximum":100},"tank_l":{"type":"number","minimum":0,"description":"药箱余量（L）"},
"fault_code":{"type":"string"},"last_seen_at":` + kit.DateTimeType + `}}`),
			Level: mcp.LevelRead, Idempotent: true, TimeSensitive: true,
		},
	}
}

// NewReal 构造 real 骨架（未对接上游：待对接极飞 XGEO）。
func NewReal(cfg kit.RealConfig) *kit.RealServer { return kit.NewRealServer(Name, Specs(), cfg) }
