package model

import (
	"encoding/json"
	"strconv"
	"time"
)

// P2 L6：长对话压缩 compaction_state（contracts/compaction_state.schema.json，由 contracts_test 双向校验）
// 与会话原料。facts 只由代码从业务表与结构化状态提取，不交模型改写（P2 §6.7 功能任务 3）。

// CompactionVersion 是当前识别的 compaction_state 版本；其他取值视为无压缩、用完整上下文（技术实现 7）。
const CompactionVersion = 1

// CompactionState 是 agent_run.compaction_state 与检查点 payload 中的压缩状态。
type CompactionState struct {
	CompactionVersion int             `json:"compaction_version"`
	Facts             CompactionFacts `json:"facts"`
	// Summary 是 SummarizedUntilStepID 及之前轮次自由文本的摘要；未压缩过为 nil。
	Summary               *CompactionSummary `json:"summary,omitempty"`
	SummarizedUntilStepID string             `json:"summarized_until_step_id,omitempty"`
	// RawRefs 是被压缩原文的 raw_object.id（kind=COMPACTED_MSGS），按写入顺序。
	RawRefs []uint64            `json:"raw_refs,omitempty"`
	History []CompactionHistory `json:"history,omitempty"`
}

// CompactionFacts 是压缩不得丢弃、不得改写的白名单字段（红线）。
type CompactionFacts struct {
	FieldID              uint64                `json:"field_id"`
	Crop                 string                `json:"crop,omitempty"`
	PhenologyStage       string                `json:"phenology_stage,omitempty"`
	UserConstraints      []UserConstraint      `json:"user_constraints,omitempty"`
	Orders               []SessionOrder        `json:"orders,omitempty"`
	VetoedProducts       []VetoedProduct       `json:"vetoed_products,omitempty"`
	Measured             []MeasuredFact        `json:"measured,omitempty"`
	ConfirmedConclusions []ConfirmedConclusion `json:"confirmed_conclusions,omitempty"`
	CitedKBIDs           []string              `json:"cited_kb_ids,omitempty"`
}

// UserConstraint 是用户口述约束 / 禁忌原句（命中 policy/user_constraint_lexicon.yaml），TurnStepID 为所在轮次。
type UserConstraint struct {
	Text       string `json:"text"`
	TurnStepID string `json:"turn_step_id"`
}

// SessionOrder 是本会话作业单与其最新票据、驳回原因；AdviceID 为作业单所出自的建议卡
// （已驳回 / 已取消单对应的结论据此从 confirmed_conclusions 排除）。
type SessionOrder struct {
	OrderID          uint64          `json:"order_id"`
	AdviceID         uint64          `json:"advice_id,omitempty"`
	Status           WorkOrderStatus `json:"status"`
	TicketID         uint64          `json:"ticket_id,omitempty"`
	TicketStatus     TicketStatus    `json:"ticket_status,omitempty"`
	RejectReasonCode string          `json:"reject_reason_code,omitempty"`
	RejectText       string          `json:"reject_text,omitempty"`
}

// 否决药剂来源（vetoed_products[].source）：
//   - VetoedProductRejected：审批以药剂不对（PRODUCT）驳回，ref_id 为票据 id；
//   - VetoedProductUserCanceled：用户 / 审批人中止作业单（进入 CANCELED 的迁移原因为 CANCELED_BY_USER），
//     ref_id 为作业单 id。这是业务表里唯一可读的「用户否决」信号；口述的「不能用某药」走 user_constraints。
const (
	VetoedProductRejected     = "REJECTED_ORDER"
	VetoedProductUserCanceled = "USER_CANCELED"
)

// VetoedProduct 是已否决药剂。
type VetoedProduct struct {
	Product string `json:"product"`
	Source  string `json:"source"`
	RefID   uint64 `json:"ref_id"`
}

// MeasuredFact 是时效实测（tool_call time_sensitive=1 且未被取代）的摘要与取数时间。
type MeasuredFact struct {
	ToolCallID    uint64          `json:"tool_call_id"`
	ToolName      string          `json:"tool_name"`
	ResultSummary json.RawMessage `json:"result_summary"`
	CollectedAt   time.Time       `json:"collected_at"`
}

// ConfirmedConclusion 是已落卡（outcome=ADVICE）的结论正文、剂量、药剂与作业窗口。
type ConfirmedConclusion struct {
	AdviceID        uint64      `json:"advice_id"`
	Conclusion      string      `json:"conclusion"`
	Numbers         []Number    `json:"numbers,omitempty"`
	SelectedProduct string      `json:"selected_product,omitempty"`
	Window          *TimeWindow `json:"window,omitempty"`
}

// CompactionSummary 是更早自由文本的摘要；Tokens 为估算值。
type CompactionSummary struct {
	Text          string `json:"text"`
	Tokens        int    `json:"tokens"`
	PromptVersion string `json:"prompt_version"`
}

// CompactionHistory 是一次压缩的前后估算 token 数。
type CompactionHistory struct {
	At           time.Time `json:"at"`
	TokensBefore int       `json:"tokens_before"`
	TokensAfter  int       `json:"tokens_after"`
}

// TurnStepID 是会话轮次的 step id（一个 run 即一轮）。
func TurnStepID(runID uint64) string { return "run-" + strconv.FormatUint(runID, 10) }

// SessionSource 是一次上下文组装从业务表读出的会话原料（repository 读取，compaction 提取与断言共用同一读法）。
// Turns 为本 run 之前的会话轮次（沿 prev_run_id 由早到晚）；其余为本会话全部 run（含本 run）的业务行。
//
// 会话链超过回溯上限被截断时 Truncated 为 true：更早的 run 不进轮次 / 作业单 / 结论，但其用户约束与否决药剂
// 不能静默丢失，按同租户 / 农场 / 地块 / 用户、id 早于链首的 run 从业务表汇总进 EarlierTurns（只取问题）
// 与 EarlierOrders（只用于否决药剂）——会话续接要求同地块同用户，被截掉的链段必在其中。
type SessionSource struct {
	RunID      uint64
	FieldID    uint64
	State      json.RawMessage // 本 run 的 agent_run.compaction_state（续接时复制而来），空为无
	Turns      []SessionTurn
	Cards      []SessionCard
	Orders     []SessionOrderSource
	Measured   []MeasuredFact
	CitedKBIDs []string

	Truncated     bool
	EarlierRuns   int // 汇总的更早 run 数（Truncated 时）
	EarlierTurns  []SessionTurn
	EarlierOrders []SessionOrderSource
}

// SessionTurn 是一轮问答：Question 取 RUN_STARTED（已脱敏），Answer 取该 run 建议卡结论（无卡为空）。
type SessionTurn struct {
	RunID    uint64
	Question string
	Answer   string
}

// SessionCard 是会话内一张建议卡；Window 取其作业单的作业窗口（无单为 nil）。
type SessionCard struct {
	AdviceID        uint64
	RunID           uint64
	Crop            string
	PhenologyStage  string
	Outcome         Outcome
	Conclusion      string
	Numbers         []Number
	SelectedProduct string
	Window          *TimeWindow
}

// SessionOrderSource 是会话内作业单行：SessionOrder 外加对应建议的药剂与进入 CANCELED 的迁移原因
// （work_order_transition.reason_code，未取消为空；否决药剂取数用）。
type SessionOrderSource struct {
	SessionOrder
	RunID        uint64
	Product      string
	CancelReason ReasonCode
}

// RunLinkKind 是新 run 与已有 run 的关联方式。
type RunLinkKind string

const (
	// RunLinkSession 是带 prev_run_id 的续接：须同租户 / 农场 / 地块 / 用户、终态且在 session_idle 内。
	RunLinkSession RunLinkKind = "SESSION"
	// RunLinkSupersede 是复检取代的新 run（superseded_by_run_id）：接替旧 run 在会话链中的位置。
	RunLinkSupersede RunLinkKind = "SUPERSEDE"
)

// RunLink 是建 run 时的会话关联；Idle 只对 RunLinkSession 生效（compaction.session_idle：上一 run 的
// updated_at 须不早于 now - Idle，now 取 repository 注入的时钟）。
type RunLink struct {
	Kind  RunLinkKind
	RunID uint64
	Idle  time.Duration
}
