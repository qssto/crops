package llm

// Task 是 JSON 任务名，与 prompts/{version}/tasks/{task}.md 一一对应。
type Task string

// 主链路的三个模型任务。
const (
	TaskClassify Task = "classify"
	TaskChoose   Task = "choose"
	TaskExplain  Task = "explain"
)

// Tasks 是全部任务（加载提示词时逐个校验存在）。
var Tasks = []Task{TaskClassify, TaskChoose, TaskExplain}

// 意图（ClassifyOutput.Intent）：决定规则层走哪条判定分支。
const (
	IntentIdentify    = "identify"     // 识别 / 诊断，只给观察类
	IntentTiming      = "timing"       // 是否需要作业、什么时候
	IntentDosage      = "dosage"       // 药量 / 兑水 / 安全间隔
	IntentProduct     = "product"      // 选药 / 选肥
	IntentSafety      = "safety"       // 混配、防护等安全类知识，只给观察类
	IntentRate        = "rate"         // 肥料用量
	IntentAmount      = "amount"       // 灌水量
	IntentIrrigateNow = "irrigate_now" // 是否现在灌水 / 复水
	IntentDrain       = "drain"        // 晒田 / 排水 / 断水
	IntentDepth       = "depth"        // 水层管理
	IntentAfterSpray  = "after_spray"  // 施药后水层管理
	IntentRespond     = "respond"      // 灾害应对
)

// 槽位取值。
const (
	SprayerKnapsack = "knapsack"
	SprayerUAV      = "uav"

	ProductUnknown = "unknown" // 问题里点名了知识库 / calc 不支持的肥料（如农家肥）

	SplitFirst  = "first"
	SplitSecond = "second"

	NutrientN  = "n"
	NutrientK  = "k"
	NutrientZn = "zn"

	HazardHeat    = "heat"
	HazardCold    = "cold"
	HazardTyphoon = "typhoon"
	HazardFlood   = "flood"
	HazardLodging = "lodging"
)

// ClassifyInput 是 classify 任务的输入。Categories / Topics 来自 question_taxonomy，Targets 来自登记表 targets。
type ClassifyInput struct {
	Question   string            `json:"question"`
	Categories []string          `json:"categories"`
	Topics     []string          `json:"topics"`
	Targets    map[string]string `json:"targets"` // 防治对象 id → 中文名
}

// ClassifyOutput 是 classify 的回复。InScope=false 表示超出本农场水稻业务（走 OUT_OF_SCOPE）；
// Unknown=true 表示在本业务内但问题主体不在知识覆盖内（走 KB_MISS，不得用模型记忆作答）。
type ClassifyOutput struct {
	InScope  bool   `json:"in_scope"`
	Unknown  bool   `json:"unknown,omitempty"`
	Category string `json:"category,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Target   string `json:"target,omitempty"` // 防治对象 id（仅 protect）
	Intent   string `json:"intent,omitempty"`
	Slots    Slots  `json:"slots"`
}

// Slots 是从问题中抽取的作业参数（不是事实：长势、虫情、水层等测量值一律以工具为准）。
type Slots struct {
	Sprayer       string   `json:"sprayer,omitempty"`         // knapsack / uav
	FertStage     string   `json:"fert_stage,omitempty"`      // basal / tillering / panicle_promote / panicle_protect
	FertProduct   string   `json:"fert_product,omitempty"`    // urea / compound / unknown
	Split         bool     `json:"split,omitempty"`           // 分蘖肥分两次
	SplitPart     string   `json:"split_part,omitempty"`      // first / second：只问其中一次
	Nutrient      string   `json:"nutrient,omitempty"`        // 缺素诊断的疑似元素 n / k / zn
	Hazard        string   `json:"hazard,omitempty"`          // 灾害种类 heat / cold / typhoon / flood / lodging
	Foliar        bool     `json:"foliar,omitempty"`          // 叶面喷施
	TargetDepthCm *float64 `json:"target_depth_cm,omitempty"` // 目标水层（用户意图，不是测量值）
	Dispatch      bool     `json:"dispatch,omitempty"`        // 明确要求安排作业
	DilutionRatio *float64 `json:"dilution_ratio,omitempty"`  // 用户给定的稀释倍数（浸种等按倍数登记的药剂）
	WaterL        *float64 `json:"water_l,omitempty"`         // 用户给定的药液量（升）
	BagKg         *float64 `json:"bag_kg,omitempty"`          // 肥料每袋规格（公斤），问"多少袋"时给出
}

// Candidate 是一个药剂候选（规则层给出，模型只能在其中选择）。
type Candidate struct {
	ID         string `json:"id"`
	CommonName string `json:"common_name"`
}

// ChooseInput 是 choose 任务的输入。
type ChooseInput struct {
	Question   string      `json:"question"`
	Target     string      `json:"target"`
	Candidates []Candidate `json:"candidates"`
}

// ChooseOutput 是 choose 的回复：ProductID 必须是 Candidates 中的 id。
type ChooseOutput struct {
	ProductID string `json:"product_id"`
}

// NumberSlot 是 explain 可引用的一个数值占位符；数值与单位由 calc 回填，模型看不到具体值。
type NumberSlot struct {
	Placeholder string `json:"placeholder"` // {{n0}}
	Field       string `json:"field"`
	Label       string `json:"label"` // 中文名，不含数字
}

// ExplainInput 是 explain 任务的输入：规则层已定结论，模型只组织语言。
type ExplainInput struct {
	Question   string       `json:"question"`
	Category   string       `json:"category"`
	AdviceType string       `json:"advice_type"`
	Decision   string       `json:"decision"` // 规则层判定摘要（无数字）
	Reasons    []string     `json:"reasons"`  // 规则层依据摘要（无数字）
	Product    string       `json:"product,omitempty"`
	Numbers    []NumberSlot `json:"numbers,omitempty"`
}

// ExplainOutput 是 explain 的回复；数字只能以 {{nK}} 占位符出现。
type ExplainOutput struct {
	Conclusion string   `json:"conclusion"`
	Reasons    []string `json:"reasons"`
}
