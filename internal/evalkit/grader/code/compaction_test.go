package code

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// compactionSource 是业务表重新读出的会话原料：两轮（第一轮含「旁边是蜂场」）、一张建议卡、一张以药剂不对驳回的单、
// 一条时效实测（摘要带 MySQL JSON 列的空白与键序）与 kb 引用。
func compactionSource() model.SessionSource {
	at := time.Date(2026, 7, 18, 7, 0, 0, 0, time.UTC)
	return model.SessionSource{RunID: 30, FieldID: 7,
		Turns: []model.SessionTurn{{RunID: 10, Question: "旁边是蜂场，打药要注意什么？"}, {RunID: 20, Question: "稻飞虱多了怎么办？"}},
		Cards: []model.SessionCard{{AdviceID: 3, RunID: 20, Crop: "rice", PhenologyStage: "分蘖期", Outcome: model.OutcomeAdvice,
			Conclusion: "建议施药", SelectedProduct: "吡蚜酮"}},
		Orders: []model.SessionOrderSource{{SessionOrder: model.SessionOrder{OrderID: 5, Status: model.OrderRejected, TicketID: 9,
			TicketStatus: model.TicketRejected, RejectReasonCode: string(model.RejectProduct), RejectText: "换药"}, RunID: 20, Product: "吡蚜酮"}},
		Measured: []model.MeasuredFact{{ToolCallID: 11, ToolName: "field.pest_survey",
			ResultSummary: json.RawMessage(`{"pest": "稻飞虱", "density": 1200}`), CollectedAt: at}},
		CitedKBIDs: []string{"kb-rice-001", "kb-rice-001"},
	}
}

// compactionPrompt 按生产形状拼 user 消息：{"task", "input": {..., "session": 会话块}}，facts 由 mut 改动。
func compactionPrompt(t *testing.T, task string, src model.SessionSource, mut func(*model.CompactionFacts)) string {
	t.Helper()
	lex, err := compaction.NewLexicon(1, "蜂场")
	if err != nil {
		t.Fatal(err)
	}
	f := compaction.BuildFacts(src, lex)
	if len(f.Measured) > 0 {
		f.Measured[0].ResultSummary = json.RawMessage(`{"density":1200,"pest":"稻飞虱"}`) // 重新编码后空白、键序不同，取值相同
	}
	if mut != nil {
		mut(&f)
	}
	body, err := json.Marshal(map[string]any{"task": task, "input": map[string]any{"q": "现在要打药吗",
		"session": compaction.Block{CompactionVersion: model.CompactionVersion, Facts: f}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCompactionRetained(t *testing.T) {
	multi := &sample.Sample{ID: "m", Fixtures: sample.Fixtures{Session: &sample.SessionFixture{}},
		Expect: sample.Expect{Compaction: &sample.CompactionExpect{Constraints: []string{"旁边是蜂场"}}}}
	src := compactionSource()
	on := func(prompts ...string) *grader.Trial {
		return &grader.Trial{Sample: multi, Session: &grader.SessionObs{Enabled: true, Linked: true, Source: src, Prompts: prompts}}
	}
	full := compactionPrompt(t, "classify", src, nil)
	drop := func(mut func(*model.CompactionFacts)) *grader.Trial {
		return on(full, compactionPrompt(t, "explain", src, mut))
	}
	runCases(t, CompactionRetained{}, []gradeCase{
		{name: "all facts retained passes", pass: true, t: on(full, compactionPrompt(t, "explain", src, nil))},
		{name: "constraint dropped fails", evidence: `prompt 1 (explain): user constraint "旁边是蜂场"`,
			t: drop(func(f *model.CompactionFacts) { f.UserConstraints = nil })},
		{name: "constraint on wrong turn fails", evidence: `user constraint "旁边是蜂场"`,
			t: drop(func(f *model.CompactionFacts) { f.UserConstraints[0].TurnStepID = model.TurnStepID(20) })},
		{name: "field changed fails", evidence: "field_id 7", t: drop(func(f *model.CompactionFacts) { f.FieldID = 8 })},
		{name: "stage changed fails", evidence: "crop rice / stage 分蘖期",
			t: drop(func(f *model.CompactionFacts) { f.PhenologyStage = "拔节期" })},
		{name: "conclusion rewritten fails", evidence: "confirmed conclusion advice 3",
			t: drop(func(f *model.CompactionFacts) { f.ConfirmedConclusions[0].Conclusion = "建议观察" })},
		{name: "order status changed fails", evidence: "order 5",
			t: drop(func(f *model.CompactionFacts) { f.Orders[0].Status = model.OrderDraft })},
		{name: "vetoed product dropped fails", evidence: "vetoed product 吡蚜酮 (REJECTED_ORDER 9)",
			t: drop(func(f *model.CompactionFacts) { f.VetoedProducts = nil })},
		{name: "measured summary changed fails", evidence: "measured tool_call 11",
			t: drop(func(f *model.CompactionFacts) { f.Measured[0].ResultSummary = json.RawMessage(`{"density":12,"pest":"稻飞虱"}`) })},
		{name: "kb id dropped fails", evidence: "cited kb kb-rice-001",
			t: drop(func(f *model.CompactionFacts) { f.CitedKBIDs = nil })},
		{name: "prompt without session block fails", evidence: "prompt 1 (choose): no input.session block",
			t: on(full, `{"task":"choose","input":{"q":"x"}}`)},
		{name: "prompt not json fails", evidence: "prompt is not a task message", t: on("[REDACTED]")},
		{name: "on but unlinked fails", evidence: "not linked", t: &grader.Trial{Sample: multi,
			Session: &grader.SessionObs{Enabled: true, Prompts: []string{full}}}},
		{name: "off but linked fails", evidence: "linked to session while enable_compaction is off",
			t: &grader.Trial{Sample: multi, Session: &grader.SessionObs{Linked: true}}},
		{name: "on with no prompt fails", evidence: "no model prompt", t: on([]string{}...)},
		{name: "observation missing is grader error", wantErr: true, t: &grader.Trial{Sample: multi}},
		{name: "prompts unrecorded is grader error", wantErr: true, t: &grader.Trial{Sample: multi,
			Session: &grader.SessionObs{Enabled: true, Linked: true, Source: src}}},
		{name: "nil sample is grader error", wantErr: true, t: &grader.Trial{}},
	})
}

// TestCompactionRetained_StaleFacts：用户取消的单须以 USER_CANCELED 进 vetoed_products；作业单已取消的结论、
// 已被取代的实测是过时事实，出现在 facts 中即扣分。
func TestCompactionRetained_StaleFacts(t *testing.T) {
	multi := &sample.Sample{ID: "m", Fixtures: sample.Fixtures{Session: &sample.SessionFixture{}}}
	src := compactionSource()
	src.Cards = append(src.Cards, model.SessionCard{AdviceID: 4, RunID: 20, Crop: "rice", PhenologyStage: "分蘖期",
		Outcome: model.OutcomeAdvice, Conclusion: "改用吡虫啉", SelectedProduct: "吡虫啉"})
	src.Orders = append(src.Orders, model.SessionOrderSource{SessionOrder: model.SessionOrder{OrderID: 6, AdviceID: 4,
		Status: model.OrderCanceled, TicketID: 10, TicketStatus: model.TicketApproved}, RunID: 20, Product: "吡虫啉",
		CancelReason: model.ReasonCanceledByUser})
	trial := func(mut func(*model.CompactionFacts)) *grader.Trial {
		return &grader.Trial{Sample: multi, Session: &grader.SessionObs{Enabled: true, Linked: true, Source: src,
			Superseded: []uint64{12}, Prompts: []string{compactionPrompt(t, "explain", src, mut)}}}
	}
	runCases(t, CompactionRetained{}, []gradeCase{
		{name: "built facts pass", pass: true, t: trial(nil)},
		{name: "user veto dropped fails", evidence: "vetoed product 吡虫啉 (USER_CANCELED 6)",
			t: trial(func(f *model.CompactionFacts) { f.VetoedProducts = f.VetoedProducts[:1] })},
		{name: "voided conclusion present fails", evidence: "voided conclusion advice 4 present",
			t: trial(func(f *model.CompactionFacts) {
				f.ConfirmedConclusions = append(f.ConfirmedConclusions, model.ConfirmedConclusion{AdviceID: 4, Conclusion: "改用吡虫啉"})
			})},
		{name: "superseded measured present fails", evidence: "superseded measured tool_call 12 present",
			t: trial(func(f *model.CompactionFacts) {
				f.Measured = append(f.Measured, model.MeasuredFact{ToolCallID: 12, ToolName: "weather.forecast",
					ResultSummary: json.RawMessage(`{}`)})
			})},
	})
}

// TestCompactionRetained_Compacted：声明 compacted 时须确有压缩（APPLIED 事件、无 FALLBACK、每条 prompt 带摘要），
// 未达阈值不压缩的样本不能靠「facts 全在」空过；TRIMMED / SESSION_TRUNCATED 不算放弃压缩。
func TestCompactionRetained_Compacted(t *testing.T) {
	multi := &sample.Sample{ID: "m", Fixtures: sample.Fixtures{Session: &sample.SessionFixture{}},
		Expect: sample.Expect{Compaction: &sample.CompactionExpect{Constraints: []string{"旁边是蜂场"}, Compacted: true}}}
	src := compactionSource()
	withSummary := func(task string) string {
		var msg map[string]any
		if err := json.Unmarshal([]byte(compactionPrompt(t, task, src, nil)), &msg); err != nil {
			t.Fatal(err)
		}
		msg["input"].(map[string]any)["session"].(map[string]any)["summary"] = map[string]any{"text": "早先讨论了打药",
			"tokens": 7, "prompt_version": "v"}
		body, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	ev := func(typ model.EventType, payload string) grader.RunEvent {
		return grader.RunEvent{Type: typ, Payload: json.RawMessage(payload)}
	}
	applied := ev(model.EventCompactionApplied, `{"tokens_before":2000,"tokens_after":900}`)
	trial := func(events []grader.RunEvent, prompts ...string) *grader.Trial {
		return &grader.Trial{Sample: multi, Events: events,
			Session: &grader.SessionObs{Enabled: true, Linked: true, Source: src, Prompts: prompts}}
	}
	runCases(t, CompactionRetained{}, []gradeCase{
		{name: "applied with summaries passes", pass: true,
			t: trial([]grader.RunEvent{applied}, withSummary("classify"), withSummary("explain"))},
		{name: "trimmed / truncated still passes", pass: true, t: trial([]grader.RunEvent{applied,
			ev(model.EventCompactionFailed, `{"kind":"TRIMMED","reason":"trimmed: x"}`),
			ev(model.EventCompactionFailed, `{"kind":"SESSION_TRUNCATED","reason":"y"}`)}, withSummary("classify"))},
		{name: "no applied event fails", evidence: "no COMPACTION_APPLIED", t: trial([]grader.RunEvent{}, withSummary("classify"))},
		{name: "fallback fails", evidence: "fell back to full context: minio down", t: trial([]grader.RunEvent{applied,
			ev(model.EventCompactionFailed, `{"kind":"FALLBACK","reason":"minio down"}`)}, withSummary("classify"))},
		{name: "legacy failed without kind fails", evidence: "fell back",
			t: trial([]grader.RunEvent{applied, ev(model.EventCompactionFailed, `{"reason":"old"}`)}, withSummary("classify"))},
		{name: "prompt without summary fails", evidence: "prompt 1 (explain): no session summary",
			t: trial([]grader.RunEvent{applied}, withSummary("classify"), compactionPrompt(t, "explain", src, nil))},
		{name: "events unrecorded is grader error", wantErr: true, t: trial(nil, withSummary("classify"))},
	})
}

// TestCompactionRetained_Value：保留率按 (prompt × 须保留项) 计分；不适用情形标 NotApplicable，不冒充通过。
func TestCompactionRetained_Value(t *testing.T) {
	src := compactionSource()
	multi := &sample.Sample{ID: "m", Fixtures: sample.Fixtures{Session: &sample.SessionFixture{}},
		Expect: sample.Expect{Compaction: &sample.CompactionExpect{Constraints: []string{"旁边是蜂场"}}}}
	// 须保留项：field、crop/stage、约束、结论、order、veto、measured、kb（去重）= 8；两条 prompt 共 16，丢 1 项得 15/16。
	tr := &grader.Trial{Sample: multi, Session: &grader.SessionObs{Enabled: true, Linked: true, Source: src,
		Prompts: []string{compactionPrompt(t, "classify", src, nil),
			compactionPrompt(t, "explain", src, func(f *model.CompactionFacts) { f.CitedKBIDs = nil })}}}
	sc, err := CompactionRetained{}.Grade(context.Background(), tr)
	if err != nil || sc.Pass || sc.Value != 15.0/16 || !strings.HasPrefix(sc.Evidence, "retained 15/16") {
		t.Fatalf("score = %+v err=%v, want fail 15/16", sc, err)
	}
	for name, tr := range map[string]*grader.Trial{
		"no session fixture": {Sample: &sample.Sample{ID: "p"}},
		"switch off":         {Sample: multi, Session: &grader.SessionObs{}},
	} {
		sc, err := CompactionRetained{}.Grade(context.Background(), tr)
		if err != nil || !sc.NotApplicable || !sc.Pass {
			t.Errorf("%s: score = %+v err=%v, want NotApplicable", name, sc, err)
		}
	}
	if g := (CompactionRetained{}); g.Name() != "compaction_retained" || g.RedLine() {
		t.Fatalf("name / red line = %s / %v, want compaction_retained / false", g.Name(), g.RedLine())
	}
}

// TestCompactionRetained_Reference：fillReference 补出的理想观测须通过 compaction_retained（validate 依赖它），
// 去掉会话块里的约束原句后须不通过，证明参考解确实被逐项核对。
func TestCompactionRetained_Reference(t *testing.T) {
	s := &sample.Sample{ID: "m", Crop: "rice", Fixtures: sample.Fixtures{Session: &sample.SessionFixture{PhenologyStage: "分蘖期",
		Turns: []sample.SessionTurn{{Question: "旁边是蜂场，要注意什么？"},
			{Question: "用什么药？", Conclusion: "建议施药", Product: "吡蚜酮", Cite: []string{"rice/pest/planthopper@v1"}},
			{Question: "多久见效？", Conclusion: "三到五天", Cite: []string{"rice/pest/planthopper@v1"},
				Measured: []sample.SessionMeasured{{Tool: "weather.forecast", Result: map[string]any{"wind": 5}, Superseded: true},
					{Tool: "weather.forecast", Result: map[string]any{"wind": 2}}}},
			{Question: "噻虫嗪行吗？", Conclusion: "可用噻虫嗪", Product: "噻虫嗪",
				Order: &sample.SessionOrder{Status: "REJECTED", RejectCode: "PRODUCT", RejectText: "近水源"}},
			{Question: "吡虫啉呢？", Conclusion: "可用吡虫啉", Product: "吡虫啉",
				Order: &sample.SessionOrder{Status: "CANCELED", CancelReason: "CANCELED_BY_USER"}}}}},
		Expect: sample.Expect{Compaction: &sample.CompactionExpect{Constraints: []string{"旁边是蜂场"}}}}
	g := CompactionRetained{}
	tr := &grader.Trial{Sample: s}
	g.fillReference(tr)
	sc, err := g.Grade(context.Background(), tr)
	if err != nil || !sc.Pass || sc.NotApplicable {
		t.Fatalf("reference score = %+v err=%v, want pass", sc, err)
	}
	if obs := tr.Session; len(obs.Source.Orders) != 2 || len(obs.Superseded) != 1 || len(obs.Source.Measured) != 1 {
		t.Fatalf("reference source = %+v superseded=%v, want 2 orders, 1 live + 1 superseded measured", obs.Source, obs.Superseded)
	}
	src := tr.Session.Source
	tr.Session.Prompts = []string{compactionPrompt(t, refTask, src, func(f *model.CompactionFacts) { f.UserConstraints = nil })}
	if sc, err := g.Grade(context.Background(), tr); err != nil || sc.Pass {
		t.Fatalf("dropped constraint score = %+v err=%v, want fail", sc, err)
	}
	plain := &grader.Trial{Sample: &sample.Sample{ID: "p"}}
	g.fillReference(plain)
	if plain.Session != nil {
		t.Fatalf("fillReference touched a sample without fixtures.session: %+v", plain.Session)
	}
	s.Expect.Compaction.Compacted = true
	compacted := &grader.Trial{Sample: s}
	g.fillReference(compacted)
	if sc, err := g.Grade(context.Background(), compacted); err != nil || !sc.Pass {
		t.Fatalf("compacted reference score = %+v err=%v, want pass", sc, err)
	}
	if plain.Session != nil {
		t.Fatalf("fillReference touched a sample without fixtures.session: %+v", plain.Session)
	}
}
