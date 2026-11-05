//go:build integration

package repository_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func kbItem(runID uint64, rank int, kbID, reason string, at time.Time) model.KBLoadItem {
	return model.KBLoadItem{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, LoadSeq: 0, RankNo: rank,
		QuestionType: "protect", KBID: kbID, KBVersion: "v1", PickReason: reason, Strategy: model.LoaderStrategyV1, CreatedAt: at}
}

// P2 L5：kb_load_item 整批替换（resume 换策略重跑只留最后一次）、读回、线上加载精度 SQL、逐农场保留期清理。
func TestKBLoadItemRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.KBLoadItemRepo{DB: slot.DB}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}

	// 1. 整批替换：中断前按 v0 写了 5 行，resume 后按 v1 重跑写 4 行 → 只剩 v1 的 4 行；同 run 其它 load_seq 不动。
	run := itest.InsertRun(t, slot.DB, t0)
	var stale []model.KBLoadItem
	for i, kb := range []string{"rice/pest/planthopper", "rice/pest/stem-borer", "common/safety/phi", "rice/pest/leaf-roller", "common/safety/ppe"} {
		it := kbItem(run, i, kb, model.PickReasonRanked, t0)
		it.Strategy = model.LoaderStrategyV0
		stale = append(stale, it)
	}
	if err := repo.ReplaceKBLoadItems(ctx, scope, stale); err != nil {
		t.Fatal(err)
	}
	otherSeq := kbItem(run, 0, "common/safety/phi", model.PickReasonCommon, t0)
	otherSeq.LoadSeq = 1
	if err := repo.ReplaceKBLoadItems(ctx, scope, []model.KBLoadItem{otherSeq}); err != nil {
		t.Fatal(err)
	}
	items := []model.KBLoadItem{
		kbItem(run, 0, "common/safety/phi", model.PickReasonMustInclude, t0),
		kbItem(run, 1, "rice/pest/planthopper", model.PickReasonRanked, t0),
		kbItem(run, 2, "rice/pest/stem-borer", model.PickReasonRanked, t0),
		kbItem(run, 3, "common/safety/mixing", model.PickReasonCommon, t0),
	}
	if err := repo.ReplaceKBLoadItems(ctx, scope, items); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := repo.ListKBLoadItems(ctx, scope, run)
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(items), otherSeq)
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %d (v0 rows of seq 0 replaced, seq 1 kept)", got, len(want))
	}
	for i, g := range got {
		w := want[i]
		w.ID = g.ID
		if g.ID == 0 || !g.CreatedAt.Equal(w.CreatedAt) {
			t.Fatalf("row %d = %+v", i, g)
		}
		g.CreatedAt = w.CreatedAt
		if g != w {
			t.Fatalf("row %d = %+v, want %+v", i, g, w)
		}
	}
	bad := kbItem(run, 9, "x/y/z", "BOOSTED", t0)
	if err := repo.ReplaceKBLoadItems(ctx, scope, []model.KBLoadItem{bad}); err == nil {
		t.Fatal("unknown pick_reason accepted")
	}
	other := kbItem(run, 9, "x/y/z", model.PickReasonRanked, t0)
	other.FarmID = 2
	if err := repo.ReplaceKBLoadItems(ctx, scope, []model.KBLoadItem{other}); err == nil {
		t.Fatal("cross-farm item accepted")
	}
	mixed := kbItem(run, 1, "x/y/z", model.PickReasonRanked, t0)
	mixed.LoadSeq = 2
	if err := repo.ReplaceKBLoadItems(ctx, scope, []model.KBLoadItem{kbItem(run, 0, "x/y/z", model.PickReasonRanked, t0), mixed}); err == nil {
		t.Fatal("batch mixing load_seq accepted")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM kb_load_item WHERE tenant_id=? AND farm_id=? AND run_id=?`,
		itest.TenantID, itest.FarmID, run); n != int64(len(want)) {
		t.Fatalf("rejected batches changed rows: %d, want %d", n, len(want))
	}

	// 2. 线上加载精度：RANKED 两条中 planthopper 被本 run 建议卡引用 → 0.5；MUST_INCLUDE / COMMON 不计。
	l6Exec(t, slot.DB, `INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type,
  outcome, conclusion, reasons, next_action, schema_ver, created_at)
  VALUES (?, ?, 1, ?, 'rice', 'tillering', 'pest', 'observe', 'ADVICE', 'c', '[]', 'NONE', 1, ?)`,
		itest.TenantID, itest.FarmID, run, t0)
	adviceID := uint64(itest.Count(t, slot.DB, `SELECT id FROM advice_card WHERE tenant_id=? AND farm_id=? AND run_id=?`,
		itest.TenantID, itest.FarmID, run))
	for i, kb := range []string{"rice/pest/planthopper", "common/safety/phi"} {
		l6Exec(t, slot.DB, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, ?, 'conclusion', 'kb', ?, 'v1')`, itest.TenantID, itest.FarmID, adviceID, i, kb)
	}
	p, ok, err := repo.OnlineLoadPrecision(ctx, scope, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil || !ok || p != 0.5 {
		t.Fatalf("precision = %v, %v, %v; want 0.5", p, ok, err)
	}
	if _, ok, err := repo.OnlineLoadPrecision(ctx, scope, t0.Add(time.Hour), t0.Add(2*time.Hour)); err != nil || ok {
		t.Fatalf("empty window: ok=%v err=%v; want ok=false", ok, err)
	}

	// 3. 保留期清理：只删早于截止时间的行，单次至多 5000 行，逐农场。
	old := t0.AddDate(0, 0, -200)
	oldRun := itest.InsertRun(t, slot.DB, old)
	var olds []model.KBLoadItem
	for i := range 5003 {
		olds = append(olds, kbItem(oldRun, i, "rice/pest/planthopper", model.PickReasonRanked, old))
	}
	if err := repo.ReplaceKBLoadItems(ctx, scope, olds); err != nil {
		t.Fatal(err)
	}
	cutoff := t0.AddDate(0, 0, -180)
	farms, err := repo.KBLoadFarms(ctx, repository.NewSystemScope("kb_load_retention"), cutoff)
	if err != nil || len(farms) != 1 || farms[0].TenantID != itest.TenantID || farms[0].FarmID != itest.FarmID {
		t.Fatalf("farms = %v, %v", farms, err)
	}
	// 列农场走 idx_scope_time 松散索引扫描（与 KBLoadFarms 同一 SQL）：每组只读首行，不全量扫明细。
	// 新建表的索引统计为空，优化器会选全索引扫描；先 ANALYZE 让统计反映 5000+ 行 / 1 组的分布（线上表统计自动更新）。
	if err := slot.DB.WithContext(ctx).Exec(`ANALYZE TABLE kb_load_item`).Error; err != nil {
		t.Fatal(err)
	}
	var plan []struct {
		Key   *string `gorm:"column:key"`
		Extra *string `gorm:"column:Extra"`
	}
	if err := slot.DB.WithContext(ctx).Raw(`EXPLAIN SELECT tenant_id, farm_id FROM kb_load_item GROUP BY tenant_id, farm_id HAVING MIN(created_at) < ?
 ORDER BY tenant_id, farm_id`, cutoff).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Key == nil || *plan[0].Key != "idx_scope_time" || plan[0].Extra == nil ||
		!strings.Contains(*plan[0].Extra, "Using index for group-by") {
		for _, p := range plan {
			t.Logf("plan row: key=%v extra=%v", deref(p.Key), deref(p.Extra))
		}
		t.Fatal("KBLoadFarms: want loose index scan on idx_scope_time")
	}
	for _, want := range []int64{repository.KBLoadPurgeBatch, 3, 0} {
		n, err := repo.PurgeKBLoadItems(ctx, scope, cutoff)
		if err != nil || n != want {
			t.Fatalf("purge = %d, %v; want %d", n, err, want)
		}
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM kb_load_item WHERE tenant_id=? AND farm_id=?`, itest.TenantID, itest.FarmID); n != int64(len(want)) {
		t.Fatalf("rows after purge = %d, want %d (recent rows kept)", n, len(want))
	}
	if _, err := repo.PurgeKBLoadItems(ctx, model.Scope{}, cutoff); err == nil {
		t.Fatal("empty scope accepted")
	}
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
