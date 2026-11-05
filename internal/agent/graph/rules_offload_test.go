package graph_test

import (
	"context"
	"encoding/json"
	"testing"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// conflictModel 让 explain 在结论里建议禁忌混用（同一作用机理的己唑醇与咪鲜胺），应被 tank_mix 拒收。
var conflictModel = scriptedModel{override: map[llm.Task]string{
	llm.TaskExplain: `{"conclusion":"达到防治条件，建议在作业窗口内施药，可将己唑醇与咪鲜胺桶混一起喷","reasons":[]}`}}

// TestAgentAsk_TankMixConflictRejected（P2 L4 验收 1，红线）：模型结论与混用禁忌冲突 → REFUSED / VALIDATION_FAILED，
// VALIDATION_FAILED 事件 check=tank_mix，不出作业单。未装配开关读取（按 off）时同样拒收。
func TestAgentAsk_TankMixConflictRejected(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	store, ev := newMemStore(), &recordingEvents{}
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: conflictModel, tools: fixtureTools(t, s, clk), store: store, events: ev})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Outcome != model.OutcomeRefused || ans.Card.RefuseReason != "VALIDATION_FAILED" || store.orderCount() != 0 {
		t.Fatalf("outcome %s/%q orders %d", ans.Outcome, ans.Card.RefuseReason, store.orderCount())
	}
	if got := validationChecks(t, ev); len(got) != 1 || got[0] != "tank_mix" {
		t.Fatalf("VALIDATION_FAILED checks = %v, want [tank_mix]", got)
	}
}

// TestAgentAsk_WindowConflictRejected（P2 L4 验收 1，红线）：模型把作业安排到规则层作业窗口之外 → REFUSED /
// VALIDATION_FAILED（check=operation_window），不出作业单。窗口打开时安排到窗口外的日子、窗口关闭（预报降雨）时仍让当天作业，都拒收。
func TestAgentAsk_WindowConflictRejected(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	tests := []struct {
		name   string
		sample *sample.Sample
		text   string
	}{
		{name: "open window, next week", sample: s, text: "达到防治条件，下周再统一喷药"},
		{name: "closed by rain, act now", sample: withToolValue(s, toolForecast, "rain_mm_24h", 12),
			text: "虽然有雨，今天抓紧施药"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := scriptedModel{override: map[llm.Task]string{
				llm.TaskExplain: `{"conclusion":"` + tt.text + `","reasons":[]}`}}
			store, ev := newMemStore(), &recordingEvents{}
			clk := sampleClock(t, tt.sample)
			a := h.newAgent(t, clk, deps{model: m, tools: fixtureTools(t, tt.sample, clk), store: store, events: ev})
			q, err := questionOf(tt.sample)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if ans.Outcome != model.OutcomeRefused || ans.Card.RefuseReason != "VALIDATION_FAILED" || store.orderCount() != 0 {
				t.Fatalf("outcome %s/%q orders %d", ans.Outcome, ans.Card.RefuseReason, store.orderCount())
			}
			if got := validationChecks(t, ev); len(got) != 1 || got[0] != "operation_window" {
				t.Fatalf("VALIDATION_FAILED checks = %v, want [operation_window]", got)
			}
		})
	}
}

// TestAgentAsk_PriorityConflictRejected（P2 L4 验收 1，红线）：虫情调查同时给出稻飞虱与稻纵卷叶螟（后者超标倍数更高）时，
// 模型主张优先防治稻飞虱 → REFUSED / VALIDATION_FAILED（check=pest_priority）；只调查到一个对象时规则不适用、放行。
func TestAgentAsk_PriorityConflictRejected(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation) // 稻飞虱 1300 头/百丛（指标 1000）
	two := withToolValue(s, toolPestSurvey, "larvae_per_100_hills", 150.0)
	m := scriptedModel{override: map[llm.Task]string{
		llm.TaskExplain: `{"conclusion":"达到防治条件，优先防治稻飞虱","reasons":[]}`}}
	tests := []struct {
		name   string
		sample *sample.Sample
		reject bool
	}{
		{name: "two pests, model order differs", sample: two, reject: true},
		{name: "one pest, rule not applicable", sample: s},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, ev := newMemStore(), &recordingEvents{}
			clk := sampleClock(t, tt.sample)
			a := h.newAgent(t, clk, deps{model: m, tools: fixtureTools(t, tt.sample, clk), store: store, events: ev})
			q, err := questionOf(tt.sample)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.reject {
				if ans.Outcome != model.OutcomeAdvice || len(validationChecks(t, ev)) != 0 {
					t.Fatalf("outcome %s/%q, want ADVICE", ans.Outcome, ans.Card.RefuseReason)
				}
				return
			}
			if ans.Outcome != model.OutcomeRefused || ans.Card.RefuseReason != "VALIDATION_FAILED" || store.orderCount() != 0 {
				t.Fatalf("outcome %s/%q orders %d", ans.Outcome, ans.Card.RefuseReason, store.orderCount())
			}
			if got := validationChecks(t, ev); len(got) != 1 || got[0] != "pest_priority" {
				t.Fatalf("VALIDATION_FAILED checks = %v, want [pest_priority]", got)
			}
		})
	}
}

// toolPestSurvey 是虫情调查工具。
const toolPestSurvey = "field.pest_survey"

// withToolValue 复制样本，把工具 tool 各条固定返回里的 key 改为 v（不改原样本）。
func withToolValue(s *sample.Sample, tool, key string, v any) *sample.Sample {
	c := *s
	c.Fixtures.MCP = make(map[string][]sample.ToolFixture, len(s.Fixtures.MCP))
	for name, fx := range s.Fixtures.MCP {
		c.Fixtures.MCP[name] = append([]sample.ToolFixture(nil), fx...)
	}
	for i, f := range c.Fixtures.MCP[tool] {
		if r, ok := f.Return.(map[string]any); ok {
			nr := make(map[string]any, len(r)+1)
			for k, val := range r {
				nr[k] = val
			}
			nr[key] = v
			c.Fixtures.MCP[tool][i].Return = nr
		}
	}
	return &c
}

// validationChecks 取 VALIDATION_FAILED 事件 payload 的 check。
func validationChecks(t *testing.T, ev *recordingEvents) []string {
	t.Helper()
	ev.mu.Lock()
	defer ev.mu.Unlock()
	var out []string
	for _, e := range ev.events {
		if e.Type != model.EventValidationFailed {
			continue
		}
		var p struct {
			Check string `json:"check"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Check)
	}
	return out
}
