package graph_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/model"
)

// hitlStore 是 L6 的内存 fake：CheckpointStore（Set 同时置 run INTERRUPTED，与 repository.CheckpointRepo 的同事务语义一致）、
// RunResumeStore（MarkResumed 校验中断类型 / APPROVED 票 / graph_version 后 epoch+1）与 ApprovalGate（Open 幂等、Activate 要求 run 已中断）。
// 并行期没有真正的 ApprovalService（L5），票据状态由测试经 approve 驱动。
type hitlStore struct {
	mu          sync.Mutex
	checkpoints map[string][][]byte
	sets        int
	gets        int
	status      map[uint64]model.RunStatus
	interrupt   map[uint64]model.InterruptType
	epoch       map[uint64]int
	tickets     map[uint64]model.ApprovalTicket // run_id → ticket
	activateErr error
	setErr      error
}

var (
	_ agent.CheckpointStore = (*hitlStore)(nil)
	_ agent.RunResumeStore  = (*hitlStore)(nil)
	_ agent.ApprovalGate    = (*hitlStore)(nil)
)

func newHITLStore() *hitlStore {
	return &hitlStore{checkpoints: map[string][][]byte{}, status: map[uint64]model.RunStatus{},
		interrupt: map[uint64]model.InterruptType{}, epoch: map[uint64]int{}, tickets: map[uint64]model.ApprovalTicket{}}
}

func (h *hitlStore) Get(ctx context.Context, id string) ([]byte, bool, error) {
	if _, ok := agent.CheckpointTargetFrom(ctx); !ok { // 与 CheckpointRepo 一致：按 ctx 中的归属限定租户 / 农场
		return nil, false, errors.New("no checkpoint target")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gets++
	cps := h.checkpoints[id]
	if len(cps) == 0 {
		return nil, false, nil
	}
	return append([]byte(nil), cps[len(cps)-1]...), true, nil
}

func (h *hitlStore) Set(ctx context.Context, id string, payload []byte) error {
	t, ok := agent.CheckpointTargetFrom(ctx)
	if !ok {
		return errors.New("no checkpoint target")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.setErr != nil {
		return h.setErr
	}
	if h.status[t.RunID] != "" && h.status[t.RunID] != model.RunRunning {
		return fmt.Errorf("run %d is %s: %w", t.RunID, h.status[t.RunID], model.ErrConcurrentUpdate)
	}
	h.sets++
	cps := append(h.checkpoints[id], append([]byte(nil), payload...)) // 拷贝：模拟落库，不与 Eino 共享内存
	if len(cps) > 3 {
		cps = cps[len(cps)-3:]
	}
	h.checkpoints[id] = cps
	h.status[t.RunID], h.interrupt[t.RunID] = model.RunInterrupted, t.Interrupt
	return nil
}

func (h *hitlStore) MarkResumed(_ context.Context, _ model.Scope, req agent.ResumeRequest) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.status[req.RunID] != model.RunInterrupted || req.GraphVersion != graph.GraphVersion {
		return 0, agent.ErrNotResumable
	}
	switch h.interrupt[req.RunID] {
	case model.InterruptWaitApproval:
		if h.tickets[req.RunID].Status != model.TicketApproved {
			return 0, agent.ErrNotResumable
		}
	case model.InterruptSystemError:
	default:
		return 0, agent.ErrNotResumable
	}
	h.epoch[req.RunID]++
	h.status[req.RunID], h.interrupt[req.RunID] = model.RunRunning, ""
	return h.epoch[req.RunID], nil
}

func (h *hitlStore) Open(_ context.Context, scope model.Scope, req agent.ApprovalRequest) (model.ApprovalTicket, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.tickets[req.Order.RunID]; ok {
		return t, nil
	}
	t := model.ApprovalTicket{TicketID: 100 + req.Order.OrderID, RunID: req.Order.RunID, TenantID: scope.TenantID,
		FarmID: scope.FarmID, OrderID: req.Order.OrderID, TicketKind: model.TicketKindOrder, TicketSeq: 1,
		RiskLevel: model.RiskHigh, InitiatorID: req.InitiatorID, Status: model.TicketPreparing}
	h.tickets[req.Order.RunID] = t
	return t, nil
}

func (h *hitlStore) Activate(_ context.Context, _ model.Scope, runID uint64) (model.ApprovalTicket, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activateErr != nil {
		return model.ApprovalTicket{}, h.activateErr
	}
	t, ok := h.tickets[runID]
	if !ok || h.status[runID] != model.RunInterrupted || h.interrupt[runID] != model.InterruptWaitApproval {
		return model.ApprovalTicket{}, agent.ErrNotActivatable
	}
	if t.Status == model.TicketPreparing {
		t.Status = model.TicketOpen
		h.tickets[runID] = t
	}
	return t, nil
}

// approve 模拟 DecisionService 生效：票据 OPEN → APPROVED。
func (h *hitlStore) approve(runID uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.tickets[runID]
	t.Status = model.TicketApproved
	h.tickets[runID] = t
}

func (h *hitlStore) runState(runID uint64) (model.RunStatus, model.InterruptType) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status[runID], h.interrupt[runID]
}

func (h *hitlStore) counts() (gets, sets int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets, h.sets
}

// countingTools 记录每个工具的调用次数与 epoch（验收 1：已完成步骤零重复执行）。
type countingTools struct {
	inner  agent.ToolProvider
	mu     sync.Mutex
	calls  map[string]int
	epochs []int
}

func newCountingTools(inner agent.ToolProvider) *countingTools {
	return &countingTools{inner: inner, calls: map[string]int{}}
}

func (c *countingTools) Call(ctx context.Context, scope model.Scope, call agent.ToolCall) (agent.ToolResult, error) {
	c.mu.Lock()
	c.calls[call.Tool]++
	c.epochs = append(c.epochs, call.Epoch)
	c.mu.Unlock()
	return c.inner.Call(ctx, scope, call)
}

func (c *countingTools) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *countingTools) callsOf(tool string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[tool]
}

func (c *countingTools) lastEpoch() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.epochs) == 0 {
		return -1
	}
	return c.epochs[len(c.epochs)-1]
}

// list 返回已记录事件的类型（recordingEvents 定义在 guard_test.go）。
func (r *recordingEvents) list() []model.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.EventType, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// countType 统计某类事件条数。
func countType(types []model.EventType, want model.EventType) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

// withHITL 注入 L6 依赖。
func withHITL(st *hitlStore, ev agent.EventSink) func(*graph.Config) {
	return func(c *graph.Config) {
		c.Gate, c.Checkpoints, c.Resumes, c.Events = st, st, st, ev
	}
}
