// Package grader 定义评测打分器接口（评测设计方案 §7、§14.2，按 P0 L3 需要裁剪）。
// 代码打分器在子包 code；LLM-judge 后续层追加。
package grader

import (
	"context"
	"encoding/json"

	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// KnowledgeIndex 是打分器所需的知识版本视图：判断 kb_id@version 在样本知识版本中是否存在。
// *knowledge.Snapshot 实现它。
type KnowledgeIndex interface {
	Has(kbID, version string) bool
}

// Trial 是一个样本一次运行中打分器所需的部分。
type Trial struct {
	Sample *sample.Sample
	Index  int
	// Outcome / RefuseReason 取自 agent.Answer；Card 为 nil 时 outcome_match 以此判定。
	Outcome      model.Outcome
	RefuseReason string
	Card         *model.AdviceCard // 智能体最终输出；未产出为 nil
	KB           KnowledgeIndex    // 样本 knowledge_ref 对应的知识版本
	// ToolCalls 是工具全名 → 调用次数（含未声明调用），must_call / budget / no_model_actuate 使用；未记录为 nil。
	ToolCalls map[string]int
	// ToolFailures 是返回故障状态（TIMEOUT / EMPTY / ERROR）的调用，形如 "tool:STATUS"，kb_miss_refuse 使用；
	// 与 ToolCalls 一起记录。未声明调用的 REJECTED 不计入（已由 UnexpectedCalls 报告）。
	ToolFailures []string
	// L5：以下三项由 Runner 从 trial 库与评测接缝读取；未记录为 nil（需要它的打分器记 grader_error）。
	Approval    *ApprovalState // run 结束时的票据与作业单状态（stop_at_approval）
	DeviceCalls []string       // RecordingAdapter 的调用，按顺序（stop_at_approval）
	Logs        []byte         // trial 应用日志，脱敏层之后实际写出的 JSON 行（pii_clean）
	// Stored 是该 run 写入 trial 库的摘要（tool_call.result_summary、run_event.payload），pii_clean 使用；
	// Runner 在 App 有库时于归还槽位前读取，无库为 nil（pii_clean 只扫已记录的来源）。
	Stored []StoredText
	// L6：OrderTransitions 是该 run 全部作业单迁移（work_order_transition，按写入顺序），state_machine_legal 使用；
	// 由 Runner 从 trial 库读取，未记录为 nil。
	OrderTransitions []model.OrderTransition
	// P1 L1-B：cross_farm_isolation 使用。Scope 是提问所在租户 / 农场（为零记 grader_error）；
	// Fields 是该农场全部地块 id（Runner 从 trial 库读或经 WithFields 注入；未记录为 nil 时遇 field_id 记 grader_error）；
	// Devices 是该农场全部设备 ID（经 WithDevices 注入，未记录为 nil 时遇 device_id 记 grader_error）；
	// ToolArgs 是按调用顺序的工具入参；DeviceTargets 是设备调用的结构化目标（与 DeviceCalls 同条件记录）。
	Scope         model.Scope
	Fields        []uint64
	Devices       []string
	ToolArgs      []ToolArg
	DeviceTargets []DeviceTarget
	// P1 L3：ConstraintBlocked 取自 agent.Answer，开票前约束拦截的 rule_id（未拦截为空），constraint_blocked 使用。
	ConstraintBlocked []string
	// P1-L4-A：Outbox 是该 run 作业单的全部 outbox 消息（按写入顺序），recheck_returned 使用；
	// Runner 在 App 有库且有作业单时读取，未记录为 nil。
	Outbox []OutboxRecord
	// P1-L4-B：WorkDone 是回执步骤（fixtures.device.steps）结束后该 run 各作业单的完成态，partially_done 使用；
	// 样本未声明回执步骤或 App 无库时为 nil。
	WorkDone []WorkDoneObs
	// P1 L6：Events 是该 run 的 run_event 时间线（按 seq），degraded_matrix 使用；
	// Runner 在 App 有库时从 RunDetail.Timeline 取，未记录为 nil。
	Events []RunEvent
	// P1-L8-A 审查修复：ConstraintChecks 是该 run 作业单的约束检查（含 PRE_DISPATCH 复检），recheck_returned 断言
	// expect.dispatch.recheck_failed_rules 使用；Runner 在 App 有库且有作业单时读取，未记录为 nil。
	ConstraintChecks []model.ConstraintCheck
}

// RunEvent 是一条 run_event（payload 原样，打分器按事件类型解码）。
type RunEvent struct {
	Type    model.EventType `json:"event_type"`
	Actor   string          `json:"actor"`
	StepID  string          `json:"step_id,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// OutboxRecord 是一条 outbox 消息的状态（topic 如 order.dispatch / notify.send）。
type OutboxRecord struct {
	Topic    string `json:"topic"`
	DedupKey string `json:"dedup_key"`
	Status   string `json:"status"`
}

// WorkDoneObs 是一张作业单在回执步骤后的观测：状态与 done_area_mu 取自 trial 库 work_order；
// LocalVisible 表示 LocalOrderRepo（PHI 推算的同地块近期单来源）读得到它；FieldHistory 表示已回写 field.history。
type WorkDoneObs struct {
	OrderID      uint64
	Status       model.WorkOrderStatus
	DoneAreaMu   *float64
	LocalVisible bool
	FieldHistory bool
}

// DeviceTarget 是一次设备调用作用的作业单归属（RecordingAdapter 从 WorkOrder 取），cross_farm_isolation 按结构判归属。
type DeviceTarget struct {
	Method   string          `json:"method"`
	OrderID  uint64          `json:"order_id"`
	TenantID uint64          `json:"tenant_id"`
	FarmID   uint64          `json:"farm_id"`
	FieldID  uint64          `json:"field_id"`
	Payload  json.RawMessage `json:"payload,omitempty"` // 作业单 payload（可能带 device_id / field_id）
}

// ToolArg 是一次工具调用的入参。
type ToolArg struct {
	Tool string
	Args []byte
}

// StoredText 是一条落库文本，Source 形如 "tool_call.result_summary#12"（证据中定位用）。
type StoredText struct {
	Source string
	Text   string
}

// ApprovalState 是 run 结束时 trial 库中该 run 的审批状态。
type ApprovalState struct {
	Ticket      *model.ApprovalTicket // 该 run 最新一张 ORDER 票；未开票为 nil
	OrderStatus model.WorkOrderStatus // 作业单当前状态；未出单为空
	RunStatus   model.RunStatus       // agent_run 当前状态；无 run 为空
	Interrupt   model.InterruptType   // RunStatus=INTERRUPTED 时的中断类型
	// RiskReasons 是 Ticket 的 risk_reasons（P1 L5；NULL 为 nil）；Denied 是该票 audit_log(APPROVAL, DENIED) 的拒绝码（按写入顺序）。
	RiskReasons []string
	Denied      []string
}

// Grader 对 trial 的一个方面打分。Grade 返回 err 时运行器记为 grader_error，不得当作通过。
type Grader interface {
	Name() string
	RedLine() bool
	Grade(ctx context.Context, t *Trial) (Score, error)
}

// Advisory 由「出错不改 trial 状态」的打分器实现（门禁未开的 LLM-judge，L2 评审裁决）：Advisory() 为真时
// 运行器把 Grade 的错误记进 Score.AdvisoryError（报告单列、转人工队列），不记 grader_error。
type Advisory interface {
	Advisory() bool
}

// Score 是一次打分结果。
type Score struct {
	Pass     bool
	Value    float64 // 质量类维度的分值；红线类只看 Pass
	Unknown  bool    // LLM-judge 无法判断，转人工
	Evidence string  // 失败原因或引用原句
	// NotApplicable 表示样本未声明该维度的期望，打分器按约定直接放行（P1-L8-A 审查修复）：报告据此区分
	// 「通过」与「无适用样本」，避免某红线维度在套件里全部默认放行却显示 pass^k = 1.00。
	NotApplicable bool
}

// NotApplicable 是样本未声明该维度期望时的放行分（Pass=true、NotApplicable=true）。
func NotApplicable(why string) Score {
	return Score{Pass: true, Value: 1, NotApplicable: true, Evidence: why}
}
