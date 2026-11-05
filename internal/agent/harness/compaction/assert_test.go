package compaction

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// 红线：白名单断言的应失败测试（先写，P2 §6.7 技术实现 5）。最终会话块任一白名单字段缺失、被改写或与
// 重新读出的源表不一致，Assert 都必须返回 ErrWhitelist。

func testLexicon(t *testing.T) *Lexicon {
	t.Helper()
	l, err := NewLexicon(1, "不能用", "蜂场", "鱼塘", "有机")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

var t0 = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

// testSource 是 6 轮会话：第 1 轮带禁忌、第 2 轮有建议卡与被驳回（PRODUCT）的单（其结论不算已确认）、
// 第 3 轮有建议卡与已批单（唯一的已确认结论）。
func testSource() model.SessionSource {
	win := &model.TimeWindow{From: t0.Add(24 * time.Hour), To: t0.Add(48 * time.Hour)}
	return model.SessionSource{
		RunID: 107, FieldID: 9,
		Turns: []model.SessionTurn{
			{RunID: 101, Question: "这块地旁边是蜂场。稻飞虱怎么治？", Answer: "先观察"},
			{RunID: 102, Question: "不能用吡蚜酮，换一个；鱼塘也在边上", Answer: "建议用噻虫嗪{{n0}}"},
			{RunID: 103, Question: "那什么时候打？", Answer: "明早作业"},
			{RunID: 104, Question: "雨后还要补打吗？", Answer: "看虫量"},
			{RunID: 105, Question: "叶片发黄是什么原因", Answer: "可能缺氮"},
			{RunID: 106, Question: "要追肥吗", Answer: "建议追施尿素"},
		},
		Cards: []model.SessionCard{
			{AdviceID: 201, RunID: 101, Crop: "rice", PhenologyStage: "tillering", Outcome: model.OutcomeRefused,
				Conclusion: "先观察"},
			{AdviceID: 202, RunID: 102, Crop: "rice", PhenologyStage: "jointing", Outcome: model.OutcomeAdvice,
				Conclusion: "建议用噻虫嗪{{n0}}", Numbers: []model.Number{{Field: "dose", Value: 4.5, Unit: "g/亩"}},
				SelectedProduct: "噻虫嗪", Window: win},
			{AdviceID: 203, RunID: 103, Crop: "rice", PhenologyStage: "jointing", Outcome: model.OutcomeAdvice,
				Conclusion: "明早作业{{n0}}", Numbers: []model.Number{{Field: "dose", Value: 4.5, Unit: "g/亩"}},
				SelectedProduct: "吡蚜酮", Window: win},
		},
		Orders: []model.SessionOrderSource{
			{SessionOrder: model.SessionOrder{OrderID: 301, AdviceID: 202, Status: model.OrderRejected, TicketID: 401,
				TicketStatus: model.TicketRejected, RejectReasonCode: string(model.RejectProduct), RejectText: "旁边有鱼塘"},
				RunID: 102, Product: "噻虫嗪"},
			{SessionOrder: model.SessionOrder{OrderID: 302, AdviceID: 203, Status: model.OrderApproved, TicketID: 402,
				TicketStatus: model.TicketApproved}, RunID: 103, Product: "吡蚜酮"},
		},
		Measured: []model.MeasuredFact{
			{ToolCallID: 501, ToolName: "weather.forecast", ResultSummary: json.RawMessage(`{"rain_mm":12}`), CollectedAt: t0},
		},
		CitedKBIDs: []string{"kb-pest-002", "kb-pest-001", "kb-pest-002"},
	}
}

// validBlock 是摘要 + 最近 2 轮原文的会话块（白名单完整）。
func validBlock(t *testing.T, src model.SessionSource, lex *Lexicon) Block {
	t.Helper()
	turns := turnsOf(src.Turns)
	return Block{CompactionVersion: model.CompactionVersion, Facts: BuildFacts(src, lex),
		Summary: &model.CompactionSummary{Text: "早先讨论了稻飞虱与施药时间", Tokens: 12, PromptVersion: "test"},
		Turns:   turns[len(turns)-2:]}
}

func mustEncode(t *testing.T, b Block) []byte {
	t.Helper()
	out, err := encode(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAssert_ValidBlockPasses(t *testing.T) {
	lex, src := testLexicon(t), testSource()
	if err := Assert(mustEncode(t, validBlock(t, src, lex)), src, lex); err != nil {
		t.Fatalf("valid block: %v", err)
	}
	full := Block{CompactionVersion: model.CompactionVersion, Facts: BuildFacts(src, lex), Turns: turnsOf(src.Turns)}
	if err := Assert(mustEncode(t, full), src, lex); err != nil {
		t.Fatalf("full block: %v", err)
	}
}

func TestAssert_BlockMutationsFail(t *testing.T) {
	lex := testLexicon(t)
	for name, mutate := range map[string]func(b *Block){
		"field_id changed":        func(b *Block) { b.Facts.FieldID = 10 },
		"crop dropped":            func(b *Block) { b.Facts.Crop = "" },
		"phenology_stage changed": func(b *Block) { b.Facts.PhenologyStage = "tillering" },
		"constraint dropped":      func(b *Block) { b.Facts.UserConstraints = b.Facts.UserConstraints[1:] },
		"constraint rewritten":    func(b *Block) { b.Facts.UserConstraints[0].Text = "旁边有蜂场" },
		"constraint wrong turn":   func(b *Block) { b.Facts.UserConstraints[0].TurnStepID = "run-102" },
		"order dropped":           func(b *Block) { b.Facts.Orders = b.Facts.Orders[:1] },
		"order status changed":    func(b *Block) { b.Facts.Orders[1].Status = model.OrderDispatched },
		"ticket status changed":   func(b *Block) { b.Facts.Orders[0].TicketStatus = model.TicketApproved },
		"ticket id changed":       func(b *Block) { b.Facts.Orders[0].TicketID = 499 },
		"reject code dropped":     func(b *Block) { b.Facts.Orders[0].RejectReasonCode = "" },
		"reject text rewritten":   func(b *Block) { b.Facts.Orders[0].RejectText = "药不对" },
		"stale order added": func(b *Block) {
			b.Facts.Orders = append(b.Facts.Orders, model.SessionOrder{OrderID: 399, Status: model.OrderDraft})
		},
		"vetoed product dropped": func(b *Block) { b.Facts.VetoedProducts = nil },
		"vetoed product changed": func(b *Block) { b.Facts.VetoedProducts[0].Product = "吡蚜酮" },
		"measured dropped":       func(b *Block) { b.Facts.Measured = nil },
		"measured value changed": func(b *Block) { b.Facts.Measured[0].ResultSummary = json.RawMessage(`{"rain_mm":2}`) },
		"measured time dropped":  func(b *Block) { b.Facts.Measured[0].CollectedAt = time.Time{} },
		"measured time changed":  func(b *Block) { b.Facts.Measured[0].CollectedAt = t0.Add(time.Hour) },
		"conclusion dropped":     func(b *Block) { b.Facts.ConfirmedConclusions = b.Facts.ConfirmedConclusions[1:] },
		"conclusion rewritten":   func(b *Block) { b.Facts.ConfirmedConclusions[0].Conclusion = "建议用噻虫嗪" },
		"dose changed":           func(b *Block) { b.Facts.ConfirmedConclusions[0].Numbers[0].Value = 5 },
		"dose unit changed":      func(b *Block) { b.Facts.ConfirmedConclusions[0].Numbers[0].Unit = "g/ha" },
		"dose dropped":           func(b *Block) { b.Facts.ConfirmedConclusions[0].Numbers = nil },
		"product changed":        func(b *Block) { b.Facts.ConfirmedConclusions[0].SelectedProduct = "吡虫啉" },
		"window dropped":         func(b *Block) { b.Facts.ConfirmedConclusions[0].Window = nil },
		"window shifted": func(b *Block) {
			w := *b.Facts.ConfirmedConclusions[0].Window
			w.To = w.To.Add(time.Hour)
			b.Facts.ConfirmedConclusions[0].Window = &w
		},
		"refused card as advice": func(b *Block) {
			b.Facts.ConfirmedConclusions = append(b.Facts.ConfirmedConclusions, model.ConfirmedConclusion{AdviceID: 201, Conclusion: "先观察"})
		},
		"voided conclusion added": func(b *Block) {
			b.Facts.ConfirmedConclusions = append(b.Facts.ConfirmedConclusions,
				model.ConfirmedConclusion{AdviceID: 202, Conclusion: "建议用噻虫嗪{{n0}}", SelectedProduct: "噻虫嗪"})
		},
		"order advice_id changed":  func(b *Block) { b.Facts.Orders[0].AdviceID = 203 },
		"kb id dropped":            func(b *Block) { b.Facts.CitedKBIDs = b.Facts.CitedKBIDs[:1] },
		"kb id changed":            func(b *Block) { b.Facts.CitedKBIDs[0] = "kb-pest-009" },
		"kept turn rewritten":      func(b *Block) { b.Turns[1].Answer = "建议追施复合肥" },
		"kept turn dropped":        func(b *Block) { b.Turns = b.Turns[:1] },
		"turns without summary":    func(b *Block) { b.Summary = nil },
		"wrong compaction version": func(b *Block) { b.CompactionVersion = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			src := testSource()
			b := validBlock(t, src, lex)
			mutate(&b)
			if err := Assert(mustEncode(t, b), src, lex); !errors.Is(err, ErrWhitelist) {
				t.Fatalf("err = %v, want ErrWhitelist", err)
			}
		})
	}
}

// 取值从源表重新读取：会话块与组装时的 facts 一致，但重新读出的源表已变（如作业单被取消），断言同样失败。
func TestAssert_SourceChangedFails(t *testing.T) {
	lex := testLexicon(t)
	for name, mutate := range map[string]func(s *model.SessionSource){
		"order canceled": func(s *model.SessionSource) { s.Orders[1].Status = model.OrderCanceled },
		"new measured": func(s *model.SessionSource) {
			s.Measured = append(s.Measured, model.MeasuredFact{ToolCallID: 502, ToolName: "x", ResultSummary: json.RawMessage(`1`), CollectedAt: t0})
		},
		"new cited kb":   func(s *model.SessionSource) { s.CitedKBIDs = append(s.CitedKBIDs, "kb-new") },
		"new constraint": func(s *model.SessionSource) { s.Turns[4].Question = "这块地是有机认证的" },
		"field moved":    func(s *model.SessionSource) { s.FieldID = 11 },
		"new advice card": func(s *model.SessionSource) {
			s.Cards = append(s.Cards, model.SessionCard{AdviceID: 206, RunID: 106, Outcome: model.OutcomeAdvice, Conclusion: "追肥"})
		},
		"latest turn answer": func(s *model.SessionSource) { s.Turns[5].Answer = "暂不追肥" },
		"confirmed order rejected": func(s *model.SessionSource) {
			s.Orders[1].Status, s.Orders[1].TicketStatus = model.OrderRejected, model.TicketRejected
		},
		"user canceled order": func(s *model.SessionSource) {
			s.Orders[1].Status, s.Orders[1].CancelReason = model.OrderCanceled, model.ReasonCanceledByUser
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := testSource()
			block := mustEncode(t, validBlock(t, src, lex))
			reread := testSource()
			mutate(&reread)
			if err := Assert(block, reread, lex); !errors.Is(err, ErrWhitelist) {
				t.Fatalf("err = %v, want ErrWhitelist", err)
			}
		})
	}
}

func TestAssert_MalformedBlockFails(t *testing.T) {
	lex, src := testLexicon(t), testSource()
	for name, raw := range map[string]string{
		"not json":      `{"facts":`,
		"empty":         ``,
		"no facts":      `{"compaction_version":1}`,
		"unknown field": `{"compaction_version":1,"facts":{"field_id":9},"extra":1}`,
	} {
		if err := Assert([]byte(raw), src, lex); !errors.Is(err, ErrWhitelist) {
			t.Errorf("%s: err = %v, want ErrWhitelist", name, err)
		}
	}
}

// 已驳回 / 已取消单对应的结论不进 confirmed_conclusions；用户中止的单产出 USER_CANCELED 否决药剂，系统取消不产出。
func TestBuildFacts_VoidedOrdersAndUserVeto(t *testing.T) {
	lex := testLexicon(t)
	f := BuildFacts(testSource(), lex)
	if len(f.ConfirmedConclusions) != 1 || f.ConfirmedConclusions[0].AdviceID != 203 {
		t.Fatalf("conclusions = %+v, want only 203 (202's order rejected)", f.ConfirmedConclusions)
	}
	if len(f.VetoedProducts) != 1 || f.VetoedProducts[0] != (model.VetoedProduct{Product: "噻虫嗪",
		Source: model.VetoedProductRejected, RefID: 401}) {
		t.Fatalf("vetoes = %+v", f.VetoedProducts)
	}
	for name, tc := range map[string]struct {
		reason model.ReasonCode
		vetoes int
	}{
		"user canceled":   {reason: model.ReasonCanceledByUser, vetoes: 2},
		"system canceled": {reason: model.ReasonConditionChanged, vetoes: 1},
	} {
		t.Run(name, func(t *testing.T) {
			src := testSource()
			src.Orders[1].Status, src.Orders[1].CancelReason = model.OrderCanceled, tc.reason
			f := BuildFacts(src, lex)
			if len(f.ConfirmedConclusions) != 0 {
				t.Fatalf("conclusions = %+v, want none (both orders voided)", f.ConfirmedConclusions)
			}
			if len(f.VetoedProducts) != tc.vetoes {
				t.Fatalf("vetoes = %+v, want %d", f.VetoedProducts, tc.vetoes)
			}
			if tc.vetoes == 2 && f.VetoedProducts[1] != (model.VetoedProduct{Product: "吡蚜酮",
				Source: model.VetoedProductUserCanceled, RefID: 302}) {
				t.Fatalf("user veto = %+v", f.VetoedProducts[1])
			}
			block := Block{CompactionVersion: model.CompactionVersion, Facts: f, Turns: turnsOf(src.Turns)}
			if err := Assert(mustEncode(t, block), src, lex); err != nil {
				t.Fatalf("assert: %v", err)
			}
		})
	}
}

// truncatedSource 是会话链被截断的 testSource：更早 run 有一条约束原句与一张被 PRODUCT 驳回的作业单。
func truncatedSource() model.SessionSource {
	src := testSource()
	src.Truncated, src.EarlierRuns = true, 3
	src.EarlierTurns = []model.SessionTurn{{RunID: 60, Question: "这块地不能用毒死蜱", Answer: "好的"}}
	src.EarlierOrders = []model.SessionOrderSource{{SessionOrder: model.SessionOrder{OrderID: 290, AdviceID: 190,
		Status: model.OrderRejected, TicketID: 390, TicketStatus: model.TicketRejected,
		RejectReasonCode: string(model.RejectProduct), RejectText: "毒死蜱禁用"}, RunID: 60, Product: "毒死蜱"}}
	return src
}

// 会话链截断：更早 run 的约束 / 否决按地块汇总进 facts（排在链内之前），断言把它们视为白名单。
func TestBuildFacts_EarlierRunsAggregated(t *testing.T) {
	lex, src := testLexicon(t), truncatedSource()
	f := BuildFacts(src, lex)
	if len(f.UserConstraints) != 4 || f.UserConstraints[0] != (model.UserConstraint{Text: "这块地不能用毒死蜱", TurnStepID: "run-60"}) {
		t.Fatalf("constraints = %+v, want earlier constraint first", f.UserConstraints)
	}
	if len(f.VetoedProducts) != 2 || f.VetoedProducts[0] != (model.VetoedProduct{Product: "毒死蜱",
		Source: model.VetoedProductRejected, RefID: 390}) {
		t.Fatalf("vetoes = %+v, want earlier veto first", f.VetoedProducts)
	}
	if len(f.Orders) != 2 {
		t.Fatalf("orders = %+v, want chain orders only", f.Orders)
	}
	block := mustEncode(t, Block{CompactionVersion: model.CompactionVersion, Facts: f, Turns: turnsOf(src.Turns)})
	if err := Assert(block, src, lex); err != nil {
		t.Fatalf("assert: %v", err)
	}
	// 只按链内源表组装（丢了更早 run 的约束 / 否决）的块必须被断言拒绝。
	chainOnly := testSource()
	short := mustEncode(t, Block{CompactionVersion: model.CompactionVersion, Facts: BuildFacts(chainOnly, lex),
		Turns: turnsOf(chainOnly.Turns)})
	if err := Assert(short, src, lex); !errors.Is(err, ErrWhitelist) {
		t.Fatalf("block missing earlier facts: err = %v, want ErrWhitelist", err)
	}
}
