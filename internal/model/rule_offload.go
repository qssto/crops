package model

import (
	"encoding/json"
	"strings"
	"time"
)

// ---- P2 L4 规则与模型职责再切分：下沉项、影子比对差异类型与 rule_shadow_diff 行 ----

// RuleOffloadItem 是下沉项名（rule_shadow_diff.item，≤32 字符；与开关 rule_offload.<项名> 一一对应）。
type RuleOffloadItem string

const (
	RuleItemOperationWindow RuleOffloadItem = "operation_window"
	RuleItemTankMix         RuleOffloadItem = "tank_mix"
	RuleItemPestPriority    RuleOffloadItem = "pest_priority"
)

// ruleOffloadPrefix 是下沉项开关键前缀。
const ruleOffloadPrefix = "rule_offload."

// RuleOffloadItems 是三个下沉项（顺序同 RuleOffloadFlags）。
func RuleOffloadItems() []RuleOffloadItem {
	return []RuleOffloadItem{RuleItemOperationWindow, RuleItemTankMix, RuleItemPestPriority}
}

// Flag 是下沉项的开关键。
func (i RuleOffloadItem) Flag() FlagKey { return FlagKey(ruleOffloadPrefix + string(i)) }

// RuleOffloadItemOf 由开关键取下沉项名；不是 rule_offload.* 键时返回 false。
func RuleOffloadItemOf(k FlagKey) (RuleOffloadItem, bool) {
	s, ok := strings.CutPrefix(string(k), ruleOffloadPrefix)
	return RuleOffloadItem(s), ok && s != ""
}

// RuleDiffKind 是影子比对的差异类型（rule_shadow_diff.diff_kind；一致时为空，写 NULL）。
type RuleDiffKind string

const (
	DiffWindowOutside RuleDiffKind = "WINDOW_OUTSIDE" // 模型给出的作业时间在规则窗口外（或窗口关闭时仍要求作业）
	DiffForbiddenMix  RuleDiffKind = "FORBIDDEN_MIX"  // 模型建议了禁忌混用或超过单桶产品数上限
	DiffOrderMismatch RuleDiffKind = "ORDER_MISMATCH" // 模型的优先防治对象与规则排序首位不一致
)

// RuleShadowDiff 是 rule_shadow_diff 的一行：shadow 模式下规则结论与模型输出的比对结果。
// AdviceID 为 0 表示该轮未出卡（拒答）。RuleOutput / ModelOutput 为 JSON（ModelOutput 是对应部分的掩码摘要）。
type RuleShadowDiff struct {
	TenantID    uint64
	FarmID      uint64
	RunID       uint64
	AdviceID    uint64
	Item        RuleOffloadItem
	Agreed      bool
	DiffKind    RuleDiffKind
	RuleOutput  json.RawMessage
	ModelOutput json.RawMessage
	RuleVer     string
	CreatedAt   time.Time
}

// RuleShadowDiffPayload 是 RULE_SHADOW_DIFF 事件 payload（不投影 audit）。
type RuleShadowDiffPayload struct {
	RunID    uint64          `json:"run_id"`
	Item     RuleOffloadItem `json:"item"`
	Agreed   bool            `json:"agreed"`
	DiffKind RuleDiffKind    `json:"diff_kind,omitempty"`
	RuleVer  string          `json:"rule_ver"`
}
