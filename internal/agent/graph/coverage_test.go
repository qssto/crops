package graph_test

import (
	"context"
	"slices"
	"testing"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// P2 L4 覆盖检查（生育期、PHI、product_candidates 是否已全部由规则层给出）：发现的绕过路径先写应失败测试再修。

const sPanicleFert = "rice-fert-0002" // 拔节孕穗期施穗肥：作业类，按氮肥运筹算尿素用量

// TestAgentAsk_FertStageFollowsPhenology（覆盖检查·生育期）：施肥时期取自分类槽位（模型从问题里抽取），
// 物候工具给出的当前生育期不在该时期条目的适用生育期内时，规则层不得给作业类结论（只给观察类、不出作业单）；
// 用量照算（提前规划）。修复前：时期条目的阈值因生育期不符全部「不适用」，被当成满足，给出作业类。
func TestAgentAsk_FertStageFollowsPhenology(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sPanicleFert)
	tillering := withToolValue(s, toolPhenology, "stage", "分蘖期")
	store := newMemStore()
	clk := sampleClock(t, tillering)
	a := h.newAgent(t, clk, deps{tools: fixtureTools(t, tillering, clk), store: store})
	q, err := questionOf(tillering)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	c := ans.Card
	if ans.Outcome != model.OutcomeAdvice || c.AdviceType != model.AdviceTypeObservation || store.orderCount() != 0 {
		t.Fatalf("outcome %s type %q orders %d, want observation without order", ans.Outcome, c.AdviceType, store.orderCount())
	}
	if len(c.Numbers) == 0 {
		t.Error("rate question should still get calc numbers")
	}
}

// TestAgentAsk_PHIWithoutHarvestWords（覆盖检查·PHI）：问题没提收获，但预计收获日已近到所选药剂的安全间隔期之内时，
// 规则层不过滤候选（P1 L3 设计），由开票前约束闸门的 phi_interval 拦截：单留 DRAFT、不开票。
// 确认该路径无绕过（问题措辞不影响拦截）；若规则层改为提前过滤，红线样本 rice-cons-0012 会变。
func TestAgentAsk_PHIWithoutHarvestWords(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation) // 时钟 2026-07-18，问题未提收获
	near := withHarvest("2026-07-28")(s)           // 距收获 10 天，吡蚜酮 PHI 14 天
	r := h.runWithReview(t, near, reviewGate{rv: h.newReviewer(t)})
	ans := r.ans
	if ans.Outcome != model.OutcomeAdvice || ans.Order == nil || ans.Ticket != nil {
		t.Fatalf("outcome %s order %v ticket %v, want advice with DRAFT order and no ticket", ans.Outcome, ans.Order != nil, ans.Ticket)
	}
	if !slices.Contains(ans.ConstraintBlocked, "phi_interval") {
		t.Fatalf("blocked %v, want phi_interval (selected %s)", ans.ConstraintBlocked, ans.Card.SelectedProduct)
	}
}

// TestAgentAsk_ProductMentionOutsideCandidates（覆盖检查·product_candidates）：选药在候选内，模型结论却建议候选外的
// 药剂 → REFUSED / VALIDATION_FAILED（check=product）。修复前：护栏只查卡片字段，模型原文里的药名不受约束。
func TestAgentAsk_ProductMentionOutsideCandidates(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	m := scriptedModel{override: map[llm.Task]string{
		llm.TaskExplain: `{"conclusion":"达到防治条件，建议在作业窗口内施药，建议改用毒死蜱效果更好","reasons":[]}`}}
	store, ev := newMemStore(), &recordingEvents{}
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: m, tools: fixtureTools(t, s, clk), store: store, events: ev})
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
	if got := validationChecks(t, ev); len(got) != 1 || got[0] != "product" {
		t.Fatalf("VALIDATION_FAILED checks = %v, want [product]", got)
	}
}

// TestAgentAsk_ProductMentionWithoutCandidates（P2 L4 审查修复·product_candidates）：规则层无药剂候选（观察类）时，
// 模型原文肯定推荐任何药剂 → REFUSED / VALIDATION_FAILED（check=product）。修复前：无候选时不查原文药名。
func TestAgentAsk_ProductMentionWithoutCandidates(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sObservation)
	m := scriptedModel{override: map[llm.Task]string{
		llm.TaskExplain: `{"conclusion":"注意施药防护，明天喷吡虫啉","reasons":[]}`}}
	store, ev := newMemStore(), &recordingEvents{}
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: m, tools: fixtureTools(t, s, clk), store: store, events: ev})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Outcome != model.OutcomeRefused || ans.Card.RefuseReason != "VALIDATION_FAILED" {
		t.Fatalf("outcome %s/%q, want REFUSED VALIDATION_FAILED", ans.Outcome, ans.Card.RefuseReason)
	}
	if got := validationChecks(t, ev); len(got) != 1 || got[0] != "product" {
		t.Fatalf("VALIDATION_FAILED checks = %v, want [product]", got)
	}
}
