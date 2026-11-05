package code

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// refTask 是参考解 prompt 的任务名（任意模型任务均携带同一会话块）。
const refTask = "explain"

// refSummary 是参考解会话块的摘要文本（内容不打分，只核对存在）。
const refSummary = "早先各轮的摘要"

var _ referenceFiller = CompactionRetained{}

// refCollectedAt 是参考解时效实测的取数时间。
var refCollectedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fillReference 补 fixtures.session 样本的理想观测（validate 参考解检查）：种子轮次按 run_id 1..n 读回业务表
// （建议卡 / 作业单 / 票据 id 均取所在轮次 run_id，实测 id 按出现顺序 1..m），开关开且已续接，唯一一条 prompt 的
// 会话块 facts 原样带上地块、作物 / 物候期、约束原句（所在首个轮次）、作业单与否决药剂、未被取代的实测、
// 作业单未驳回 / 取消的已落卡结论与 kb 引用；声明 compacted 时会话块带摘要，并追加一条 COMPACTION_APPLIED 事件。
// 无 fixtures.session 时不动。
func (CompactionRetained) fillReference(t *grader.Trial) {
	fx := t.Sample.Fixtures.Session
	if fx == nil {
		return
	}
	src := model.SessionSource{RunID: uint64(len(fx.Turns) + 1), FieldID: refID}
	facts := model.CompactionFacts{FieldID: refID}
	var superseded []uint64
	for i, turn := range fx.Turns {
		runID := uint64(i + 1)
		src.Turns = append(src.Turns, model.SessionTurn{RunID: runID, Question: turn.Question, Answer: turn.Conclusion})
		for _, m := range turn.Measured {
			id := uint64(len(src.Measured) + len(superseded) + 1)
			if m.Superseded {
				superseded = append(superseded, id)
				continue
			}
			body, err := json.Marshal(m.Result)
			if err != nil {
				continue // sample.Validate 已校验可编码
			}
			mf := model.MeasuredFact{ToolCallID: id, ToolName: m.Tool, ResultSummary: body, CollectedAt: refCollectedAt}
			src.Measured, facts.Measured = append(src.Measured, mf), append(facts.Measured, mf)
		}
		if turn.Conclusion == "" {
			continue
		}
		src.Cards = append(src.Cards, model.SessionCard{AdviceID: runID, RunID: runID, Crop: t.Sample.Crop,
			PhenologyStage: fx.PhenologyStage, Outcome: model.OutcomeAdvice, Conclusion: turn.Conclusion,
			SelectedProduct: turn.Product})
		facts.Crop, facts.PhenologyStage = t.Sample.Crop, fx.PhenologyStage
		if turn.Order != nil {
			o := refOrder(runID, turn.Product, *turn.Order)
			src.Orders, facts.Orders = append(src.Orders, o), append(facts.Orders, o.SessionOrder)
			if v, ok := refVeto(o); ok {
				facts.VetoedProducts = append(facts.VetoedProducts, v)
			}
		} else {
			facts.ConfirmedConclusions = append(facts.ConfirmedConclusions, model.ConfirmedConclusion{AdviceID: runID,
				Conclusion: turn.Conclusion, SelectedProduct: turn.Product})
		}
		for _, ref := range turn.Cite {
			if id, _, ok := sample.SplitCite(ref); ok {
				src.CitedKBIDs = append(src.CitedKBIDs, id)
			}
		}
	}
	facts.CitedKBIDs = slices.Compact(slices.Sorted(slices.Values(src.CitedKBIDs)))
	if exp := t.Sample.Expect.Compaction; exp != nil {
		for _, c := range exp.Constraints {
			for _, turn := range src.Turns {
				if strings.Contains(turn.Question, c) {
					facts.UserConstraints = append(facts.UserConstraints, model.UserConstraint{Text: c,
						TurnStepID: model.TurnStepID(turn.RunID)})
					break
				}
			}
		}
	}
	block := compaction.Block{CompactionVersion: model.CompactionVersion, Facts: facts}
	if exp := t.Sample.Expect.Compaction; exp != nil && exp.Compacted {
		block.Summary = &model.CompactionSummary{Text: refSummary, Tokens: compaction.EstimateTokens(refSummary),
			PromptVersion: compaction.DeterministicPromptVersion}
		t.Events = append(t.Events, grader.RunEvent{Type: model.EventCompactionApplied, Actor: string(model.ActorAgent),
			Payload: json.RawMessage(`{"tokens_before":2000,"tokens_after":900}`)})
	}
	body, err := json.Marshal(map[string]any{"task": refTask, "input": map[string]any{"session": block}})
	obs := &grader.SessionObs{Enabled: true, Linked: true, Source: src, Superseded: superseded, Prompts: []string{}}
	if err == nil {
		obs.Prompts = append(obs.Prompts, string(body))
	}
	t.Session = obs
}

// refOrder 是种子终态作业单读回的样子：REJECTED 带已驳回票与原因，CANCELED 带已通过票与取消迁移原因。
func refOrder(runID uint64, product string, o sample.SessionOrder) model.SessionOrderSource {
	so := model.SessionOrderSource{RunID: runID, Product: product, CancelReason: model.ReasonCode(o.CancelReason),
		SessionOrder: model.SessionOrder{OrderID: runID, AdviceID: runID, Status: model.WorkOrderStatus(o.Status),
			TicketID: runID, TicketStatus: model.TicketApproved}}
	if so.Status == model.OrderRejected {
		so.TicketStatus, so.RejectReasonCode, so.RejectText = model.TicketRejected, o.RejectCode, o.RejectText
	}
	return so
}

// refVeto 是参考解的否决药剂（与打分器 orderItems 同一规则）。
func refVeto(o model.SessionOrderSource) (model.VetoedProduct, bool) {
	switch {
	case o.Product == "":
	case o.TicketStatus == model.TicketRejected && o.RejectReasonCode == string(model.RejectProduct):
		return model.VetoedProduct{Product: o.Product, Source: model.VetoedProductRejected, RefID: o.TicketID}, true
	case o.Status == model.OrderCanceled && o.CancelReason == model.ReasonCanceledByUser:
		return model.VetoedProduct{Product: o.Product, Source: model.VetoedProductUserCanceled, RefID: o.OrderID}, true
	}
	return model.VetoedProduct{}, false
}
