package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/grader/code"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// l6FaultPrefix 是 P1 L6 故障样本的 id 前缀。
const l6FaultPrefix = "rice-l6fault-"

// askWithEvents 用样本的模型故障包装与 FixtureMCP 跑一次主链路，返回 degraded_matrix 所需的 Trial（含 DEGRADED 时间线）。
func (h *harness) askWithEvents(t *testing.T, s *sample.Sample) *grader.Trial {
	t.Helper()
	chat, err := env.NewFaultModel(llm.Stub{}, s.Fixtures.Model)
	if err != nil {
		t.Fatal(err)
	}
	ev := &recordingEvents{}
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: chat, tools: fixtureTools(t, s, clk), store: newMemStore(), events: ev})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: ask: %v", s.ID, err)
	}
	events := make([]grader.RunEvent, 0, len(ev.events))
	for _, e := range ev.events {
		events = append(events, grader.RunEvent{Type: e.Type, Actor: string(e.Actor), StepID: e.StepID, Payload: e.Payload})
	}
	return &grader.Trial{Sample: s, Outcome: ans.Outcome, RefuseReason: ans.RefuseReason, Card: ans.Card, Events: events}
}

// degradedFailEvents 只让 DEGRADED 写失败，其余事件照常记录（观测类事件写失败本就只告警，这里隔离 DEGRADED）。
type degradedFailEvents struct {
	recordingEvents
	err error
}

func (d *degradedFailEvents) Emit(ctx context.Context, e model.RunEvent) (int64, error) {
	if e.Type == model.EventDegraded {
		return 0, d.err
	}
	return d.recordingEvents.Emit(ctx, e)
}

// DEGRADED 写失败让 run 失败（审查修复 L6-8）：规则层降级 / 缓存回落 / 三类故障拒答各取一个样本，DEGRADED 落库失败时
// Ask 返回该错误，且不发 ADVICE_READY（降级卡 / 故障拒答卡未在缺降级留痕时落库）；应失败：只告警、照常出卡。
func TestFaultSamples_DegradedEmitFailureFailsRun(t *testing.T) {
	h := newHarness(t)
	ss := loadSamples(t)
	boom := errors.New("event store down")
	for _, id := range []string{"0001", "0002", "0003", "0004", "0006"} {
		t.Run(id, func(t *testing.T) {
			s := findSample(t, ss, l6FaultPrefix+id)
			chat, err := env.NewFaultModel(llm.Stub{}, s.Fixtures.Model)
			if err != nil {
				t.Fatal(err)
			}
			ev := &degradedFailEvents{err: boom}
			clk := sampleClock(t, s)
			a := h.newAgent(t, clk, deps{model: chat, tools: fixtureTools(t, s, clk), store: newMemStore(), events: ev})
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Ask(context.Background(), q); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want %v", err, boom)
			}
			for _, e := range ev.events {
				if e.Type == model.EventAdviceReady {
					t.Fatalf("ADVICE_READY emitted although DEGRADED was not recorded")
				}
			}
		})
	}
}

// 验收 §6.7 第 2-4 条（主链路侧）：每个 rice-l6fault 样本（设备离线样本需 trial 库，只在连库评测判定）经桩模型 + FixtureMCP
// 运行后，DEGRADED 事件、文案 key 与结局都与故障矩阵一致（degraded_matrix 通过）。
func TestFaultSamples_DegradedMatrix(t *testing.T) {
	h := newHarness(t)
	g := code.NewDegradedMatrix(h.faults)
	n := 0
	for _, s := range loadSamples(t) {
		if !strings.HasPrefix(s.ID, l6FaultPrefix) || s.Fixtures.Dispatch != nil {
			continue
		}
		n++
		t.Run(s.ID, func(t *testing.T) {
			if len(s.Expect.Degraded) == 0 {
				t.Fatal("fault sample must declare expect.degraded")
			}
			sc, err := g.Grade(context.Background(), h.askWithEvents(t, s))
			if err != nil || !sc.Pass {
				t.Fatalf("degraded_matrix pass=%t evidence=%q err=%v", sc.Pass, sc.Evidence, err)
			}
		})
	}
	if n < 6 {
		t.Fatalf("want >= 6 graph-level fault samples, got %d", n)
	}
}

// 应失败：同一次运行按错误的期望打分必须不通过——缓存超限样本若期望 CACHE_FALLBACK、
// 规则层降级样本若期望拒答、无故障样本若期望降级，degraded_matrix 都要判失败。
func TestFaultSamples_DegradedMatrixRejectsWrongExpectation(t *testing.T) {
	h := newHarness(t)
	g := code.NewDegradedMatrix(h.faults)
	ss := loadSamples(t)
	tests := []struct {
		name string
		id   string
		want []sample.DegradedExpect
	}{
		{name: "stale cache is not a cache fallback", id: l6FaultPrefix + "0005",
			want: []sample.DegradedExpect{{Fault: string(model.FaultMCPTimeout), Action: string(model.DegradeCacheFallback)}}},
		{name: "rule fallback is not a refusal", id: l6FaultPrefix + "0001",
			want: []sample.DegradedExpect{{Fault: string(model.FaultLLMUnavailable), Action: string(model.DegradeRefuse)}}},
		{name: "healthy run has no DEGRADED", id: sOperation,
			want: []sample.DegradedExpect{{Fault: string(model.FaultMCPTimeout), Action: string(model.DegradeCacheFallback)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := *findSample(t, ss, tt.id)
			cp.Expect.Degraded = tt.want
			sc, err := g.Grade(context.Background(), h.askWithEvents(t, &cp))
			if err != nil {
				t.Fatal(err)
			}
			if sc.Pass || sc.Evidence == "" {
				t.Fatalf("pass=%t evidence=%q, want failure", sc.Pass, sc.Evidence)
			}
		})
	}
}

// 规则层降级剥离药剂相关数值（审查修复 L6-2）：用药量样本 rice-pest-0004（选药后 compute 补算 phi_days / 亩用量 /
// 亩施药液量）在 explain 不可用时降级，卡片不得带出这些数值、numbers[*] 引用与只由它们引入的 dosage-dilution 引用，
// degraded_matrix 通过（应失败：degrade 不清 s.numbers，选药撤销后用量照旧上卡）。
func TestFaultSamples_RuleFallbackDropsProductNumbers(t *testing.T) {
	h := newHarness(t)
	cp := *findSample(t, loadSamples(t), "rice-pest-0004")
	cp.Fixtures.Model = &sample.ModelFixture{Unavailable: []string{"explain"}}
	cp.Expect.Degraded = []sample.DegradedExpect{{Fault: string(model.FaultLLMUnavailable), Action: string(model.DegradeRuleFallback)}}
	tr := h.askWithEvents(t, &cp)
	sc, err := code.NewDegradedMatrix(h.faults).Grade(context.Background(), tr)
	if err != nil || !sc.Pass {
		t.Fatalf("degraded_matrix pass=%t evidence=%q err=%v", sc.Pass, sc.Evidence, err)
	}
	if len(tr.Card.Numbers) != 0 {
		t.Errorf("numbers = %+v, want none on degraded card", tr.Card.Numbers)
	}
	for _, c := range tr.Card.Citations {
		if strings.HasPrefix(c.ClaimPath, "numbers[") || c.KBID == calc.KBDosageDilution {
			t.Errorf("citation %s %s survives degrade", c.ClaimPath, c.KBID)
		}
	}
}

// 缓存回落事件带网关按原 collected_at 算的新鲜度：恰 1h 前的天气缓存 staleness_sec = 3600，tool 为 weather.forecast。
func TestFaultSamples_CacheFallbackStaleness(t *testing.T) {
	h := newHarness(t)
	tr := h.askWithEvents(t, findSample(t, loadSamples(t), l6FaultPrefix+"0004"))
	var got []model.DegradedPayload
	for _, e := range tr.Events {
		if e.Type != model.EventDegraded {
			continue
		}
		var p model.DegradedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	const wantStale = 3600
	if len(got) != 1 || got[0].StalenessSec == nil || *got[0].StalenessSec != wantStale || got[0].Tool != "weather.forecast" {
		t.Fatalf("DEGRADED = %+v, want one CACHE_FALLBACK staleness %d on weather.forecast", got, wantStale)
	}
	if tr.Card == nil || !tr.Card.Degraded {
		t.Fatalf("card = %+v, want degraded", tr.Card)
	}
}

// 非故障拒答不写 DEGRADED：空数据 / 非法返回的 DATA_MISSING 与校验失败不属于故障矩阵（应失败：误把它们记成 MCP_TIMEOUT）。
// explain 回复非 JSON（llm.ErrBadReply）是回复不合规而非模型不可用：拒答 VALIDATION_FAILED，不走规则层降级、
// 不写 DEGRADED（审查修复 L6-6；应失败：把 ErrBadReply 当 LLM_UNAVAILABLE 降级出观察类建议）。
func TestFaultSamples_NonFaultRefusalsNotDegraded(t *testing.T) {
	h := newHarness(t)
	ss := loadSamples(t)
	nonJSONExplain := scriptedModel{override: map[llm.Task]string{llm.TaskExplain: "好的，建议尽快防治。"}}
	tests := []struct {
		name   string
		id     string
		model  scriptedModel
		reason string
	}{
		{name: "empty tool data", id: sEmpty},
		{name: "malformed tool data", id: sMalformed},
		{name: "validation failed", id: sOperation, model: bareNumberModel, reason: graph.RefuseValidation},
		{name: "explain reply not JSON", id: sOperation, model: nonJSONExplain, reason: graph.RefuseValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := findSample(t, ss, tt.id)
			ev := &recordingEvents{}
			clk := sampleClock(t, s)
			a := h.newAgent(t, clk, deps{model: tt.model, tools: fixtureTools(t, s, clk), store: newMemStore(), events: ev})
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if ans.Outcome != model.OutcomeRefused {
				t.Fatalf("outcome = %s, want REFUSED", ans.Outcome)
			}
			if tt.reason != "" && ans.RefuseReason != tt.reason {
				t.Fatalf("refuse_reason = %s, want %s", ans.RefuseReason, tt.reason)
			}
			for _, e := range ev.events {
				if e.Type == model.EventDegraded {
					t.Fatalf("unexpected DEGRADED %s", e.Payload)
				}
			}
			if ans.Card != nil && ans.Card.Degraded {
				t.Fatal("card degraded on non-fault refusal")
			}
		})
	}
}
