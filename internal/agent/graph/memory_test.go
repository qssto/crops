package graph_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// fakeMemory 是可控的 agent.MemoryStore。
type fakeMemory struct {
	items []agent.MemoryItem
	err   error
}

func (f fakeMemory) Recent(context.Context, model.Scope, uint64, int) ([]agent.MemoryItem, error) {
	return f.items, f.err
}

// recordingModel 在 scriptedModel 之上记录 explain 任务的用户消息。
type recordingModel struct {
	scriptedModel
	mu      sync.Mutex
	explain []string
}

func (m *recordingModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	if taskOf(msgs) == llm.TaskExplain {
		m.mu.Lock()
		m.explain = append(m.explain, msgs[len(msgs)-1].Content)
		m.mu.Unlock()
	}
	return m.scriptedModel.Generate(ctx, msgs, opts...)
}

func (m *recordingModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}

func (m *recordingModel) explainPrompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.explain, "\n")
}

// 验收 4：记忆开关开 / 关 / 读失败时主链路都正常出建议；开启时去数字的摘要进入 explain 入参。
func TestAgentAsk_Memory(t *testing.T) {
	h := newHarness(t)
	ss := loadSamples(t)
	items := []agent.MemoryItem{
		{Kind: "REJECTED", Summary: "作业单驳回：雨前不宜喷药"},
		{Kind: "ADVICE", Summary: "上次每亩用药20克"},
		{Kind: "ADVICE", Summary: "123"}, // 去数字后为空 → 丢弃
	}
	tests := []struct {
		name string
		mem  agent.MemoryStore
		want []string
	}{
		{name: "memory off", mem: nil},
		{name: "memory on", mem: fakeMemory{items: items}, want: []string{"历史记录：作业单驳回：雨前不宜喷药", "历史记录：上次每亩用药"}},
		{name: "memory read error ignored", mem: fakeMemory{err: errors.New("db down")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := findSample(t, ss, sOperation)
			store := newMemStore()
			clk := sampleClock(t, s)
			rec := &recordingModel{}
			a := h.newAgent(t, clk, deps{model: rec, tools: fixtureTools(t, s, clk), store: store,
				cfg: func(c *graph.Config) { c.Memory = tt.mem }})
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			if ans.Outcome != model.OutcomeAdvice || ans.Order == nil {
				t.Fatalf("got %s/%q order=%v, want advice with order", ans.Outcome, ans.RefuseReason, ans.Order != nil)
			}
			prompt := rec.explainPrompt()
			if prompt == "" {
				t.Fatal("explain not called")
			}
			for _, w := range tt.want {
				if !strings.Contains(prompt, w) {
					t.Errorf("explain prompt missing %q", w)
				}
			}
			if strings.Contains(prompt, "20") {
				t.Errorf("memory digits leaked into explain prompt")
			}
			if n := strings.Count(prompt, "历史记录："); n != len(tt.want) {
				t.Errorf("memory hints = %d, want %d", n, len(tt.want))
			}
		})
	}
}
