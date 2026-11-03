package service

import (
	"context"
	"encoding/json"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7-B 指标埋点（service 层；repository 不埋点）。Observer 是指标出口，MetricsObserver（metrics_observer.go）实现；
// 各埋点处只依赖它的一个方法。run_id / order_id 不进 label（下钻走 ALERT.* 审计与告警列表）。

// Observer 是业务指标出口。实现必须并发安全、不阻塞、不 panic（指标失败不得影响业务路径）。
type Observer interface {
	DenialCounter
	// AlertFired 计 as_alert_total。
	AlertFired(a model.Alert)
	// EventCommitted 由已提交的 run_event 派生指标：ORDER_TRANSITION（作业 / 下发 / EXPIRED / 复检退回）、
	// DEGRADED（按故障码）、LLM_CALL_END（token）。
	EventCommitted(e model.RunEvent)
	// Dispatched 计一次下发结果（as_dispatch_total）。
	Dispatched(farmID uint64, mode model.DispatchMode, result DispatchOutcome)
	// ApprovalDecided 记一次审批等待时长（开票到终态决定）。
	ApprovalDecided(farmID uint64, decision model.TicketStatus, wait time.Duration)
	// ApprovalBacklog 以全量快照替换 OPEN 票积压 gauge（未出现的农场归零移除）。
	ApprovalBacklog(open map[uint64]int)
	// FlagChanged 计一次开关变更。
	FlagChanged(flag model.FlagKey)
	// Resumed 计一次 run 续跑结果。
	Resumed(ok bool)
	// ToolCalled 计一次 MCP 工具调用（按 server 与 tool_call.status）。
	ToolCalled(tool, status string)
}

// DispatchOutcome 是 as_dispatch_total 的 result 取值（有限枚举）。
type DispatchOutcome string

const (
	DispatchOK       DispatchOutcome = "ok"       // DRY_RUN 写结果 / REAL 平台受理
	DispatchRejected DispatchOutcome = "rejected" // 平台明确拒绝，单 FAILED(DISPATCH_REJECTED)
	DispatchManual   DispatchOutcome = "manual"   // 无法判断是否动作，转人工对账
	DispatchError    DispatchOutcome = "error"    // 设备调用错误 / 回执不合规，尝试保持 SENT 待续跑
	DispatchBlocked  DispatchOutcome = "blocked"  // 设备调用前审批复核不通过（pre_approval_device）
)

// NopObserver 丢弃全部指标（测试与未装配时的缺省）。
type NopObserver struct{}

var _ Observer = NopObserver{}

func (NopObserver) IncDenied(string)                                       {}
func (NopObserver) AlertFired(model.Alert)                                 {}
func (NopObserver) EventCommitted(model.RunEvent)                          {}
func (NopObserver) Dispatched(uint64, model.DispatchMode, DispatchOutcome) {}
func (NopObserver) ApprovalDecided(uint64, model.TicketStatus, time.Duration) {
}
func (NopObserver) ApprovalBacklog(map[uint64]int) {}
func (NopObserver) FlagChanged(model.FlagKey)      {}
func (NopObserver) Resumed(bool)                   {}
func (NopObserver) ToolCalled(string, string)      {}

// observeAfterCommit 在 tx 提交后执行 fn（回滚不执行）。
//
// 无提交钩子时（审查修复 P3 写明）：tx 不是经 repository 事务入口（InTx）开启的——例如 tx 为 nil 的自动提交写、
// 测试直接传入的 *gorm.DB——repository.AfterCommit 返回 false，fn 立即执行。此时语句已自动提交，
// 立即计数与「提交后计数」等价；若调用方在外部自行开了事务却没走 InTx，回滚的事件也会被计入（只影响指标，
// 不影响审计 / 业务数据）。现有调用方都经 InTx，计数只在提交后发生。
func observeAfterCommit(tx *repository.DB, fn func()) {
	if !repository.AfterCommit(tx, func(context.Context) { fn() }) {
		fn()
	}
}

// ObservedEvents 包装 EventStore：事件随事务提交后交给 Observer（回滚的事件不计）。
type ObservedEvents struct {
	Next EventStore
	Obs  Observer
}

var _ EventStore = ObservedEvents{}

// AppendTx 实现 EventStore。
func (o ObservedEvents) AppendTx(ctx context.Context, tx *repository.DB, e model.RunEvent) (int64, error) {
	seq, err := o.Next.AppendTx(ctx, tx, e)
	if err != nil {
		return seq, err
	}
	observeAfterCommit(tx, func() { o.Obs.EventCommitted(e) })
	return seq, nil
}

// ObservedEventSink 包装 agent.EventSink（图内事件各自短事务，Emit 返回即已提交）。
type ObservedEventSink struct {
	Next agent.EventSink
	Obs  Observer
}

var _ agent.EventSink = ObservedEventSink{}

// Emit 实现 agent.EventSink。
func (o ObservedEventSink) Emit(ctx context.Context, e model.RunEvent) (int64, error) {
	seq, err := o.Next.Emit(ctx, e)
	if err == nil {
		o.Obs.EventCommitted(e)
	}
	return seq, err
}

// ObservedTools 包装 agent.ToolProvider：按 tool_call.status 计 MCP 调用（传输层错误计 ERROR）。
type ObservedTools struct {
	Next agent.ToolProvider
	Obs  Observer
}

var _ agent.ToolProvider = ObservedTools{}

// Call 实现 agent.ToolProvider。
func (o ObservedTools) Call(ctx context.Context, scope model.Scope, call agent.ToolCall) (agent.ToolResult, error) {
	res, err := o.Next.Call(ctx, scope, call)
	status := res.Status
	if err != nil {
		status = model.ToolStatusError
	}
	o.Obs.ToolCalled(call.Tool, status)
	return res, err
}

// ObservedResumer 包装 agent.Resumer：计续跑成功 / 失败。
type ObservedResumer struct {
	Next agent.Resumer
	Obs  Observer
}

var _ agent.Resumer = ObservedResumer{}

// Resume 实现 agent.Resumer。
func (o ObservedResumer) Resume(ctx context.Context, scope model.Scope, runID uint64) (agent.Answer, error) {
	ans, err := o.Next.Resume(ctx, scope, runID)
	o.Obs.Resumed(err == nil)
	return ans, err
}

// eventFacts 是从 run_event payload 里取的指标字段（解码失败即不计）。
type eventFacts struct {
	To               model.WorkOrderStatus `json:"to"`
	ReasonCode       model.ReasonCode      `json:"reason_code"`
	Fault            model.FaultCode       `json:"fault"`
	PromptTokens     int                   `json:"prompt_tokens"`
	CompletionTokens int                   `json:"completion_tokens"`
}

func decodeEventFacts(raw json.RawMessage) (eventFacts, bool) {
	var f eventFacts
	if len(raw) == 0 || json.Unmarshal(raw, &f) != nil {
		return f, false
	}
	return f, true
}
