package model

// Outcome 是一次问答的终态，取值与 advice_card.outcome 一致（L0 自 internal/agent 迁入）。
type Outcome string

const (
	OutcomeAdvice  Outcome = "ADVICE"
	OutcomeRefused Outcome = "REFUSED"
)

// Category 是建议类别，取 eval/taxonomy.yaml 的一级 code。
type Category string

const (
	CategoryProtect   Category = "protect"
	CategoryFertilize Category = "fertilize"
	CategoryIrrigate  Category = "irrigate"
	CategoryDisaster  Category = "disaster"
)

// AdviceType 是建议类型：作业类会转作业单，观察类不建单。取值与 L0 样本一致。
type AdviceType string

const (
	AdviceTypeOperation   AdviceType = "作业类"
	AdviceTypeObservation AdviceType = "观察类"
)

// NextAction 是建议卡给出的下一步（advice_card.next_action）。
type NextAction string

const (
	NextCreateOrder NextAction = "CREATE_ORDER" // 作业类：转作业单草稿
	NextObserve     NextAction = "OBSERVE"      // 观察类：继续观察
	NextNone        NextAction = "NONE"         // 拒答等无后续
)

// CitationType 是依据引用类型。
type CitationType string

const (
	CitationKB       CitationType = "kb"
	CitationMeasured CitationType = "measured"
)

// RunStatus 是 agent_run.status。
type RunStatus string

const (
	RunRunning     RunStatus = "RUNNING"
	RunInterrupted RunStatus = "INTERRUPTED"
	RunCompleted   RunStatus = "COMPLETED"
	RunFailed      RunStatus = "FAILED"
	RunCanceled    RunStatus = "CANCELED"
)

// InterruptType 是 agent_run.interrupt_type（P0 L6 三种中断）。
type InterruptType string

const (
	InterruptWaitApproval InterruptType = "WAIT_APPROVAL"
	InterruptWaitInput    InterruptType = "WAIT_INPUT"
	InterruptSystemError  InterruptType = "SYSTEM_ERROR"
)

// ReasonCode 是终态原因，用于 agent_run.reason_code 与 work_order_transition.reason_code。
// 后续阶段增量见 P0 L7 流程 6，实现到对应阶段时加入。
type ReasonCode string

const (
	ReasonApprovalRejected ReasonCode = "APPROVAL_REJECTED"
	ReasonApprovalExpired  ReasonCode = "APPROVAL_EXPIRED"
	ReasonConditionChanged ReasonCode = "CONDITION_CHANGED"
	ReasonRetryExhausted   ReasonCode = "RETRY_EXHAUSTED"
)

// StepStatus 是 run_step.status。
type StepStatus string

const (
	StepStarted   StepStatus = "STARTED"
	StepCompleted StepStatus = "COMPLETED"
	StepFailed    StepStatus = "FAILED"
	StepSkipped   StepStatus = "SKIPPED"
)

// TicketStatus 是 approval_ticket.status。
type TicketStatus string

const (
	TicketPreparing  TicketStatus = "PREPARING"
	TicketOpen       TicketStatus = "OPEN"
	TicketApproved   TicketStatus = "APPROVED"
	TicketRejected   TicketStatus = "REJECTED"
	TicketExpired    TicketStatus = "EXPIRED"
	TicketSuperseded TicketStatus = "SUPERSEDED"
)

// TicketKind 是 approval_ticket.ticket_kind（母文档 §4.6.2-6），P3 增 PLAN。
type TicketKind string

const (
	TicketKindOrder TicketKind = "ORDER"
	TicketKindPlan  TicketKind = "PLAN"
)

// RiskLevel 是审批风险等级；HIGH 需第二审批人确认。
type RiskLevel string

const (
	RiskLow  RiskLevel = "LOW"
	RiskHigh RiskLevel = "HIGH"
)

// Role 是静态鉴权配置中的角色。
type Role string

const (
	RoleFarmManager   Role = "farm_manager"
	RoleAgronomist    Role = "agronomist"
	RoleOperator      Role = "operator"
	RolePlatformAdmin Role = "platform_admin"
)

// ActorType 是 run_event.actor 与 work_order_transition.actor_type 的取值。
type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorSystem ActorType = "system"
	ActorAgent  ActorType = "agent"
)

// Actor 是一次写操作的发起方；system 的 ID 为 0。
type Actor struct {
	Type ActorType
	ID   uint64
}

// RawKind 是 raw_object.kind。后续阶段增量见 P0 L7 流程 6。
type RawKind string

const (
	RawLLMIO      RawKind = "LLM_IO"
	RawToolRaw    RawKind = "TOOL_RAW"
	RawSnapshot   RawKind = "SNAPSHOT"
	RawCheckpoint RawKind = "CHECKPOINT"
	RawExport     RawKind = "EXPORT"
)

// Valid 判断是否为已登记的 kind。
func (k RawKind) Valid() bool {
	switch k {
	case RawLLMIO, RawToolRaw, RawSnapshot, RawCheckpoint, RawExport:
		return true
	}
	return false
}
