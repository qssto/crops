// Package constraint 是开票前 / 下发前的约束校验（P1 L3）：Checker 接口 + 注册表。
// 每个 Checker 只读 Input（工具结果、知识阈值、本地作业单），不做任何 IO；输出一行 constraint_check：
// rule_id / passed / observed{value,unit,source,collected_at,tool_call_id} / threshold{value,unit,kb@version}。
// 数据缺失、过期、无 tool_call_id 一律 passed=false（失败关闭）；阈值只来自知识库条目。
package constraint

import (
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

// RuleIDMax 是 constraint_check.rule_id 的列宽（varchar(48)）。
const RuleIDMax = 48

// 已注册的 rule_id（稳定 code，L4 / L7 按此引用）。
const (
	RuleWind       = "wind_speed_max"
	RuleRain       = "rain_max"
	RuleTemp       = "temp_max"
	RulePHI        = "phi_interval"
	RuleRepeat     = "repeat_operation"
	RuleNoFly      = "no_fly_zone"
	RuleDrift      = "drift_risk"
	RuleBattery    = "device_battery"
	RulePayload    = "device_payload"
	RuleRegistered = "reg_pesticide_registered" // 法规前置条件：药剂在本作物 × 防治对象 × 区域登记
	// RuleNoPlan 是注册表对该作业类型 / 设备没有任何必检项时的合成失败行（空校验也拦截）。
	RuleNoPlan = "no_constraint_plan"
)

// 设备类型（work_order.device_type）。
const (
	DeviceKnapsack = "knapsack"
	DeviceUAV      = "uav"
	DeviceManual   = "manual"
	DeviceValve    = "valve"
)

// 工具名（与 graph / MCP 网关一致）。
const (
	ToolProfile   = "field.profile"
	ToolHistory   = "field.history"
	ToolPhenology = "crop.phenology"
	ToolForecast  = "weather.forecast"
	ToolDevice    = "device.status"
)

// SignoffPending 是阈值所在条目仍为 pending_expert 时 threshold.signoff 的取值。
const SignoffPending = "待农艺专家签字"

// KB 是阈值来源：按 kb_id 取条目（*knowledge.Snapshot 实现）。
type KB interface {
	Entry(kbID string) (*knowledge.Entry, bool)
}

// Source 是一次只读工具调用的结果（graph 由 tool 结果构造）。CollectedAt 取数据 collected_at，
// 缺省为 now − data_freshness_sec；零值表示未知（视为过期）。
type Source struct {
	Tool        string
	Status      string
	ToolCallID  uint64
	Source      string
	CollectedAt time.Time
	Data        map[string]any
}

// Product 是工单所用药剂（植保）。Registered 为该药剂在本作物 × 防治对象 × 区域有登记；
// PHIDays 为登记安全间隔期（nil 表示不适用）；Ref 是登记表引用 common/pesticide_registry@vN。
type Product struct {
	ID         string
	CommonName string
	Registered bool
	PHIDays    *int
	Ref        string
}

// Input 是一次校验的全部输入（只读）。
type Input struct {
	Phase        model.ConstraintPhase
	WorkType     model.WorkType
	DeviceType   string
	DispatchMode model.DispatchMode
	Now          time.Time
	Loc          *time.Location
	KB           KB
	Sources      map[string]Source // 工具名 → 结果
	Product      *Product          // 植保工单所用药剂；nil 表示未选药
	// PHIOf 按农事记录 / 本地单里的药剂名取本作物最长安全间隔期（天）：known=false 表示登记表查不到该药或该药无本作物登记
	// （phi_interval 判失败）；known=true 且 days=nil 表示已登记、本作物 PHI 不适用（不计入最早收获日）。
	PHIOf func(name string) (days *int, known bool)
	// LocalOrders 是本地库同地块近期 REAL 单（agent.LocalOrderSource）；LocalLoaded=false 表示未能读取（重复作业判失败）。
	LocalOrders []agent.LocalOrder
	LocalLoaded bool
	AreaMu      *float64
	// Window 是作业单 time_window：逐日预报按窗口覆盖的日历日取值（零值取 daily[0]）。
	Window model.TimeWindow
	// DeviceID 是作业单 payload.device_id（下发前复检只认这台设备的 device.status；为空即电量 / 载药量判失败）。
	DeviceID string
}

// Observed 是 constraint_check.observed。
type Observed struct {
	Value       any        `json:"value"`
	Unit        string     `json:"unit,omitempty"`
	Source      string     `json:"source,omitempty"`
	CollectedAt *time.Time `json:"collected_at,omitempty"`
	ToolCallID  uint64     `json:"tool_call_id,omitempty"`
	Note        string     `json:"note,omitempty"`
}

// Threshold 是 constraint_check.threshold：kb 为 kb:<kb_id>@<version>。
type Threshold struct {
	KB          string  `json:"kb"`
	ThresholdID string  `json:"threshold_id,omitempty"`
	Op          string  `json:"op,omitempty"`
	Value       float64 `json:"value"`
	Unit        string  `json:"unit,omitempty"`
	Signoff     string  `json:"signoff,omitempty"`
}

// Result 是一条校验结论。
type Result struct {
	Passed    bool
	Observed  Observed
	Threshold Threshold
}

// Checker 是一个约束校验器：纯函数，不做 IO。
type Checker interface {
	RuleID() string
	Check(in *Input) Result
}

// Precondition 由法规前置条件实现（校验顺序在前，且无工具来源，不要求 tool_call_id）。
type Precondition interface {
	Precondition() bool
}

// fail 是失败结论（带说明）。
func fail(th Threshold, obs Observed, note string) Result {
	obs.Note = note
	return Result{Passed: false, Observed: obs, Threshold: th}
}
