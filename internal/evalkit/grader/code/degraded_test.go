package code

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const (
	testCachedAt = "2026-07-18T06:30:00+08:00"
	testTool     = "weather.forecast"
)

func loadTestFaults(t *testing.T) *policy.FaultMatrix {
	t.Helper()
	m, err := policy.LoadFaultMatrix(repoRoot+"/policy/fault_matrix.yaml", repoRoot+"/configs/texts/zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func degradedEvent(t *testing.T, actor model.ActorType, p model.DegradedPayload) grader.RunEvent {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return grader.RunEvent{Type: model.EventDegraded, Actor: string(actor), Payload: b}
}

// faultTrial 构造一条符合矩阵的观测：expect.degraded 只含 (fault, action) 一条，事件 / 卡片 / outbox 均为理想值。
func faultTrial(t *testing.T, m *policy.FaultMatrix, code model.FaultCode, action model.DegradeAction) *grader.Trial {
	t.Helper()
	f, ok := m.Fault(code)
	if !ok {
		t.Fatalf("fault %s not in matrix", code)
	}
	at, err := time.Parse(time.RFC3339, testCachedAt)
	if err != nil {
		t.Fatal(err)
	}
	s := &sample.Sample{ID: "tmp-fault", Expect: sample.Expect{
		Degraded: []sample.DegradedExpect{{Fault: string(code), Action: string(action)}}}}
	p := model.DegradedPayload{Fault: code, Action: action, TextKey: f.TextKey}
	tr := &grader.Trial{Sample: s}
	switch action {
	case model.DegradeRuleFallback:
		p.TextKey = f.DegradedTextKey
		tr.Outcome = model.OutcomeAdvice
		tr.Card = &model.AdviceCard{Outcome: model.OutcomeAdvice, Degraded: true, AdviceType: model.AdviceTypeObservation}
	case model.DegradeCacheFallback:
		stale := 3600
		p.StalenessSec, p.Tool = &stale, testTool
		s.Fixtures.MCP = map[string][]sample.ToolFixture{testTool: {{Error: sample.FaultTimeout,
			Cache: map[string]any{"collected_at": testCachedAt}}}}
		tr.Outcome = model.OutcomeAdvice
		tr.Card = &model.AdviceCard{Outcome: model.OutcomeAdvice, Degraded: true, AdviceType: model.AdviceTypeOperation,
			Citations: []model.Citation{{Type: model.CitationMeasured, ClaimPath: "reasons[0]",
				DataSource: model.ToolSourceCache, CollectedAt: &at}}}
	case model.DegradeRefuse:
		if code == model.FaultMCPTimeout {
			p.Tool = testTool
		}
		tr.Outcome, tr.RefuseReason = model.OutcomeRefused, f.RefuseReason
		tr.Card = &model.AdviceCard{Outcome: model.OutcomeRefused, RefuseReason: f.RefuseReason, Conclusion: f.Text}
	case model.DegradeHoldNotify:
		tr.Outcome = model.OutcomeAdvice
		tr.DeviceTargets = []grader.DeviceTarget{}
		tr.Outbox = []grader.OutboxRecord{
			{Topic: "order.dispatch", DedupKey: "dispatch:3", Status: outboxDone},
			{Topic: topicNotifySend, DedupKey: model.NotifyDedupKey(model.NotifyDeviceOffline, 3, 7), Status: outboxDone}}
	}
	tr.Events = []grader.RunEvent{degradedEvent(t, model.ActorSystem, p)}
	return tr
}

// degraded_matrix（红线 §6.7）：理想观测通过；事件缺失 / 多出、actor 或文案 key 错、结局与矩阵不一致都应失败。
func TestDegradedMatrix_Grade(t *testing.T) {
	m := loadTestFaults(t)
	g := NewDegradedMatrix(m)
	rule := func() *grader.Trial { return faultTrial(t, m, model.FaultLLMUnavailable, model.DegradeRuleFallback) }
	cache := func() *grader.Trial { return faultTrial(t, m, model.FaultMCPTimeout, model.DegradeCacheFallback) }
	mcpRefuse := func() *grader.Trial { return faultTrial(t, m, model.FaultMCPTimeout, model.DegradeRefuse) }
	hold := func() *grader.Trial { return faultTrial(t, m, model.FaultDeviceOffline, model.DegradeHoldNotify) }
	withEvent := func(tr *grader.Trial, p model.DegradedPayload) *grader.Trial {
		tr.Events = []grader.RunEvent{degradedEvent(t, model.ActorSystem, p)}
		return tr
	}
	llmDown, _ := m.Fault(model.FaultLLMUnavailable)
	mcpDown, _ := m.Fault(model.FaultMCPTimeout)
	tests := []struct {
		name  string
		trial func() *grader.Trial
		pass  bool
	}{
		{name: "rule fallback", trial: rule, pass: true},
		{name: "cache fallback keeps original collected_at", trial: cache, pass: true},
		{name: "mcp refuse with tool", trial: mcpRefuse, pass: true},
		{name: "kb miss refuse", pass: true,
			trial: func() *grader.Trial { return faultTrial(t, m, model.FaultKBMiss, model.DegradeRefuse) }},
		{name: "llm refuse", pass: true,
			trial: func() *grader.Trial { return faultTrial(t, m, model.FaultLLMUnavailable, model.DegradeRefuse) }},
		{name: "hold and notify", trial: hold, pass: true},
		{name: "no expectation passes without events", pass: true,
			trial: func() *grader.Trial { return &grader.Trial{Sample: &sample.Sample{ID: "tmp"}} }},
		{name: "missing DEGRADED event", trial: func() *grader.Trial { tr := rule(); tr.Events = []grader.RunEvent{}; return tr }},
		{name: "duplicate DEGRADED event", trial: func() *grader.Trial {
			tr := rule()
			tr.Events = append(tr.Events, tr.Events[0])
			return tr
		}},
		{name: "actor agent", trial: func() *grader.Trial {
			tr := rule()
			tr.Events[0].Actor = string(model.ActorAgent)
			return tr
		}},
		{name: "rule fallback with refused text key", trial: func() *grader.Trial {
			return withEvent(rule(), model.DegradedPayload{Fault: model.FaultLLMUnavailable,
				Action: model.DegradeRuleFallback, TextKey: llmDown.TextKey})
		}},
		{name: "rule fallback card not degraded", trial: func() *grader.Trial { tr := rule(); tr.Card.Degraded = false; return tr }},
		{name: "rule fallback picks product", trial: func() *grader.Trial {
			tr := rule()
			tr.Card.SelectedProduct = "pymetrozine-50wg"
			return tr
		}},
		{name: "rule fallback operation advice", trial: func() *grader.Trial {
			tr := rule()
			tr.Card.AdviceType = model.AdviceTypeOperation
			return tr
		}},
		{name: "rule fallback keeps rule-layer numbers", pass: true, trial: func() *grader.Trial {
			tr := rule()
			tr.Card.Category = model.CategoryProtect
			tr.Card.Numbers = []model.Number{{Field: "max_hours_per_day", Value: 8, Unit: "小时"}}
			return tr
		}},
		{name: "fertilize rule fallback keeps water_per_mu", pass: true, trial: func() *grader.Trial {
			tr := rule()
			tr.Card.Category = model.CategoryFertilize
			tr.Card.Numbers = []model.Number{{Field: "water_per_mu", Value: 30, Unit: "L"}}
			return tr
		}},
		{name: "rule fallback leaks phi_days", trial: func() *grader.Trial {
			tr := rule()
			tr.Card.Numbers = []model.Number{{Field: "phi_days", Value: 14, Unit: "天"}}
			return tr
		}},
		{name: "rule fallback leaks dose", trial: func() *grader.Trial {
			tr := rule()
			tr.Card.Numbers = []model.Number{{Field: "dose_per_mu_max", Value: 20, Unit: "g"}}
			return tr
		}},
		{name: "protect rule fallback leaks water_per_mu", trial: func() *grader.Trial {
			tr := rule()
			tr.Card.Category = model.CategoryProtect
			tr.Card.Numbers = []model.Number{{Field: "water_per_mu", Value: 30, Unit: "L"}}
			return tr
		}},
		{name: "rule fallback keeps candidates", trial: func() *grader.Trial {
			tr := rule()
			tr.Card.ProductCandidates = []string{"pymetrozine-50wg"}
			return tr
		}},
		{name: "cache fallback without cache citation", trial: func() *grader.Trial {
			tr := cache()
			tr.Card.Citations = nil
			return tr
		}},
		{name: "cache fallback citing only live data", trial: func() *grader.Trial {
			tr := cache()
			tr.Card.Citations[0].DataSource = "mock-station"
			return tr
		}},
		{name: "cache citation rewritten to fallback time", trial: func() *grader.Trial {
			tr := cache()
			now := time.Date(2026, 7, 18, 7, 30, 0, 0, time.UTC)
			tr.Card.Citations[0].CollectedAt = &now
			return tr
		}},
		{name: "cache fallback without staleness", trial: func() *grader.Trial {
			return withEvent(cache(), model.DegradedPayload{Fault: model.FaultMCPTimeout,
				Action: model.DegradeCacheFallback, TextKey: mcpDown.TextKey, Tool: testTool})
		}},
		{name: "cache fallback card not degraded", trial: func() *grader.Trial { tr := cache(); tr.Card.Degraded = false; return tr }},
		{name: "mcp refuse without tool", trial: func() *grader.Trial {
			return withEvent(mcpRefuse(), model.DegradedPayload{Fault: model.FaultMCPTimeout,
				Action: model.DegradeRefuse, TextKey: mcpDown.TextKey})
		}},
		{name: "refuse with wrong reason", trial: func() *grader.Trial { tr := mcpRefuse(); tr.RefuseReason = "KB_MISS"; return tr }},
		{name: "refuse not refused", trial: func() *grader.Trial { tr := mcpRefuse(); tr.Outcome = model.OutcomeAdvice; return tr }},
		{name: "refuse conclusion not matrix text", trial: func() *grader.Trial {
			tr := mcpRefuse()
			tr.Card.Conclusion = "数据缺失"
			return tr
		}},
		{name: "device offline still dispatched", trial: func() *grader.Trial {
			tr := hold()
			tr.DeviceTargets = []grader.DeviceTarget{{Method: deviceMethodDryRun, OrderID: 3}}
			return tr
		}},
		{name: "device offline notified twice", trial: func() *grader.Trial {
			tr := hold()
			tr.Outbox = append(tr.Outbox, grader.OutboxRecord{Topic: topicNotifySend,
				DedupKey: model.NotifyDedupKey(model.NotifyDeviceOffline, 3, 8), Status: outboxDone})
			return tr
		}},
		{name: "device offline not notified", trial: func() *grader.Trial { tr := hold(); tr.Outbox = tr.Outbox[:1]; return tr }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, err := g.Grade(context.Background(), tt.trial())
			if err != nil {
				t.Fatal(err)
			}
			if sc.Pass != tt.pass || (!tt.pass && sc.Evidence == "") {
				t.Fatalf("pass=%t evidence=%q, want pass=%t", sc.Pass, sc.Evidence, tt.pass)
			}
		})
	}
}

// 缺观测记 grader_error（不静默通过）：无矩阵、无事件时间线、设备离线缺设备调用或 outbox。
func TestDegradedMatrix_GraderError(t *testing.T) {
	m := loadTestFaults(t)
	tests := []struct {
		name  string
		g     DegradedMatrix
		trial func() *grader.Trial
		want  error
	}{
		{name: "no fault matrix", g: NewDegradedMatrix(nil), want: errNoFaultMatrix,
			trial: func() *grader.Trial { return faultTrial(t, m, model.FaultKBMiss, model.DegradeRefuse) }},
		{name: "no events", g: NewDegradedMatrix(m), want: errNoEvents, trial: func() *grader.Trial {
			tr := faultTrial(t, m, model.FaultKBMiss, model.DegradeRefuse)
			tr.Events = nil
			return tr
		}},
		{name: "no device calls", g: NewDegradedMatrix(m), want: errNoDevice, trial: func() *grader.Trial {
			tr := faultTrial(t, m, model.FaultDeviceOffline, model.DegradeHoldNotify)
			tr.DeviceTargets = nil
			return tr
		}},
		{name: "no outbox", g: NewDegradedMatrix(m), want: errNoOutbox, trial: func() *grader.Trial {
			tr := faultTrial(t, m, model.FaultDeviceOffline, model.DegradeHoldNotify)
			tr.Outbox = nil
			return tr
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.g.Grade(context.Background(), tt.trial()); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// fillReference 补出的理想观测必须通过 degraded_matrix（validate 的参考解检查依赖它）。
func TestDegradedMatrix_FillReferencePasses(t *testing.T) {
	m := loadTestFaults(t)
	g := NewDegradedMatrix(m)
	for _, d := range []sample.DegradedExpect{
		{Fault: string(model.FaultLLMUnavailable), Action: string(model.DegradeRuleFallback)},
		{Fault: string(model.FaultLLMUnavailable), Action: string(model.DegradeRefuse)},
		{Fault: string(model.FaultMCPTimeout), Action: string(model.DegradeRefuse)},
		{Fault: string(model.FaultKBMiss), Action: string(model.DegradeRefuse)},
		{Fault: string(model.FaultDeviceOffline), Action: string(model.DegradeHoldNotify)},
	} {
		t.Run(d.Fault+"/"+d.Action, func(t *testing.T) {
			tr := faultTrial(t, m, model.FaultCode(d.Fault), model.DegradeAction(d.Action))
			tr.Events, tr.Outbox, tr.DeviceTargets = nil, nil, nil
			g.fillReference(tr)
			if sc, err := g.Grade(context.Background(), tr); err != nil || !sc.Pass {
				t.Fatalf("pass=%t evidence=%q err=%v", sc.Pass, sc.Evidence, err)
			}
		})
	}
}

// 仓库故障样本（rice-l6fault-*，均断言 expect.degraded）的参考解在 degraded_matrix 下通过（validate 的 L4 参考解规则）；
// 去掉 fillReference 的事件后应报 grader_error（参考解违例），证明该样本确实被 degraded_matrix 判定。
func TestReferenceCheck_RepoFaultSamples(t *testing.T) {
	tx, err := knowledge.LoadTaxonomy(taxonomyFile)
	if err != nil {
		t.Fatal(err)
	}
	ss, _, err := sample.LoadDir(repoRoot + "/eval/suites")
	if err != nil {
		t.Fatal(err)
	}
	var faults []*sample.Sample
	for _, s := range ss {
		if len(s.Expect.Degraded) > 0 {
			faults = append(faults, s)
		}
	}
	const minFaultSamples = 7
	if len(faults) < minFaultSamples {
		t.Fatalf("want >= %d samples with expect.degraded, got %d", minFaultSamples, len(faults))
	}
	kb := knowledge.NewStore(knowledge.NewRepoReader(repoRoot))
	ref := NewReferenceCheck(tx, kb, []grader.Grader{NewDegradedMatrix(loadTestFaults(t))})
	if v := ref.Check(faults); len(v) != 0 {
		t.Fatalf("reference violations: %v", v)
	}
	if v := NewReferenceCheck(tx, kb, []grader.Grader{NewDegradedMatrix(nil)}).Check(faults); len(v) != len(faults) {
		t.Fatalf("without fault matrix want %d violations, got %d: %v", len(faults), len(v), v)
	}
}
