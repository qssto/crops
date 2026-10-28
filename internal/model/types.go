package model

import (
	"encoding/json"
	"time"
)

// EventType 是 run_event.event_type（P0 L7 流程 6 的枚举），写入时用 Valid 校验，新增类型先登记。
type EventType string

const (
	EventRunStarted       EventType = "RUN_STARTED"
	EventStepStarted      EventType = "STEP_STARTED"
	EventStepEnded        EventType = "STEP_ENDED"
	EventToolCallEnd      EventType = "TOOL_CALL_END"
	EventKBLoaded         EventType = "KB_LOADED"
	EventLLMCallEnd       EventType = "LLM_CALL_END"
	EventValidationFailed EventType = "VALIDATION_FAILED"
	EventDegraded         EventType = "DEGRADED"
	EventAdviceReady      EventType = "ADVICE_READY"
	EventApprovalOpen     EventType = "APPROVAL_OPEN"
	EventApprovalPending  EventType = "APPROVAL_PENDING"
	EventApprovalDecided  EventType = "APPROVAL_DECIDED"
	EventOrderTransition  EventType = "ORDER_TRANSITION"
	EventDispatched       EventType = "DISPATCHED"
	EventRunInterrupted   EventType = "RUN_INTERRUPTED"
	EventRunResumed       EventType = "RUN_RESUMED"
	EventRunCompleted     EventType = "RUN_COMPLETED"
	EventRunFailed        EventType = "RUN_FAILED"
)

var eventTypes = map[EventType]struct{}{
	EventRunStarted: {}, EventStepStarted: {}, EventStepEnded: {}, EventToolCallEnd: {},
	EventKBLoaded: {}, EventLLMCallEnd: {}, EventValidationFailed: {}, EventDegraded: {},
	EventAdviceReady: {}, EventApprovalOpen: {}, EventApprovalPending: {}, EventApprovalDecided: {},
	EventOrderTransition: {}, EventDispatched: {}, EventRunInterrupted: {}, EventRunResumed: {},
	EventRunCompleted: {}, EventRunFailed: {},
}

// Valid 判断事件类型是否已登记。
func (t EventType) Valid() bool {
	_, ok := eventTypes[t]
	return ok
}

// RunEvent 是一条待追加的 run_event；seq 由 EventStore.AppendTx 分配。
type RunEvent struct {
	TenantID   uint64
	FarmID     uint64
	RunID      uint64
	StepID     string  // 空串写 NULL
	ToolCallID *uint64 // nil 写 NULL
	Type       EventType
	Actor      ActorType
	Payload    json.RawMessage // 只含控制字段与脱敏摘要，≤ 8KB
	CreatedAt  time.Time
}

// RawPut 是 RawStore.Put 的入参。对象 key 为
// raw/{tenant}/{farm}/{yyyy}/{mm}/{dd}/{run_id 或 0}/{kind}/{name}（母文档 §4.6.2-8，{id} 段由调用方给出稳定名，
// 重跑写同一 key 即覆盖同一对象）；日期取 At（UTC），调用方传业务时间以保证重跑 key 不变。
type RawPut struct {
	TenantID    uint64
	FarmID      uint64
	RunID       uint64 // 0 表示不关联 run
	Kind        RawKind
	Name        string
	At          time.Time
	Body        []byte
	ContentType string
}

// RawObject 是 raw_object 表行。
type RawObject struct {
	ID        uint64
	TenantID  uint64
	FarmID    uint64
	RunID     *uint64
	Kind      RawKind
	ObjectKey string
	SHA256    string
	SizeBytes int64
	CreatedAt time.Time
}

// OrderRef 定位一张作业单；所有查询都带 tenant_id / farm_id。
type OrderRef struct {
	TenantID uint64
	FarmID   uint64
	OrderID  uint64
}

// Scope 是鉴权中间件从 token 与静态配置解析出的身份范围（母文档 §4.6.2-1）。
type Scope struct {
	TenantID uint64 `json:"tenant_id"`
	FarmID   uint64 `json:"farm_id"`
	UserID   uint64 `json:"user_id"`
	Roles    []Role `json:"roles"`
}

// Grant 是一个用户在某农场上的授权（configs/auth.yaml 的一项；P1 起对应 user_role 行）。
type Grant struct {
	TenantID uint64 `json:"tenant_id"`
	FarmID   uint64 `json:"farm_id"`
	Roles    []Role `json:"roles"`
}

// HasRole 判断 Scope 是否含指定角色。
func (s Scope) HasRole(r Role) bool {
	for _, have := range s.Roles {
		if have == r {
			return true
		}
	}
	return false
}
