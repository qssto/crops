package compaction

import (
	"bytes"
	"slices"

	"agri-steward/internal/model"
)

// BuildFacts 由会话原料提取白名单事实（P2 §6.7 技术实现 3）。只做拷贝与筛选，不改写任何取值，
// 结果不与 src 共享底层数组（断言要拿重新读出的源比对，facts 被改不能连带改到源）：
//   - crop / phenology_stage 取最后一张带作物的建议卡；
//   - user_constraints 取此前各轮问题中命中词表的原句（链被截断时先取汇总的更早 run，见 constraintTurns）；
//   - orders 原样取作业单与最新票据、驳回原因；vetoed_products 见 vetoOf；
//   - measured 原样取时效实测；confirmed_conclusions 见 confirmedCards；cited_kb_ids 去重排序。
func BuildFacts(src model.SessionSource, lex *Lexicon) model.CompactionFacts {
	f := model.CompactionFacts{FieldID: src.FieldID}
	for _, c := range src.Cards {
		if c.Crop != "" {
			f.Crop, f.PhenologyStage = c.Crop, c.PhenologyStage
		}
	}
	for _, t := range constraintTurns(src) {
		for _, s := range lex.Constraints(t.Question) {
			f.UserConstraints = append(f.UserConstraints, model.UserConstraint{Text: s, TurnStepID: model.TurnStepID(t.RunID)})
		}
	}
	for _, o := range src.Orders {
		f.Orders = append(f.Orders, o.SessionOrder)
	}
	f.VetoedProducts = vetoesOf(src)
	for _, m := range src.Measured {
		m.ResultSummary = bytes.Clone(m.ResultSummary)
		f.Measured = append(f.Measured, m)
	}
	for _, c := range confirmedCards(src) {
		cc := model.ConfirmedConclusion{AdviceID: c.AdviceID, Conclusion: c.Conclusion,
			Numbers: slices.Clone(c.Numbers), SelectedProduct: c.SelectedProduct}
		if c.Window != nil {
			w := *c.Window
			cc.Window = &w
		}
		f.ConfirmedConclusions = append(f.ConfirmedConclusions, cc)
	}
	f.CitedKBIDs = uniqueSorted(src.CitedKBIDs)
	return f
}

// vetoOf 判定作业单是否产出否决药剂（对应建议须有药剂）：
//   - 最新票据 REJECTED 且驳回原因 PRODUCT → REJECTED_ORDER（ref_id 为票据 id）；
//   - 作业单 CANCELED 且迁移原因 CANCELED_BY_USER（用户 / 审批人中止）→ USER_CANCELED（ref_id 为作业单 id）。
func vetoOf(o model.SessionOrderSource) (model.VetoedProduct, bool) {
	switch {
	case o.Product == "":
		return model.VetoedProduct{}, false
	case o.TicketStatus == model.TicketRejected && o.RejectReasonCode == string(model.RejectProduct):
		return model.VetoedProduct{Product: o.Product, Source: model.VetoedProductRejected, RefID: o.TicketID}, true
	case o.Status == model.OrderCanceled && o.CancelReason == model.ReasonCanceledByUser:
		return model.VetoedProduct{Product: o.Product, Source: model.VetoedProductUserCanceled, RefID: o.OrderID}, true
	}
	return model.VetoedProduct{}, false
}

// constraintTurns 是提取用户约束的轮次：链被截断时汇总的更早 run 在前，随后是会话链轮次。
func constraintTurns(src model.SessionSource) []model.SessionTurn {
	return append(slices.Clone(src.EarlierTurns), src.Turns...)
}

// vetoesOf 是全部否决药剂：先汇总的更早 run 的作业单，再会话链作业单，各按源顺序。
func vetoesOf(src model.SessionSource) []model.VetoedProduct {
	var out []model.VetoedProduct
	for _, list := range [][]model.SessionOrderSource{src.EarlierOrders, src.Orders} {
		for _, o := range list {
			if v, ok := vetoOf(o); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// confirmedCards 是可作「已确认结论」的建议卡（按源顺序）：outcome=ADVICE，且其作业单未被驳回 / 取消
// （否则模型会同时看到「已确认用 X」与「X 已否决」）。BuildFacts 与 Assert 共用同一判定。
func confirmedCards(src model.SessionSource) []model.SessionCard {
	voided := map[uint64]bool{}
	for _, o := range src.Orders {
		if o.AdviceID != 0 && (o.Status == model.OrderRejected || o.Status == model.OrderCanceled) {
			voided[o.AdviceID] = true
		}
	}
	var out []model.SessionCard
	for _, c := range src.Cards {
		if c.Outcome == model.OutcomeAdvice && !voided[c.AdviceID] {
			out = append(out, c)
		}
	}
	return out
}

func uniqueSorted(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
