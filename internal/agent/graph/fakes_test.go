package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// scriptedModel 在桩模型上按任务注入故障或改写回复。
type scriptedModel struct {
	fail     map[llm.Task]bool
	override map[llm.Task]string
}

var _ einomodel.BaseChatModel = scriptedModel{}

func taskOf(msgs []*schema.Message) llm.Task {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.User {
			var env struct {
				Task llm.Task `json:"task"`
			}
			if err := json.Unmarshal([]byte(msgs[i].Content), &env); err == nil {
				return env.Task
			}
		}
	}
	return ""
}

func (m scriptedModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	t := taskOf(msgs)
	if m.fail[t] {
		return nil, fmt.Errorf("scripted: %s down", t)
	}
	if r, ok := m.override[t]; ok {
		return schema.AssistantMessage(r, nil), nil
	}
	return llm.Stub{}.Generate(ctx, msgs, opts...)
}

func (m scriptedModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}

// memStore 是内存版 RunStateStore / AdviceStore / OrderDraftStore（幂等语义与仓储一致：按 run / advice 去重）。
type memStore struct {
	mu       sync.Mutex
	runs     uint64
	steps    map[uint64][]model.RunStep
	finished map[uint64]model.RunStatus
	reasons  map[uint64]string
	cards    map[uint64]model.AdviceCard // run_id → card
	adviceID map[uint64]uint64           // run_id → advice_id
	orders   map[uint64]model.WorkOrder  // advice_id → order
	saveErr  error
}

func newMemStore() *memStore {
	return &memStore{steps: map[uint64][]model.RunStep{}, finished: map[uint64]model.RunStatus{},
		reasons: map[uint64]string{}, cards: map[uint64]model.AdviceCard{}, adviceID: map[uint64]uint64{},
		orders: map[uint64]model.WorkOrder{}}
}

func (m *memStore) CreateRun(_ context.Context, _ model.RunState, _, _ uint64, _ agent.RunMeta) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs++
	return m.runs, nil
}

func (m *memStore) LoadRun(context.Context, model.Scope, uint64) (model.RunState, error) {
	return model.RunState{}, errors.New("not implemented")
}

func (m *memStore) SaveStep(_ context.Context, _ model.Scope, runID uint64, st model.RunStep) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps[runID] = append(m.steps[runID], st)
	return nil
}

func (m *memStore) FinishRun(_ context.Context, _ model.Scope, runID uint64, st model.RunStatus, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished[runID], m.reasons[runID] = st, reason
	return nil
}

func (m *memStore) Heartbeat(context.Context, model.Scope, uint64) error { return nil }

func (m *memStore) SaveAdvice(_ context.Context, _ model.Scope, c model.AdviceCard) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return 0, m.saveErr
	}
	for _, ct := range c.Citations {
		if ct.Type == model.CitationMeasured && (ct.ToolCallID == nil || *ct.ToolCallID == 0) {
			return 0, errors.New("measured citation without tool_call_id")
		}
	}
	if id, ok := m.adviceID[c.RunID]; ok {
		return id, nil
	}
	id := uint64(len(m.adviceID) + 1)
	m.adviceID[c.RunID], m.cards[c.RunID] = id, c
	return id, nil
}

func (m *memStore) CreateDraft(_ context.Context, _ model.Scope, o model.WorkOrder) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o.AdviceID == 0 || o.RunID == 0 || o.FieldID == 0 || o.TimeWindow.From.IsZero() || o.TimeWindow.To.Before(o.TimeWindow.From) {
		return 0, fmt.Errorf("invalid draft %+v", o)
	}
	if prev, ok := m.orders[o.AdviceID]; ok {
		return prev.OrderID, nil
	}
	o.OrderID = uint64(len(m.orders) + 1)
	m.orders[o.AdviceID] = o
	return o.OrderID, nil
}

func (m *memStore) orderCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.orders)
}

func (m *memStore) stepNodes(runID uint64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for _, s := range m.steps[runID] {
		fmt.Fprintf(&b, "%s:%s ", s.Node, s.Status)
	}
	return b.String()
}

// fakeLocker 发放租约；renewErr 非空时续期失败（模拟执行权丢失）。
type fakeLocker struct{ renewErr error }

type fakeLease struct{ renewErr error }

func (l fakeLocker) AcquireRun(context.Context, uint64) (agent.Lease, error) {
	return fakeLease(l), nil
}

func (l fakeLease) Renew(context.Context) error   { return l.renewErr }
func (l fakeLease) Release(context.Context) error { return nil }
