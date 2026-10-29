package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// RunDetailLoader 读一个 run 在各表的原始行，repository.RunDetailRepo 实现。
type RunDetailLoader interface {
	Load(ctx context.Context, scope model.Scope, runID uint64) (repository.RunDetailRows, error)
}

// RunDetail 是 GET /runs/{run_id} 的完整链路（P0 L7 验收 1）：问了什么 → 调了什么 → 依据 → 建议 → 谁审批 → 执行结果，
// 外加 run_event 时间线与 audit_log。run / card 两个键与 L4 的 RunView 一致（向后兼容）。
type RunDetail struct {
	Run      model.RunSummary         `json:"run"`
	Card     *model.AdviceCard        `json:"card,omitempty"`
	Question *model.RunStartedPayload `json:"question,omitempty"` // RUN_STARTED（已脱敏）；L7-A 补发前为空
	// Steps 是三层结构的第二层（run → step → tool_call / llm_call），按首次出现顺序。
	Steps     []StepDetail          `json:"steps"`
	Evidence  Evidence              `json:"evidence"`
	Approvals []TicketDetail        `json:"approvals"`
	Execution Execution             `json:"execution"`
	Timeline  []repository.EventRow `json:"timeline"`
	Audit     []repository.AuditRow `json:"audit"`
	// Warnings 列出无法解析的事件 payload（原样仍在 Timeline），不影响其余字段。
	Warnings []string `json:"warnings,omitempty"`
}

// StepDetail 是一个图节点的执行；Status 等取 run_step，run_step 无行时只由事件推出（Status 为空）。
type StepDetail struct {
	repository.StepRow
	ToolCalls []ToolCallDetail `json:"tool_calls"`
	LLMCalls  []LLMCallDetail  `json:"llm_calls"`
	// EventSeqs 是落在本 step 的其余事件（STEP_* / VALIDATION_FAILED 等）的 seq，详情见 Timeline。
	EventSeqs []int `json:"event_seqs"`
}

// ToolCallDetail 是一次工具调用：tool_call 行 + 对应 TOOL_CALL_END 事件（按 run_event.tool_call_id 关联）。
type ToolCallDetail struct {
	repository.ToolCallRow
	EventSeq *int            `json:"event_seq,omitempty"`
	Event    json.RawMessage `json:"event,omitempty"`
}

// LLMCallDetail 是一次模型调用（LLM_CALL_END）；RawObjectID 取同序的 audit_log(LLM_CALL) 行，
// 原文经 GET /runs/{run_id}/raw/{raw_id} 授权读取。
type LLMCallDetail struct {
	model.LLMCallPayload
	EventSeq    int     `json:"event_seq"`
	RawObjectID *uint64 `json:"raw_object_id,omitempty"`
}

// Evidence 是建议的依据：KB_LOADED 加载的知识条目与建议卡引用。
type Evidence struct {
	KBLoaded  []model.KBLoadedPayload `json:"kb_loaded"`
	Citations []model.Citation        `json:"citations"`
}

// TicketDetail 是一张审批票及其决策记录（按 approval_decision.id 序）。
type TicketDetail struct {
	model.ApprovalTicket
	Decisions []repository.DecisionDetailRow `json:"decisions"`
}

// Execution 是作业单及其迁移，外加 DISPATCHED 事件。
type Execution struct {
	Orders     []OrderDetail         `json:"orders"`
	Dispatches []repository.EventRow `json:"dispatches"`
}

// OrderDetail 是一张作业单及其迁移（按写入顺序）。
type OrderDetail struct {
	repository.OrderRow
	Transitions []TransitionView `json:"transitions"`
}

// TransitionView 是 work_order_transition 一行的 JSON 视图（model.OrderTransition 无 json tag）。
type TransitionView struct {
	From       model.WorkOrderStatus `json:"from"`
	To         model.WorkOrderStatus `json:"to"`
	ActorType  model.ActorType       `json:"actor_type"`
	ActorID    uint64                `json:"actor_id"`
	ReasonCode model.ReasonCode      `json:"reason_code,omitempty"`
	CreatedAt  time.Time             `json:"created_at"`
}

// RunDetailService 组装 run 详情（只读）。
type RunDetailService struct {
	rows RunDetailLoader
}

// NewRunDetailService 构造 RunDetailService。
func NewRunDetailService(rows RunDetailLoader) *RunDetailService {
	return &RunDetailService{rows: rows}
}

// Get 返回 run 的完整链路；run 不在 Scope 内返回 model.ErrNotFound，其余表缺数据不报错。
func (s *RunDetailService) Get(ctx context.Context, scope model.Scope, runID uint64) (RunDetail, error) {
	if runID == 0 {
		return RunDetail{}, fmt.Errorf("%w: run_id required", ErrBadInput)
	}
	rows, err := s.rows.Load(ctx, scope, runID)
	if err != nil {
		return RunDetail{}, fmt.Errorf("service: run %d detail: %w", runID, err)
	}
	return assembleRunDetail(rows), nil
}

// assembleRunDetail 是纯函数：每类数据单趟遍历，按 step_id / tool_call_id / ticket_id / order_id 建索引归组。
func assembleRunDetail(r repository.RunDetailRows) RunDetail {
	d := RunDetail{Run: r.Run, Card: r.Card, Timeline: nonNilSlice(r.Events), Audit: nonNilSlice(r.Audits),
		Evidence:  Evidence{KBLoaded: []model.KBLoadedPayload{}, Citations: []model.Citation{}},
		Execution: Execution{Dispatches: []repository.EventRow{}}}
	if r.Card != nil {
		d.Evidence.Citations = nonNilSlice(r.Card.Citations)
	}
	g := newStepGrouper(r.Steps)
	for _, tc := range r.ToolCalls {
		g.step(tc.StepID).ToolCalls = append(g.step(tc.StepID).ToolCalls, ToolCallDetail{ToolCallRow: tc})
	}
	toolIdx := g.toolIndex()
	llmRaw := llmRawIDs(r.Audits)
	linkRaw := len(llmRaw) == countType(r.Events, model.EventLLMCallEnd)
	llmN := 0
	for _, e := range r.Events {
		switch e.Type {
		case model.EventRunStarted:
			var q model.RunStartedPayload
			if d.decode(e, &q) {
				d.Question = &q
			}
			g.event(e)
		case model.EventToolCallEnd:
			if tc, ok := toolIdx[toolKey(e)]; ok {
				seq := e.Seq
				tc.EventSeq, tc.Event = &seq, e.Payload
				continue
			}
			g.event(e)
		case model.EventLLMCallEnd:
			var raw *uint64
			if linkRaw {
				raw = llmRaw[llmN]
			}
			llmN++
			var p model.LLMCallPayload
			if !d.decode(e, &p) {
				g.event(e)
				continue
			}
			call := LLMCallDetail{LLMCallPayload: p, EventSeq: e.Seq, RawObjectID: raw}
			st := g.step(e.StepID)
			st.LLMCalls = append(st.LLMCalls, call)
		case model.EventKBLoaded:
			var kb model.KBLoadedPayload
			if d.decode(e, &kb) {
				d.Evidence.KBLoaded = append(d.Evidence.KBLoaded, kb)
			}
			g.event(e)
		case model.EventDispatched:
			d.Execution.Dispatches = append(d.Execution.Dispatches, e)
			g.event(e)
		default:
			g.event(e)
		}
	}
	d.Steps = g.result()
	d.Approvals = groupTickets(r.Tickets, r.Decisions)
	d.Execution.Orders = groupOrders(r.Orders, r.Transitions)
	return d
}

// decode 解析事件 payload；失败记 warning 并返回 false。
func (d *RunDetail) decode(e repository.EventRow, dst any) bool {
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		d.Warnings = append(d.Warnings, fmt.Sprintf("event seq %d %s: bad payload: %v", e.Seq, e.Type, err))
		return false
	}
	return true
}

// llmRawIDs 按 audit_log.id 序取 LLM_CALL 行的 raw_object_id。run_event 没有 raw_object_id 列，
// 二者由 AppendTx 同事务按同一顺序写入，故条数相等时按序一一对应；条数不等（投影缺失）时不关联。
func llmRawIDs(audits []repository.AuditRow) []*uint64 {
	var out []*uint64
	for _, a := range audits {
		if a.Action == string(model.AuditLLMCall) {
			out = append(out, a.RawObjectID)
		}
	}
	return out
}

func countType(events []repository.EventRow, typ model.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// toolKey 是 TOOL_CALL_END 关联 tool_call 的键；tool_call_id 列为空时不关联（事件只进 step 的 EventSeqs）。
func toolKey(e repository.EventRow) uint64 {
	if e.ToolCallID == nil {
		return 0
	}
	return *e.ToolCallID
}

// stepGrouper 按 step_id 归组；run_step 的顺序在前，只在事件 / 工具调用里出现的 step 按首次出现追加。
// step_id 为空的事件归到 run 级伪 step（StepID 为空）。
type stepGrouper struct {
	order []string
	byID  map[string]*StepDetail
}

func newStepGrouper(steps []repository.StepRow) *stepGrouper {
	g := &stepGrouper{byID: make(map[string]*StepDetail, len(steps))}
	for _, s := range steps {
		g.step(s.StepID).StepRow = s
	}
	return g
}

func (g *stepGrouper) step(id string) *StepDetail {
	if s, ok := g.byID[id]; ok {
		return s
	}
	s := &StepDetail{StepRow: repository.StepRow{StepID: id, Node: id},
		ToolCalls: []ToolCallDetail{}, LLMCalls: []LLMCallDetail{}, EventSeqs: []int{}}
	g.byID[id] = s
	g.order = append(g.order, id)
	return s
}

func (g *stepGrouper) event(e repository.EventRow) {
	s := g.step(e.StepID)
	s.EventSeqs = append(s.EventSeqs, e.Seq)
}

// toolIndex 返回 tool_call.id → 明细指针（指向 step 内切片元素；建索引后不再追加工具调用）。
func (g *stepGrouper) toolIndex() map[uint64]*ToolCallDetail {
	idx := make(map[uint64]*ToolCallDetail)
	for _, id := range g.order {
		calls := g.byID[id].ToolCalls
		for i := range calls {
			idx[calls[i].ID] = &calls[i]
		}
	}
	return idx
}

func (g *stepGrouper) result() []StepDetail {
	out := make([]StepDetail, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, *g.byID[id])
	}
	return out
}

func groupTickets(tickets []model.ApprovalTicket, decisions []repository.DecisionDetailRow) []TicketDetail {
	byTicket := make(map[uint64][]repository.DecisionDetailRow, len(tickets))
	for _, d := range decisions {
		byTicket[d.TicketID] = append(byTicket[d.TicketID], d)
	}
	out := make([]TicketDetail, 0, len(tickets))
	for _, t := range tickets {
		out = append(out, TicketDetail{ApprovalTicket: t, Decisions: nonNilSlice(byTicket[t.TicketID])})
	}
	return out
}

func groupOrders(orders []repository.OrderRow, transitions []model.OrderTransition) []OrderDetail {
	byOrder := make(map[uint64][]TransitionView, len(orders))
	for _, t := range transitions {
		byOrder[t.Ref.OrderID] = append(byOrder[t.Ref.OrderID], TransitionView{From: t.From, To: t.To,
			ActorType: t.Actor.Type, ActorID: t.Actor.ID, ReasonCode: t.ReasonCode, CreatedAt: t.CreatedAt})
	}
	out := make([]OrderDetail, 0, len(orders))
	for _, o := range orders {
		out = append(out, OrderDetail{OrderRow: o, Transitions: nonNilSlice(byOrder[o.ID])})
	}
	return out
}

// nonNilSlice 让空表编码为 [] 而非 null。
func nonNilSlice[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
