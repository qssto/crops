package checkpoint

// Eino 检查点 spike（P0 L0 任务 7）：确认 CheckPointStore 只在中断时写入，
// 以及用同一 checkpoint_id resume 时已完成节点不重复执行。结论写入 docs/dev-log.md。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// countingStore 是记录调用次数与顺序的内存 CheckPointStore。
type countingStore struct {
	mu   sync.Mutex
	data map[string][]byte
	gets int
	sets int
	log  []string // 形如 "get:run-1:miss" / "set:run-1"
}

func newCountingStore() *countingStore {
	return &countingStore{data: make(map[string][]byte)}
}

func (s *countingStore) Get(_ context.Context, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	b, ok := s.data[id]
	hit := "miss"
	if ok {
		hit = "hit"
	}
	s.log = append(s.log, fmt.Sprintf("get:%s:%s", id, hit))
	return b, ok, nil
}

func (s *countingStore) Set(_ context.Context, id string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	s.data[id] = b
	s.log = append(s.log, fmt.Sprintf("set:%s:%dB", id, len(b)))
	return nil
}

func (s *countingStore) counts() (gets, sets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.sets
}

// nodeCounter 记录每个节点的执行次数，并把节点执行穿插进 store 日志，便于观察 Get / Set 时机。
type nodeCounter struct {
	mu    sync.Mutex
	runs  map[string]int
	store *countingStore
}

func (c *nodeCounter) hit(node string) {
	c.mu.Lock()
	c.runs[node]++
	c.mu.Unlock()
	c.store.mu.Lock()
	c.store.log = append(c.store.log, "node:"+node)
	c.store.mu.Unlock()
}

func (c *nodeCounter) get(node string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[node]
}

// 模拟主链路的四个节点：分类 → 生成作业单草稿 → 审批闸门 → 下发（副作用）。
const (
	nodeClassify = "classify"
	nodeDraft    = "draft_order"
	nodeGate     = "approval_gate"
	nodeDispatch = "dispatch"
)

var pipeline = []string{nodeClassify, nodeDraft, nodeGate, nodeDispatch}

// gateFunc 决定审批闸门节点的行为；nil 表示直接放行。
type gateFunc func(ctx context.Context, in string) (string, error)

// buildGraph 构造线性 Graph：START → classify → draft_order → approval_gate → dispatch → END。
func buildGraph(t *testing.T, c *nodeCounter, gate gateFunc, opts ...compose.GraphCompileOption) compose.Runnable[string, string] {
	t.Helper()
	g := compose.NewGraph[string, string]()
	for _, node := range pipeline {
		name := node
		fn := func(ctx context.Context, in string) (string, error) {
			c.hit(name)
			if name == nodeGate && gate != nil {
				return gate(ctx, in)
			}
			return in + ">" + name, nil
		}
		if err := g.AddLambdaNode(name, compose.InvokableLambda(fn)); err != nil {
			t.Fatalf("add node %s: %v", name, err)
		}
	}
	edges := append(append([]string{compose.START}, pipeline...), compose.END)
	for i := 0; i+1 < len(edges); i++ {
		if err := g.AddEdge(edges[i], edges[i+1]); err != nil {
			t.Fatalf("add edge %s->%s: %v", edges[i], edges[i+1], err)
		}
	}
	r, err := g.Compile(context.Background(), opts...)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return r
}

func assertRuns(t *testing.T, c *nodeCounter, want map[string]int) {
	t.Helper()
	for _, node := range pipeline {
		if got := c.get(node); got != want[node] {
			t.Errorf("node %s ran %d times, want %d", node, got, want[node])
		}
	}
}

func assertStore(t *testing.T, s *countingStore, wantGets, wantSets int) {
	t.Helper()
	gets, sets := s.counts()
	if gets != wantGets || sets != wantSets {
		t.Errorf("store gets=%d sets=%d, want gets=%d sets=%d; log=%s",
			gets, sets, wantGets, wantSets, strings.Join(s.log, " "))
	}
}

func newFixture() (*countingStore, *nodeCounter) {
	s := newCountingStore()
	return s, &nodeCounter{runs: make(map[string]int), store: s}
}

const runID = "run-1"

// 无中断：带 checkpoint_id 运行只在开头 Get 一次（miss），全程 Set 0 次；不带 id 则 Get / Set 都为 0。
func TestEinoSpike_NoInterrupt_NeverSets(t *testing.T) {
	tests := []struct {
		name     string
		withID   bool
		wantGets int
	}{
		{name: "with checkpoint id", withID: true, wantGets: 1},
		{name: "without checkpoint id", withID: false, wantGets: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, c := newFixture()
			r := buildGraph(t, c, nil, compose.WithCheckPointStore(store))
			var opts []compose.Option
			if tt.withID {
				opts = append(opts, compose.WithCheckPointID(runID))
			}
			out, err := r.Invoke(context.Background(), "q", opts...)
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if want := "q>classify>draft_order>approval_gate>dispatch"; out != want {
				t.Fatalf("out = %q, want %q", out, want)
			}
			assertStore(t, store, tt.wantGets, 0)
			assertRuns(t, c, map[string]int{nodeClassify: 1, nodeDraft: 1, nodeGate: 1, nodeDispatch: 1})
			t.Logf("store log: %s", strings.Join(store.log, " "))
		})
	}
}

// WithInterruptBeforeNodes：中断时 Set 1 次；同 id resume 时先 Get（hit）再只跑 dispatch，
// 完成后不再 Set，也不删除检查点。再用同一 id 调用会从旧检查点再跑一次 dispatch（需业务层防重）。
func TestEinoSpike_InterruptBeforeNode_ResumeSkipsCompleted(t *testing.T) {
	store, c := newFixture()
	r := buildGraph(t, c, nil,
		compose.WithCheckPointStore(store),
		compose.WithInterruptBeforeNodes([]string{nodeDispatch}))
	ctx := context.Background()

	_, err := r.Invoke(ctx, "q", compose.WithCheckPointID(runID))
	info, ok := compose.ExtractInterruptInfo(err)
	if !ok {
		t.Fatalf("want interrupt error, got %v", err)
	}
	if len(info.BeforeNodes) != 1 || info.BeforeNodes[0] != nodeDispatch {
		t.Fatalf("BeforeNodes = %v, want [%s]", info.BeforeNodes, nodeDispatch)
	}
	assertStore(t, store, 1, 1)
	assertRuns(t, c, map[string]int{nodeClassify: 1, nodeDraft: 1, nodeGate: 1, nodeDispatch: 0})

	// resume：输入会被忽略，节点输入取自检查点
	out, err := r.Invoke(ctx, "ignored", compose.WithCheckPointID(runID))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if want := "q>classify>draft_order>approval_gate>dispatch"; out != want {
		t.Fatalf("resume out = %q, want %q", out, want)
	}
	assertStore(t, store, 2, 1)
	assertRuns(t, c, map[string]int{nodeClassify: 1, nodeDraft: 1, nodeGate: 1, nodeDispatch: 1})

	// 完成后检查点仍在：同 id 再调用会再次执行 dispatch
	if _, err := r.Invoke(ctx, "again", compose.WithCheckPointID(runID)); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	assertStore(t, store, 3, 1)
	assertRuns(t, c, map[string]int{nodeClassify: 1, nodeDraft: 1, nodeGate: 1, nodeDispatch: 2})
	t.Logf("store log: %s", strings.Join(store.log, " "))
}

// gateState 是审批闸门中断时自存的状态；自定义类型须先注册才能进检查点序列化。
type gateState struct {
	Ticket string
	Input  string
}

func init() { schema.RegisterName[gateState]("spike_gate_state") }

// 节点内 StatefulInterrupt：中断时 Set 1 次；resume 时只有中断节点自身重跑，之前已完成节点不重跑。
// 观察到的坑：普通调用（未用 WithGraphInterrupt）时，中断节点的输入不进检查点，resume 重跑拿到的是零值，
// 所以节点要把需要的输入放进 StatefulInterrupt 的 state 自己带回。
func TestEinoSpike_NodeInterrupt_ResumeWithData(t *testing.T) {
	store, c := newFixture()
	var resumedInput *string
	gate := func(ctx context.Context, in string) (string, error) {
		wasInterrupted, hasState, st := compose.GetInterruptState[gateState](ctx)
		if !wasInterrupted {
			return "", compose.StatefulInterrupt(ctx, "WAIT_APPROVAL", gateState{Ticket: "ticket-1", Input: in})
		}
		resumedInput = &in
		isTarget, hasData, decision := compose.GetResumeContext[string](ctx)
		if !isTarget || !hasData || !hasState {
			return "", errors.New("resumed without approval decision")
		}
		return st.Input + ">" + st.Ticket + ":" + decision, nil
	}
	r := buildGraph(t, c, gate, compose.WithCheckPointStore(store))
	ctx := context.Background()

	_, err := r.Invoke(ctx, "q", compose.WithCheckPointID(runID))
	info, ok := compose.ExtractInterruptInfo(err)
	if !ok {
		t.Fatalf("want interrupt error, got %v", err)
	}
	var interruptID string
	for _, ic := range info.InterruptContexts {
		if ic.IsRootCause {
			interruptID = ic.ID
		}
	}
	if interruptID == "" {
		t.Fatalf("no root-cause interrupt context in %+v", info.InterruptContexts)
	}
	assertStore(t, store, 1, 1)
	assertRuns(t, c, map[string]int{nodeClassify: 1, nodeDraft: 1, nodeGate: 1, nodeDispatch: 0})
	t.Logf("interrupt id: %s, rerun nodes: %v", interruptID, info.RerunNodes)

	resumeCtx := compose.ResumeWithData(ctx, interruptID, "approve")
	out, err := r.Invoke(resumeCtx, "ignored", compose.WithCheckPointID(runID))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if want := "q>classify>draft_order>ticket-1:approve>dispatch"; out != want {
		t.Fatalf("resume out = %q, want %q", out, want)
	}
	if resumedInput == nil || *resumedInput != "" {
		t.Errorf("rerun node input = %v, want zero value (input not persisted)", resumedInput)
	}
	assertStore(t, store, 2, 1)
	assertRuns(t, c, map[string]int{nodeClassify: 1, nodeDraft: 1, nodeGate: 2, nodeDispatch: 1})
	t.Logf("store log: %s", strings.Join(store.log, " "))
}
