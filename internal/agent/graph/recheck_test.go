package graph_test

import (
	"context"
	"errors"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const (
	sForecastOp  = "rice-pest-0001" // 稻飞虱达标、天气适宜：作业类，依赖 weather.forecast（时效工具）
	toolForecast = "weather.forecast"
)

// withForecasts 把样本的 weather.forecast 改为按调用次序返回（第 1 次原值，之后依次为 later，字段覆盖原值）。
func withForecasts(later ...map[string]any) func(*sample.Sample) *sample.Sample {
	return func(s *sample.Sample) *sample.Sample {
		c := *s
		c.Fixtures.MCP = make(map[string][]sample.ToolFixture, len(s.Fixtures.MCP))
		for tool, fx := range s.Fixtures.MCP {
			c.Fixtures.MCP[tool] = append([]sample.ToolFixture(nil), fx...)
		}
		fx := &c.Fixtures.MCP[toolForecast][0]
		orig, _ := fx.Return.(map[string]any)
		returns := []any{orig}
		for _, over := range later {
			r := make(map[string]any, len(orig))
			for k, v := range orig {
				r[k] = v
			}
			for k, v := range over {
				r[k] = v
			}
			returns = append(returns, r)
		}
		fx.Return, fx.Returns = nil, returns
		return &c
	}
}

// 验收 3：审批后改天气，下发前复检以新 epoch 重取时效工具；条件恶化即结论变化，模型不可用则无结论。
func TestAgentRecheck(t *testing.T) {
	h := newHarness(t)
	ss := loadSamples(t)
	const epoch = 2
	tests := []struct {
		name        string
		later       map[string]any
		model       scriptedModel // 复检时的模型
		wantChanged bool
		wantErr     error
	}{
		{name: "weather unchanged", later: map[string]any{}},
		{name: "rain forecast worsens", later: map[string]any{"rain_mm_24h": 12}, wantChanged: true},
		{name: "wind exceeds knapsack limit", later: map[string]any{"wind_speed_ms": 8.0}, wantChanged: true},
		{name: "model down is inconclusive", later: map[string]any{},
			model: scriptedModel{fail: map[llm.Task]bool{llm.TaskClassify: true}}, wantErr: agent.ErrRecheckInconclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := withForecasts(tt.later)(findSample(t, ss, sForecastOp))
			clk := sampleClock(t, s)
			tools := newCountingTools(fixtureTools(t, s, clk))
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := h.newAgent(t, clk, deps{tools: tools, store: newMemStore()}).Ask(context.Background(), q)
			if err != nil || ans.Card == nil || ans.Card.AdviceType != model.AdviceTypeOperation || ans.Card.SelectedProduct == "" {
				t.Fatalf("ask: %v card=%+v", err, ans.Card)
			}
			before := tools.callsOf(toolForecast)

			q.RunID = ans.RunID
			re := h.newAgent(t, clk, deps{model: tt.model, tools: tools, store: newMemStore()})
			got, err := re.Recheck(context.Background(), agent.RecheckRequest{Question: q, Card: *ans.Card, Epoch: epoch})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.Changed != tt.wantChanged {
				t.Fatalf("changed = %v (%s), want %v", got.Changed, got.Detail, tt.wantChanged)
			}
			if tools.callsOf(toolForecast) != before+1 {
				t.Errorf("forecast calls %d -> %d, want refetch once", before, tools.callsOf(toolForecast))
			}
			if last := tools.lastEpoch(); last != epoch {
				t.Errorf("recheck tool epoch = %d, want %d", last, epoch)
			}
		})
	}
}

// rice-hitl-0002 自带按次序的天气 fixture：首次建议为作业类，复检取第 2 条（24h 降雨 12mm）须判定结论变化。
func TestAgentRecheck_HITLSample(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), "rice-hitl-0002")
	clk := sampleClock(t, s)
	tools := fixtureTools(t, s, clk)
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := h.newAgent(t, clk, deps{tools: tools, store: newMemStore()}).Ask(context.Background(), q)
	if err != nil || ans.Card == nil || ans.Card.AdviceType != model.AdviceTypeOperation {
		t.Fatalf("ask: %v card=%+v", err, ans.Card)
	}
	q.RunID = ans.RunID
	got, err := h.newAgent(t, clk, deps{tools: tools, store: newMemStore()}).
		Recheck(context.Background(), agent.RecheckRequest{Question: q, Card: *ans.Card, Epoch: 2})
	if err != nil || !got.Changed {
		t.Fatalf("recheck = %+v, %v; want changed", got, err)
	}
}

func TestAgentRecheck_BadRequest(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sForecastOp)
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{tools: fixtureTools(t, s, clk), store: newMemStore()})
	for _, req := range []agent.RecheckRequest{{Epoch: 1}, {Question: agent.Question{RunID: 7}}} {
		if _, err := a.Recheck(context.Background(), req); err == nil {
			t.Errorf("recheck %+v: want error", req)
		}
	}
}
