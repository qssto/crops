//go:build integration

package env_test

import (
	"reflect"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P2 L6 多轮会话种子与打分真值：种子写进作业单 / 票据 / 驳回原因 / 取消迁移 / 时效实测后，独立真值 SQL
// （ReadSessionTruth）与生产 LoadSession 读出同一份原料，已取代的实测只进 Superseded；facts 据此排除过时结论。
// 复用 repository 的 repop2l6 库（不新建 schema）。
func TestSeedSession_Truth_Integration(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 7, 30, 0, 0, time.UTC)
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	fx := &sample.SessionFixture{PhenologyStage: "分蘖期", Turns: []sample.SessionTurn{
		{Question: "西边河道通着水源地，别让药流进河里"},
		{Question: "噻虫嗪行吗？", Conclusion: "可用噻虫嗪", Product: "噻虫嗪", Cite: []string{"rice/pest/planthopper@v1"},
			Order: &sample.SessionOrder{Status: "REJECTED", RejectCode: "PRODUCT", RejectText: "近水源换药"}},
		{Question: "吡虫啉呢？", Conclusion: "可用吡虫啉", Product: "吡虫啉",
			Order: &sample.SessionOrder{Status: "CANCELED", CancelReason: "CANCELED_BY_USER"},
			Measured: []sample.SessionMeasured{{Tool: "weather.forecast", Result: map[string]any{"wind_speed_ms": 5.5}, Superseded: true},
				{Tool: "weather.forecast", Result: map[string]any{"wind_speed_ms": 2.0}}}},
		{Question: "那用吡蚜酮", Conclusion: "吡蚜酮按登记用量", Product: "吡蚜酮", Cite: []string{"rice/pest/planthopper@v1"}},
	}}
	seeded, err := env.SeedSession(ctx, db, env.SessionSeed{Scope: scope, FieldID: 1, Crop: "rice", KnowledgeRef: "kb@test",
		Now: t0, Fixture: fx})
	if err != nil || len(seeded) != len(fx.Turns) {
		t.Fatalf("seed = %v err=%v, want %d runs", seeded, err, len(fx.Turns))
	}
	repo := repository.NewRunRepo(db, slot.Clock)
	meta := agent.RunMeta{GraphVersion: "g1", PromptVersion: "p1", ModelTier: "small", KnowledgeRef: "kb@test"}
	rs := model.RunState{TenantID: scope.TenantID, FarmID: scope.FarmID}
	cur, linked, err := repo.CreateRunLinked(ctx, rs, scope.UserID, 1, meta,
		model.RunLink{Kind: model.RunLinkSession, RunID: seeded[len(seeded)-1], Idle: 30 * time.Minute})
	if err != nil || !linked {
		t.Fatalf("create linked run = %d linked=%v err=%v", cur, linked, err)
	}

	truth, err := env.ReadSessionTruth(ctx, db, scope, seeded, cur)
	if err != nil {
		t.Fatal(err)
	}
	if !truth.Linked || len(truth.Superseded) != 1 || len(truth.Source.Measured) != 1 || len(truth.Source.Orders) != 2 {
		t.Fatalf("truth = %+v", truth)
	}
	got := truth.Source
	if o := got.Orders[0]; o.Status != model.OrderRejected || o.TicketStatus != model.TicketRejected ||
		o.RejectReasonCode != "PRODUCT" || o.RejectText != "近水源换药" || o.Product != "噻虫嗪" {
		t.Fatalf("rejected order = %+v", o)
	}
	if o := got.Orders[1]; o.Status != model.OrderCanceled || o.TicketStatus != model.TicketApproved ||
		o.CancelReason != model.ReasonCanceledByUser || o.Product != "吡虫啉" {
		t.Fatalf("canceled order = %+v", o)
	}
	prod, err := repo.LoadSession(ctx, scope, cur)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]any{"turns": {got.Turns, prod.Turns}, "cards": {got.Cards, prod.Cards},
		"orders": {got.Orders, prod.Orders}, "measured": {got.Measured, prod.Measured}, "kb": {got.CitedKBIDs, prod.CitedKBIDs},
		"field": {got.FieldID, prod.FieldID}} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s: truth %+v != LoadSession %+v", name, pair[0], pair[1])
		}
	}
	lex, err := compaction.NewLexicon(1, "水源地")
	if err != nil {
		t.Fatal(err)
	}
	f := compaction.BuildFacts(got, lex)
	if len(f.VetoedProducts) != 2 || f.VetoedProducts[0].Source != model.VetoedProductRejected ||
		f.VetoedProducts[1] != (model.VetoedProduct{Product: "吡虫啉", Source: model.VetoedProductUserCanceled, RefID: got.Orders[1].OrderID}) {
		t.Fatalf("vetoed = %+v", f.VetoedProducts)
	}
	if len(f.ConfirmedConclusions) != 1 || f.ConfirmedConclusions[0].SelectedProduct != "吡蚜酮" {
		t.Fatalf("confirmed = %+v, want only the 吡蚜酮 card", f.ConfirmedConclusions)
	}

	other, _, err := repo.CreateRunLinked(ctx, rs, scope.UserID, 1, meta, model.RunLink{})
	if err != nil {
		t.Fatal(err)
	}
	if unlinked, err := env.ReadSessionTruth(ctx, db, scope, seeded, other); err != nil || unlinked.Linked {
		t.Fatalf("unlinked truth linked=%v err=%v", unlinked.Linked, err)
	}
}
