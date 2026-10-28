// Package agent 是 Agent 核心（模型、harness、主链路图、知识、calc、护栏）的根包。
// 这里只放对外的最小调用接口：service（L4 起）与 evalkit 都经它调用建议主链路。
package agent

import "context"

// Outcome 是一次问答的终态，取值与 advice_card.outcome 一致。
// L1 冻结契约时迁入 internal/model。
type Outcome string

const (
	OutcomeAdvice  Outcome = "ADVICE"
	OutcomeRefused Outcome = "REFUSED"
)

// Question 是一次提问及其情境。FarmCode / FieldCode 在评测中是样本里的合成编码，
// L1 起由 Scope（tenant_id / farm_id）与 field_profile 解析为 bigint。
type Question struct {
	Text      string
	FarmCode  string
	FieldCode string
	Role      string
	UserID    string
}

// Answer 是结构化结果。L4 起扩展为携带 AdviceCard / WorkOrder 的引用。
type Answer struct {
	Outcome      Outcome
	RefuseReason string
}

// Agent 是建议主链路的最小接口：L0 为只拒答的空实现桩，L4 替换为 compose Graph 主链路。
type Agent interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}
