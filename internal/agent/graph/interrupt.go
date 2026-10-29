package graph

// L6 审批中断与收尾（P0 §6.7 功能任务 1-2）：to_order 之后的 await_approval 在票据非空时以
// compose.StatefulInterrupt 中断，Eino 只在此刻把检查点交给 CheckpointStore（同事务落 run_checkpoint
// 并置 run INTERRUPTED(WAIT_APPROVAL)）。L0 spike：被中断节点在 resume 时重跑、拿到零值输入，
// *state 含不可序列化字段（知识快照、calc），所以续跑所需的最小数据放进 interrupt state（resumeState），
// 已完成节点（classify … to_order）不重跑、不再调工具。finalize 只做摘要与 RUN_RESUMED / RUN_COMPLETED 事件，不调设备。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/model"
)

// L6 新增节点名（run_step.step_id / node）。
const (
	NodeAwaitApproval = "await_approval"
	NodeFinalize      = "finalize"
)

// graphNodes 是编译进图的完整节点顺序：L4 主链路 + 审批中断 + 收尾。
func graphNodes() []string {
	return append(append([]string{}, nodeOrder...), NodeAwaitApproval, NodeFinalize)
}

// resumeStateName 是 resumeState 在 Eino 序列化中的注册名；结构变更须换名（旧检查点不可续，按 graph_version 拦截）。
const resumeStateName = "agri_steward_resume_state_v1"

func init() { schema.RegisterName[resumeState](resumeStateName) }

// resumeState 是交给 StatefulInterrupt 的 interrupt state：只含一个 JSON 字符串。
// Eino 内部序列化器对 json.RawMessage（Citation.Value、WorkOrder.Payload）往返会失败，
// 所以快照先用 encoding/json 编码成字符串，再由 Eino 原样写入检查点。
type resumeState struct {
	Snapshot string
}

// resumeSnapshot 是检查点里的 context_snapshot：续跑收尾所需的最小数据（RunState 契约的 completed_step_ids /
// pending_approval 与建议卡、作业单、票据）。大对象（工具原始返回）不进快照，卡片里只有 tool_call_id 引用。
type resumeSnapshot struct {
	RunID            uint64                `json:"run_id"`
	Scope            model.Scope           `json:"scope"`
	FieldID          uint64                `json:"field_id"`
	GraphVersion     string                `json:"graph_version"`
	CompletedStepIDs []string              `json:"completed_step_ids"`
	Pending          model.PendingApproval `json:"pending_approval"`
	Card             model.AdviceCard      `json:"card"`
	Order            model.WorkOrder       `json:"order"`
	Ticket           model.ApprovalTicket  `json:"ticket"`
	InterruptedAt    time.Time             `json:"interrupted_at"`
}

// errNoResumeState 表示 await_approval 被恢复却拿不到可用的 interrupt state（检查点损坏或版本不符）。
var errNoResumeState = errors.New("graph: resumed without usable interrupt state")

// interrupts 表示本图是否会在审批点中断：需同时配置票据闸门与检查点存储（只配 Gate 时退回 L5 行为，run 直接完成）。
func (a *Agent) interrupts() bool { return a.cfg.Gate != nil && a.cfg.Checkpoints != nil }

// awaitApproval：首次执行时票据非空即中断；恢复执行时（Eino 重跑本节点、输入为零值）从 interrupt state 重建收尾所需状态。
func (a *Agent) awaitApproval(ctx context.Context, s *state) (*state, error) {
	wasInterrupted, hasState, rs := compose.GetInterruptState[resumeState](ctx)
	if wasInterrupted {
		if !hasState {
			return nil, errNoResumeState
		}
		return a.restore(ctx, rs)
	}
	if s.ticket == nil || !a.interrupts() {
		return s, nil
	}
	if err := runner.Alive(ctx); err != nil {
		return nil, err
	}
	snap, err := json.Marshal(a.snapshot(s))
	if err != nil {
		return nil, fmt.Errorf("await_approval: encode snapshot: %w", err)
	}
	return nil, compose.StatefulInterrupt(ctx, string(model.InterruptWaitApproval), resumeState{Snapshot: string(snap)})
}

// snapshot 取续跑所需的最小状态；completed_step_ids 为中断点之前的全部主链路节点。
func (a *Agent) snapshot(s *state) resumeSnapshot {
	return resumeSnapshot{RunID: s.runID, Scope: s.q.Scope, FieldID: s.q.FieldID, GraphVersion: GraphVersion,
		CompletedStepIDs: append([]string{}, nodeOrder...),
		Pending:          model.PendingApproval{TicketID: s.ticket.TicketID, OrderID: s.order.OrderID},
		Card:             *s.card, Order: *s.order, Ticket: *s.ticket, InterruptedAt: s.now}
}

// restore 由快照重建 state（无知识快照 / calc：收尾节点不需要），并发 RUN_RESUMED。
func (a *Agent) restore(ctx context.Context, rs resumeState) (*state, error) {
	var snap resumeSnapshot
	if err := json.Unmarshal([]byte(rs.Snapshot), &snap); err != nil {
		return nil, fmt.Errorf("%w: decode snapshot: %w", errNoResumeState, err)
	}
	if snap.GraphVersion != GraphVersion || snap.RunID == 0 {
		return nil, fmt.Errorf("%w: snapshot run=%d graph_version=%q", errNoResumeState, snap.RunID, snap.GraphVersion)
	}
	s := &state{q: agent.Question{Scope: snap.Scope, FieldID: snap.FieldID, RunID: snap.RunID}, runID: snap.RunID,
		now: a.cfg.Clock.Now(), card: &snap.Card, order: &snap.Order, ticket: &snap.Ticket}
	a.emit(ctx, s, model.EventRunResumed, map[string]any{"ticket_id": snap.Pending.TicketID,
		"order_id": snap.Pending.OrderID, "epoch": runner.Epoch(ctx)})
	return s, nil
}

// finalize 是收尾节点：写摘要日志并发 RUN_COMPLETED；run 的 COMPLETED 由 runner 写入。不调设备、不做复检。
// resumed 以 runner.Epoch(ctx) > 0 判定（只有 Resume 会把 resume_epoch 加一）。
func (a *Agent) finalize(ctx context.Context, s *state) (*state, error) {
	if s.card == nil {
		return nil, errors.New("graph: finalize without card")
	}
	// status / reason 对齐 model.RunEndPayload 契约，其余字段为附加摘要
	summary := map[string]any{"status": model.RunCompleted, "outcome": s.card.Outcome, "advice_id": s.card.AdviceID,
		"resumed": runner.Epoch(ctx) > 0}
	if s.card.RefuseReason != "" {
		summary["refuse_reason"] = s.card.RefuseReason
		summary["reason"] = s.card.RefuseReason
	}
	if s.order != nil {
		summary["order_id"] = s.order.OrderID
	}
	if s.ticket != nil {
		summary["ticket_id"] = s.ticket.TicketID
	}
	a.log.InfoContext(ctx, "run finalized", "run_id", s.runID, "summary", summary)
	a.emit(ctx, s, model.EventRunCompleted, summary)
	return s, nil
}

// emit 发 actor=system 的 run 级事件（RUN_RESUMED / RUN_COMPLETED），见 emitAs。
func (a *Agent) emit(ctx context.Context, s *state, typ model.EventType, payload map[string]any) {
	a.emitAs(ctx, s, model.RunEvent{Type: typ, Actor: model.ActorSystem}, payload)
}
