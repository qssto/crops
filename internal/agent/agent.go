// Package agent 是 Agent 核心（模型、harness、主链路图、知识、calc、护栏）的根包。
// 这里只放对外的最小调用接口：service（L4 起）与 evalkit 都经它调用建议主链路。
package agent

import (
	"context"

	"agri-steward/internal/model"
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
	Outcome      model.Outcome
	RefuseReason string
}

// Agent 是建议主链路的最小接口：L0 为只拒答的空实现桩，L4 替换为 compose Graph 主链路。
type Agent interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// PlaceholderRefuseReason 是占位实现的拒答原因。
const PlaceholderRefuseReason = "NOT_IMPLEMENTED"

// Placeholder 是主链路就绪前（L4 之前）的占位实现：对任何问题都返回 REFUSED / NOT_IMPLEMENTED。
// cmd/server 与 evalkit 在 L4 之前都注入它，L4 替换为 compose Graph 主链路。
type Placeholder struct{}

// Ask 实现 Agent。
func (Placeholder) Ask(context.Context, Question) (Answer, error) {
	return Answer{Outcome: model.OutcomeRefused, RefuseReason: PlaceholderRefuseReason}, nil
}
