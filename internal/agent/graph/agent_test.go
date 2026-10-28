package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// 表驱动用例所用样本（均来自回归集）。
const (
	sOperation   = "rice-pest-0015"     // 吡蚜酮背负式喷雾：作业类 → 工单
	sObservation = "rice-pest-0019"     // 施药防护：观察类 → 无工单
	sTimeout     = "rice-disaster-0015" // weather.alert 超时
	sEmpty       = "rice-irr-0011"      // field.profile 空返回
	sMalformed   = "rice-irr-0016"      // field.history 非法返回
	sKBMiss      = "rice-pest-0024"     // 稻粒黑粉病：知识库未覆盖
	sNearHarvest = "rice-pest-0016"     // 近收获：把收获日提前后 PHI 约束清空候选
)

type askCase struct {
	name       string
	sample     string
	mutate     func(*sample.Sample) *sample.Sample
	model      scriptedModel
	wantOut    model.Outcome
	wantReason string
	wantOrder  bool
	check      func(t *testing.T, ans agent.Answer)
}

func TestAgentAsk_Table(t *testing.T) {
	h := newHarness(t)
	ss := loadSamples(t)
	cases := []askCase{
		{name: "operation creates draft order", sample: sOperation, wantOut: model.OutcomeAdvice, wantOrder: true,
			check: checkOrder},
		{name: "observation has no order", sample: sObservation, wantOut: model.OutcomeAdvice,
			check: func(t *testing.T, ans agent.Answer) { wantNext(t, ans, model.NextObserve) }},
		{name: "tool timeout", sample: sTimeout, wantOut: model.OutcomeRefused, wantReason: graph.RefuseDataMissing},
		{name: "tool empty", sample: sEmpty, wantOut: model.OutcomeRefused, wantReason: graph.RefuseDataMissing},
		{name: "tool malformed", sample: sMalformed, wantOut: model.OutcomeRefused, wantReason: graph.RefuseDataMissing},
		{name: "kb miss", sample: sKBMiss, wantOut: model.OutcomeRefused, wantReason: graph.RefuseKBMiss},
		{name: "empty candidate set", sample: sNearHarvest, mutate: withHarvest("2026-09-27"),
			wantOut: model.OutcomeRefused, wantReason: graph.RefuseKBMiss},
		{name: "product outside candidates rejected", sample: sOperation,
			model:   scriptedModel{override: map[llm.Task]string{llm.TaskChoose: `{"product_id":"bifenthrin-25ec"}`}},
			wantOut: model.OutcomeRefused, wantReason: graph.RefuseValidation},
		{name: "bare number in model text rejected", sample: sOperation,
			model: scriptedModel{override: map[llm.Task]string{
				llm.TaskExplain: `{"conclusion":"每亩用吡蚜酮10克","reasons":[]}`}},
			wantOut: model.OutcomeRefused, wantReason: graph.RefuseValidation},
		{name: "llm down at explain degrades", sample: sOperation,
			model:   scriptedModel{fail: map[llm.Task]bool{llm.TaskExplain: true}},
			wantOut: model.OutcomeAdvice, check: checkDegraded},
		{name: "llm down at choose degrades", sample: sOperation,
			model:   scriptedModel{fail: map[llm.Task]bool{llm.TaskChoose: true}},
			wantOut: model.OutcomeAdvice, check: checkDegraded},
		{name: "llm down at classify refuses", sample: sOperation,
			model:   scriptedModel{fail: map[llm.Task]bool{llm.TaskClassify: true}},
			wantOut: model.OutcomeRefused, wantReason: graph.RefuseLLMUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := findSample(t, ss, tc.sample)
			if tc.mutate != nil {
				s = tc.mutate(s)
			}
			store := newMemStore()
			clk := sampleClock(t, s)
			a := h.newAgent(t, clk, deps{model: tc.model, tools: fixtureTools(t, s, clk), store: store})
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatalf("ask: %v (steps %s)", err, store.stepNodes(ans.RunID))
			}
			if ans.Outcome != tc.wantOut || ans.RefuseReason != tc.wantReason {
				t.Fatalf("got %s/%q, want %s/%q; conclusion=%q", ans.Outcome, ans.RefuseReason,
					tc.wantOut, tc.wantReason, ans.Card.Conclusion)
			}
			if got := store.finished[ans.RunID]; got != model.RunCompleted {
				t.Errorf("run status %q, want COMPLETED", got)
			}
			if _, ok := store.cards[ans.RunID]; !ok || ans.Card.AdviceID == 0 {
				t.Errorf("card not persisted")
			}
			if (ans.Order != nil) != tc.wantOrder || (store.orderCount() == 1) != tc.wantOrder {
				t.Errorf("order=%v count=%d, want order %v", ans.Order, store.orderCount(), tc.wantOrder)
			}
			if tc.check != nil {
				tc.check(t, ans)
			}
		})
	}
}

// checkOrder：作业单为 DRAFT + DRY_RUN，payload 带药剂与亩用量，关联建议卡与 run。
func checkOrder(t *testing.T, ans agent.Answer) {
	t.Helper()
	o := ans.Order
	if o.Status != model.OrderDraft || o.DispatchMode != model.DispatchDryRun || o.WorkType != model.WorkSpray {
		t.Errorf("order %s/%s/%s", o.Status, o.DispatchMode, o.WorkType)
	}
	if o.AdviceID != ans.Card.AdviceID || o.RunID != ans.RunID || o.DeviceType == "" {
		t.Errorf("order links advice=%d run=%d device=%q", o.AdviceID, o.RunID, o.DeviceType)
	}
	var p struct {
		Product   string `json:"product"`
		DosePerMu *struct {
			Value float64 `json:"value"`
		} `json:"dose_per_mu"`
		FieldID uint64 `json:"field_id"`
	}
	if err := json.Unmarshal(o.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Product == "" || p.DosePerMu == nil || p.FieldID != ans.Card.FieldID {
		t.Errorf("payload %s", o.Payload)
	}
	wantNext(t, ans, model.NextCreateOrder)
}

func wantNext(t *testing.T, ans agent.Answer, want model.NextAction) {
	t.Helper()
	if ans.Card.NextAction != want {
		t.Errorf("next_action %q, want %q", ans.Card.NextAction, want)
	}
}

// checkDegraded：降级为观察类保守建议，不选药、无工单。
func checkDegraded(t *testing.T, ans agent.Answer) {
	t.Helper()
	c := ans.Card
	if !c.Degraded || c.AdviceType != model.AdviceTypeObservation || c.SelectedProduct != "" || len(c.Citations) == 0 {
		t.Errorf("degraded=%v type=%q selected=%q cites=%d", c.Degraded, c.AdviceType, c.SelectedProduct, len(c.Citations))
	}
	wantNext(t, ans, model.NextObserve)
}

// withHarvest 返回把 crop.phenology 的 expected_harvest 改为 date 的样本副本（不改原样本）。
func withHarvest(date string) func(*sample.Sample) *sample.Sample {
	return func(s *sample.Sample) *sample.Sample {
		c := *s
		c.Fixtures.MCP = make(map[string][]sample.ToolFixture, len(s.Fixtures.MCP))
		for tool, fx := range s.Fixtures.MCP {
			c.Fixtures.MCP[tool] = append([]sample.ToolFixture(nil), fx...)
		}
		for i, f := range c.Fixtures.MCP[toolPhenology] {
			if r, ok := f.Return.(map[string]any); ok {
				nr := make(map[string]any, len(r))
				for k, v := range r {
					nr[k] = v
				}
				nr["expected_harvest"] = date
				c.Fixtures.MCP[toolPhenology][i].Return = nr
			}
		}
		return &c
	}
}

const toolPhenology = "crop.phenology"

// blockingTools 在工具调用里等待 ctx 取消（模拟慢工具期间租约续期失败）。
type blockingTools struct{}

func (blockingTools) Call(ctx context.Context, _ model.Scope, _ agent.ToolCall) (agent.ToolResult, error) {
	<-ctx.Done()
	return agent.ToolResult{}, context.Cause(ctx)
}

// 续期失败 → 执行权丢失：Ask 返回 ErrLeaseLost，不落建议卡、不建工单、不写终态。
func TestAgentAsk_LeaseLostStopsWrites(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	store := newMemStore()
	a := h.newAgent(t, sampleClock(t, s), deps{tools: blockingTools{}, store: store,
		locker: fakeLocker{renewErr: errors.New("lease stolen")}, renew: time.Millisecond})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if !errors.Is(err, runner.ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if len(store.cards) != 0 || store.orderCount() != 0 {
		t.Errorf("writes after lease lost: cards=%d orders=%d", len(store.cards), store.orderCount())
	}
	if _, ok := store.finished[ans.RunID]; ok {
		t.Errorf("run finished after lease lost")
	}
}

// 同一 run 重跑：建议卡与工单按 run / 建议卡幂等，不重复创建。
func TestAgentAsk_RerunIdempotent(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	store := newMemStore()
	clk := sampleClock(t, s)
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	var first agent.Answer
	for i := range 2 {
		a := h.newAgent(t, clk, deps{tools: fixtureTools(t, s, clk), store: store})
		ans, err := a.Ask(context.Background(), q)
		if err != nil {
			t.Fatalf("ask #%d: %v", i, err)
		}
		if i == 0 {
			first, q.RunID = ans, ans.RunID
			continue
		}
		if ans.RunID != first.RunID || ans.Card.AdviceID != first.Card.AdviceID || ans.Order.OrderID != first.Order.OrderID {
			t.Errorf("rerun run=%d advice=%d order=%d, first run=%d advice=%d order=%d", ans.RunID,
				ans.Card.AdviceID, ans.Order.OrderID, first.RunID, first.Card.AdviceID, first.Order.OrderID)
		}
	}
	if store.runs != 1 || len(store.cards) != 1 || store.orderCount() != 1 {
		t.Errorf("runs=%d cards=%d orders=%d, want 1/1/1", store.runs, len(store.cards), store.orderCount())
	}
}
