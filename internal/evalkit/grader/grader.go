// Package grader 定义评测打分器接口（评测设计方案 §7、§14.2，按 P0 L3 需要裁剪）。
// 代码打分器在子包 code；LLM-judge 后续层追加。
package grader

import (
	"context"

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
}

// Grader 对 trial 的一个方面打分。Grade 返回 err 时运行器记为 grader_error，不得当作通过。
type Grader interface {
	Name() string
	RedLine() bool
	Grade(ctx context.Context, t *Trial) (Score, error)
}

// Score 是一次打分结果。
type Score struct {
	Pass     bool
	Value    float64 // 质量类维度的分值；红线类只看 Pass
	Unknown  bool    // LLM-judge 无法判断，转人工
	Evidence string  // 失败原因或引用原句
}
