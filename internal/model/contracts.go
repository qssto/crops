package model

import (
	"encoding/json"
	"time"
)

// 五个核心契约 v1（母文档 §5.0.5 L1）。json tag 即 contracts/*.schema.json 的 properties，
// 由 contracts_test.go 双向校验；带 omitempty 的字段为可选，其余为 required。

// AdviceCard 是 Agent 产出的一条结构化建议，必带 Citation，不含执行动作。
// outcome / refuse_reason / degraded 来自 advice_card 表（P0 L4 DDL），拒答时也产出卡片。
type AdviceCard struct {
	AdviceID          uint64      `json:"advice_id"`
	RunID             uint64      `json:"run_id"`
	TenantID          uint64      `json:"tenant_id"`
	FarmID            uint64      `json:"farm_id"`
	FieldID           uint64      `json:"field_id"`
	Crop              string      `json:"crop"`
	PhenologyStage    string      `json:"phenology_stage,omitempty"`
	Category          Category    `json:"category"`
	AdviceType        AdviceType  `json:"advice_type,omitempty"`
	Outcome           Outcome     `json:"outcome"`
	RefuseReason      string      `json:"refuse_reason,omitempty"`
	Degraded          bool        `json:"degraded"`
	Conclusion        string      `json:"conclusion"`
	Reasons           []string    `json:"reasons"`
	ProductCandidates []string    `json:"product_candidates,omitempty"` // 规则层给出的药剂候选集合
	SelectedProduct   string      `json:"selected_product,omitempty"`   // 必须 ∈ ProductCandidates（L5 校验）
	Numbers           []Number    `json:"numbers,omitempty"`            // 由规则层 calc 计算
	Citations         []Citation  `json:"citations"`
	Confidence        float64     `json:"confidence"`
	SuggestedWindow   *TimeWindow `json:"suggested_window,omitempty"`
	NextAction        NextAction  `json:"next_action"`
}

// Number 是 calc 给出的一个数值。
type Number struct {
	Field string  `json:"field"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// TimeWindow 是 UTC 时间窗 [from, to]。
type TimeWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Citation 是依据引用：kb 类带 kb_id + kb_version；measured 类带 data_source + collected_at + value，
// 且必须带 tool_call_id（P0 L4）。claim_path 指向被支撑的卡片字段，如 numbers[0]。
type Citation struct {
	TenantID    uint64          `json:"tenant_id"`
	FarmID      uint64          `json:"farm_id"`
	Type        CitationType    `json:"type"`
	ClaimPath   string          `json:"claim_path,omitempty"`
	KBID        string          `json:"kb_id,omitempty"`
	KBVersion   string          `json:"kb_version,omitempty"`
	DataSource  string          `json:"data_source,omitempty"`
	CollectedAt *time.Time      `json:"collected_at,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	ToolCallID  *uint64         `json:"tool_call_id,omitempty"`
}

// WorkOrder 是由建议卡转化出的可执行作业，必经审批。
// approver / approval_expires_at 取自 approval_ticket（operator_id / expires_at），不在 work_order 冗余存储；
// done_area 对应 work_order.done_area_mu（P1 回填）。run_id、dispatch_mode 来自 work_order 表。
type WorkOrder struct {
	OrderID           uint64            `json:"order_id"`
	AdviceID          uint64            `json:"advice_id"`
	RunID             uint64            `json:"run_id"`
	TenantID          uint64            `json:"tenant_id"`
	FarmID            uint64            `json:"farm_id"`
	FieldID           uint64            `json:"field_id"`
	WorkType          WorkType          `json:"work_type"`
	DeviceType        string            `json:"device_type"`
	Payload           json.RawMessage   `json:"payload"` // 药剂·用量·航线·阀门开度等
	TimeWindow        TimeWindow        `json:"time_window"`
	ConstraintChecks  []ConstraintCheck `json:"constraint_checks"` // 审批前与下发前两次校验结果
	Status            WorkOrderStatus   `json:"status"`
	DispatchMode      DispatchMode      `json:"dispatch_mode"`
	Approver          *uint64           `json:"approver,omitempty"`
	ApprovalExpiresAt *time.Time        `json:"approval_expires_at,omitempty"`
	DispatchResult    json.RawMessage   `json:"dispatch_result,omitempty"`
	DoneArea          *float64          `json:"done_area,omitempty"` // 亩
}

// ConstraintCheck 是一条约束校验结论（constraint_check 表）。
type ConstraintCheck struct {
	Phase     ConstraintPhase `json:"phase"`
	Attempt   int             `json:"attempt"`
	RuleID    string          `json:"rule_id"`
	Passed    bool            `json:"passed"`
	Observed  json.RawMessage `json:"observed"`  // {value, unit, source, collected_at, tool_call_id}
	Threshold json.RawMessage `json:"threshold"` // 带 kb_id@version
}

// RunState 是一次 Agent 运行的可恢复状态，run_id 为全局追踪主键。
type RunState struct {
	RunID            uint64           `json:"run_id"`
	TenantID         uint64           `json:"tenant_id"`
	FarmID           uint64           `json:"farm_id"`
	Steps            []RunStep        `json:"steps"`
	CompletedStepIDs []string         `json:"completed_step_ids"`
	PendingApproval  *PendingApproval `json:"pending_approval,omitempty"`
	ContextSnapshot  json.RawMessage  `json:"context_snapshot,omitempty"` // 超限大对象只存 raw_object 引用
	CompactionState  json.RawMessage  `json:"compaction_state,omitempty"` // Phase 2 启用，P0 恒空
	CreatedAt        time.Time        `json:"created_at"`
}

// RunStep 是 run_step 表的一步。
type RunStep struct {
	StepID    string     `json:"step_id"`
	Node      string     `json:"node"`
	Status    StepStatus `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
}

// PendingApproval 是 RunState 停在审批点时的断点信息。
type PendingApproval struct {
	TicketID uint64 `json:"ticket_id"`
	OrderID  uint64 `json:"order_id"`
}

// ApprovalTicket 是一次人工审批请求，关联 run_id 与 WorkOrder。
// operator 为最终决策人（HIGH 为确认人）；ticket_kind 起的六个字段来自 approval_ticket 表（P0 L5 DDL）。
type ApprovalTicket struct {
	TicketID        uint64         `json:"ticket_id"`
	RunID           uint64         `json:"run_id"`
	TenantID        uint64         `json:"tenant_id"`
	FarmID          uint64         `json:"farm_id"`
	OrderID         uint64         `json:"order_id"`
	TicketKind      TicketKind     `json:"ticket_kind"`
	TicketSeq       int            `json:"ticket_seq"`
	RiskLevel       RiskLevel      `json:"risk_level"`
	RequiredRole    []Role         `json:"required_role"`
	ConfirmRequired bool           `json:"confirm_required"`
	InitiatorID     uint64         `json:"initiator_id"`
	FirstApprover   *uint64        `json:"first_approver,omitempty"`
	Snapshot        TicketSnapshot `json:"snapshot"`
	Status          TicketStatus   `json:"status"`
	Operator        *uint64        `json:"operator,omitempty"`
	ExpiresAt       *time.Time     `json:"expires_at,omitempty"` // PREPARING → OPEN 时写入
	DecidedAt       *time.Time     `json:"decided_at,omitempty"`
	Reason          string         `json:"reason,omitempty"`
}

// TicketSnapshot 指向审批快照：快照本体在 MinIO，决策时比对 digest 防止审批过期内容。
type TicketSnapshot struct {
	Digest      string `json:"digest"`
	RawObjectID uint64 `json:"raw_object_id"`
}
