package graph_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/model"
)

// hitlRun 用作业类样本跑到审批中断，返回 agent 构造器（同一 store 上新建 agent 即模拟进程重启）与首个 Answer。
type hitlRun struct {
	h     *harness
	store *memStore
	hitl  *hitlStore
	tools *countingTools
	ev    *recordingEvents
	newA  func() *graph.Agent
}

func newHITLRun(t *testing.T, sampleID string) *hitlRun {
	t.Helper()
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sampleID)
	clk := sampleClock(t, s)
	r := &hitlRun{h: h, store: newMemStore(), hitl: newHITLStore(), tools: newCountingTools(fixtureTools(t, s, clk)),
		ev: &recordingEvents{}}
	r.newA = func() *graph.Agent {
		return h.newAgent(t, clk, deps{tools: r.tools, store: r.store, cfg: withHITL(r.hitl, r.ev)})
	}
	return r
}

func (r *hitlRun) ask(t *testing.T, sampleID string) agent.Answer {
	t.Helper()
	q, err := questionOf(findSample(t, loadSamples(t), sampleID))
	if err != nil {
		t.Fatal(err)
	}
	ans, err := r.newA().Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	return ans
}

// startedCount 统计某节点 STARTED 记录次数（节点每执行一次落一条 STARTED）。
func startedCount(m *memStore, runID uint64, node string) int {
	return strings.Count(m.stepNodes(runID), node+":"+string(model.StepStarted)+" ")
}

// 验收 1 / 2：作业单开票后在 await_approval 中断（检查点落库、run INTERRUPTED、票据激活为 OPEN、不写终态）；
// 审批通过后用新建的 agent（模拟进程重启，只有持久化的检查点）Resume：已完成节点零重复、工具零调用，finalize 收尾置 COMPLETED。
func TestAgent_InterruptThenResume(t *testing.T) {
	r := newHITLRun(t, sOperation)
	ans := r.ask(t, sOperation)

	if ans.Ticket == nil || ans.Ticket.Status != model.TicketOpen || ans.Order == nil || ans.Card == nil {
		t.Fatalf("answer ticket=%+v order=%v card=%v, want OPEN ticket with order and card", ans.Ticket, ans.Order, ans.Card)
	}
	if st, it := r.hitl.runState(ans.RunID); st != model.RunInterrupted || it != model.InterruptWaitApproval {
		t.Fatalf("run state = %s(%s), want INTERRUPTED(WAIT_APPROVAL)", st, it)
	}
	if _, ok := r.store.finished[ans.RunID]; ok {
		t.Fatalf("interrupted run must not be finished, got %s", r.store.finished[ans.RunID])
	}
	if _, sets := r.hitl.counts(); sets != 1 {
		t.Fatalf("checkpoint sets = %d, want 1", sets)
	}
	toolsBefore := r.tools.total()
	if toolsBefore == 0 {
		t.Fatal("operation sample made no tool calls")
	}

	// 未审批时不可续跑，run 保持 INTERRUPTED
	if _, err := r.newA().Resume(context.Background(), testScope, ans.RunID); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("resume before approval: err = %v, want ErrNotResumable", err)
	}
	if st, _ := r.hitl.runState(ans.RunID); st != model.RunInterrupted {
		t.Fatalf("run state after refused resume = %s, want INTERRUPTED", st)
	}

	r.hitl.approve(ans.RunID)
	res, err := r.newA().Resume(context.Background(), testScope, ans.RunID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Card == nil || res.Card.AdviceID != ans.Card.AdviceID || res.Order == nil || res.Order.OrderID != ans.Order.OrderID ||
		res.Ticket == nil || res.Ticket.TicketID != ans.Ticket.TicketID {
		t.Fatalf("resumed answer = %+v, want same card / order / ticket as %+v", res, ans)
	}
	if got := r.tools.total(); got != toolsBefore {
		t.Errorf("tool calls after resume = %d, want %d (no re-execution)", got, toolsBefore)
	}
	for _, n := range []string{graph.NodeClassify, graph.NodeEnrich, graph.NodePersist, graph.NodeToOrder} {
		if c := startedCount(r.store, ans.RunID, n); c != 1 {
			t.Errorf("node %s started %d times, want 1; steps=%s", n, c, r.store.stepNodes(ans.RunID))
		}
	}
	if c := startedCount(r.store, ans.RunID, graph.NodeFinalize); c != 1 {
		t.Errorf("finalize started %d times, want 1", c)
	}
	if r.store.finished[ans.RunID] != model.RunCompleted || r.store.orderCount() != 1 || len(r.store.cards) != 1 {
		t.Errorf("finished=%s orders=%d cards=%d, want COMPLETED/1/1", r.store.finished[ans.RunID],
			r.store.orderCount(), len(r.store.cards))
	}
	if ev := r.ev.list(); !slices.Contains(ev, model.EventRunResumed) || !slices.Contains(ev, model.EventRunCompleted) {
		t.Errorf("events = %v, want RUN_RESUMED and RUN_COMPLETED", ev)
	}
	if _, sets := r.hitl.counts(); sets != 1 {
		t.Errorf("checkpoint sets after resume = %d, want 1 (resume never re-interrupts)", sets)
	}

	// 已完成的 run 再次 Resume 被守卫拒绝，不重放收尾
	if _, err := r.newA().Resume(context.Background(), testScope, ans.RunID); !errors.Is(err, agent.ErrNotResumable) {
		t.Errorf("second resume: err = %v, want ErrNotResumable", err)
	}
}

// 非作业类建议不开票：await_approval 直通，不写检查点，run 直接 COMPLETED。
func TestAgent_NoTicketNoInterrupt(t *testing.T) {
	r := newHITLRun(t, sObservation)
	ans := r.ask(t, sObservation)
	if ans.Ticket != nil || ans.Order != nil {
		t.Fatalf("observation answer has ticket=%v order=%v", ans.Ticket, ans.Order)
	}
	if _, sets := r.hitl.counts(); sets != 0 {
		t.Errorf("checkpoint sets = %d, want 0", sets)
	}
	if r.store.finished[ans.RunID] != model.RunCompleted {
		t.Errorf("finished = %s, want COMPLETED", r.store.finished[ans.RunID])
	}
	if _, err := r.newA().Resume(context.Background(), testScope, ans.RunID); !errors.Is(err, agent.ErrNotResumable) {
		t.Errorf("resume without checkpoint: err = %v, want ErrNotResumable", err)
	}
}

// 激活失败（如 Activate 下游故障）：run 仍 INTERRUPTED、不写终态，Answer 带 PREPARING 票，由 ticket_activate 扫描补偿。
func TestAgent_ActivateFailureLeavesPreparing(t *testing.T) {
	r := newHITLRun(t, sOperation)
	r.hitl.activateErr = errors.New("activate down")
	ans := r.ask(t, sOperation)
	if ans.Ticket == nil || ans.Ticket.Status != model.TicketPreparing {
		t.Fatalf("ticket = %+v, want PREPARING", ans.Ticket)
	}
	if st, _ := r.hitl.runState(ans.RunID); st != model.RunInterrupted {
		t.Errorf("run state = %s, want INTERRUPTED", st)
	}
	if _, ok := r.store.finished[ans.RunID]; ok {
		t.Errorf("run finished despite interrupt")
	}
}

// 检查点落库失败：不得开放审批（先持久化再展示），run 以 FAILED 收尾、票据停在 PREPARING（由扫描作废）。
func TestAgent_CheckpointFailureFailsRun(t *testing.T) {
	r := newHITLRun(t, sOperation)
	r.hitl.setErr = errors.New("db down")
	q, err := questionOf(findSample(t, loadSamples(t), sOperation))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.newA().Ask(context.Background(), q); err == nil {
		t.Fatal("ask succeeded although checkpoint could not be saved")
	}
	if r.store.finished[1] != model.RunFailed {
		t.Errorf("finished = %s, want FAILED", r.store.finished[1])
	}
	if tk := r.hitl.tickets[1]; tk.Status != model.TicketPreparing {
		t.Errorf("ticket status = %s, want PREPARING (never opened)", tk.Status)
	}
}

// 只配 Gate、不配 Checkpoints：退回 L5 行为（开票但不中断，run 直接 COMPLETED），Resume 不可用。
func TestAgent_GateWithoutCheckpointsDoesNotInterrupt(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	clk := sampleClock(t, s)
	store, hs := newMemStore(), newHITLStore()
	a := h.newAgent(t, clk, deps{tools: fixtureTools(t, s, clk), store: store,
		cfg: func(c *graph.Config) { c.Gate = hs }})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if ans.Ticket == nil || store.finished[ans.RunID] != model.RunCompleted {
		t.Errorf("ticket=%v finished=%s, want ticket and COMPLETED", ans.Ticket, store.finished[ans.RunID])
	}
	if _, err := a.Resume(context.Background(), testScope, ans.RunID); !errors.Is(err, graph.ErrResumeUnsupported) {
		t.Errorf("resume err = %v, want ErrResumeUnsupported", err)
	}
}
