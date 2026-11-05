//go:build integration

package repository_test

import (
	"encoding/json"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// shadowDiff 是测试用影子行（本农场、指定 run / 项 / 是否一致）。
func shadowDiff(runID uint64, item model.RuleOffloadItem, agreed bool, at time.Time) model.RuleShadowDiff {
	d := model.RuleShadowDiff{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, Item: item, Agreed: agreed,
		RuleOutput: json.RawMessage(`{"open":true}`), ModelOutput: json.RawMessage(`{}`), RuleVer: "op-window-v1", CreatedAt: at}
	if !agreed {
		d.DiffKind = model.DiffWindowOutside
		d.ModelOutput = json.RawMessage(`{"snippet":"下周统一防治"}`)
	}
	return d
}

// P2 L4 验收 4：影子行与建议卡同一事务写入（出卡 advice_id=卡片 id、拒答 0），重放幂等；影子行非法时整笔回滚。
func TestAdviceRepo_SaveAdviceWithShadow_Integration(t *testing.T) {
	e := itest.Setup(t, "repol4")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewAdviceRepo(slot.DB, slot.Clock)

	runID := itest.InsertRun(t, slot.DB, t0)
	card := model.AdviceCard{RunID: runID, FieldID: 1, Crop: "水稻", Category: model.CategoryProtect,
		AdviceType: model.AdviceTypeOperation, Outcome: model.OutcomeAdvice, Conclusion: "今天傍晚施药",
		Reasons: []string{"虫量超阈值"}, NextAction: model.NextCreateOrder}
	diffs := []model.RuleShadowDiff{shadowDiff(runID, model.RuleItemOperationWindow, true, t0),
		shadowDiff(runID, model.RuleItemTankMix, true, t0)}
	id, err := repo.SaveAdviceWithShadow(ctx, l4Scope, card, diffs)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := repo.SaveAdviceWithShadow(ctx, l4Scope, card, diffs); err != nil || again != id {
		t.Fatalf("replay = %d, %v; want %d", again, err, id)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM rule_shadow_diff WHERE run_id=? AND advice_id=?", runID, id); n != 2 {
		t.Fatalf("shadow rows for advice = %d, want 2", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM episodic_memory WHERE kind='ADVICE' AND ref_id=?", id); n != 1 {
		t.Fatalf("memory rows = %d, want 1 (same transaction as SaveAdvice)", n)
	}

	refusedRun := itest.InsertRun(t, slot.DB, t0)
	refused := model.AdviceCard{RunID: refusedRun, FieldID: 1, Crop: "水稻", Category: model.CategoryProtect,
		Outcome: model.OutcomeRefused, RefuseReason: "VALIDATION_FAILED", NextAction: model.NextNone}
	if _, err := repo.SaveAdviceWithShadow(ctx, l4Scope, refused,
		[]model.RuleShadowDiff{shadowDiff(refusedRun, model.RuleItemOperationWindow, false, t0)}); err != nil {
		t.Fatal(err)
	}
	var row struct {
		AdviceID uint64
		DiffKind *string
		Model    []byte
	}
	if err := slot.DB.WithContext(ctx).Raw("SELECT advice_id, diff_kind, model_output AS model FROM rule_shadow_diff WHERE run_id=?",
		refusedRun).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AdviceID != 0 || row.DiffKind == nil || *row.DiffKind != string(model.DiffWindowOutside) ||
		!jsonEqual(t, row.Model, `{"snippet":"下周统一防治"}`) {
		t.Fatalf("refused shadow row = %+v (%s), want advice_id 0, WINDOW_OUTSIDE", row, row.Model)
	}

	// 影子行非法（缺 created_at）：建议卡也不落（同成同败）。
	badRun := itest.InsertRun(t, slot.DB, t0)
	bad := card
	bad.RunID = badRun
	if _, err := repo.SaveAdviceWithShadow(ctx, l4Scope, bad,
		[]model.RuleShadowDiff{shadowDiff(badRun, model.RuleItemTankMix, true, time.Time{})}); err == nil {
		t.Fatal("want error for zero created_at")
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM advice_card WHERE run_id=?", badRun); n != 0 {
		t.Fatalf("advice_card rows after rollback = %d, want 0", n)
	}
	other := shadowDiff(badRun, model.RuleItemTankMix, true, t0)
	other.FarmID = 2
	if _, err := repo.SaveAdviceWithShadow(ctx, l4Scope, bad, []model.RuleShadowDiff{other}); err == nil {
		t.Fatal("want scope mismatch for other farm's shadow row")
	}
}

// P2 L4 §6.5：差异率按农场、since 起（含）分项；清理逐农场逐项 LIMIT 5000，只删本农场该项早于 before 的行。
func TestRuleShadowDiffRepo_RatesAndPurge_Integration(t *testing.T) {
	e := itest.Setup(t, "repol4")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.RuleShadowDiffRepo{DB: slot.DB}
	sys := repository.NewSystemScope("rule-shadow-purge")

	// 造数：本农场 operation_window 5003 行旧数据（1 行冲突）、tank_mix 2 行旧数据；farm 2 有 1 行旧数据；本农场 2 行新数据；
	// farm 3 只有 1 行新数据。
	old, fresh := t0.Add(-200*24*time.Hour), t0
	l6Exec(t, slot.DB, `INSERT INTO rule_shadow_diff (tenant_id, farm_id, run_id, advice_id, item, agreed, rule_output, model_output, rule_ver, created_at)
SELECT ?, ?, 100000 + n, 0, 'operation_window', n <> 0, '{}', '{}', 'op-window-v1', ? FROM (
  SELECT a.d * 1000 + b.d * 100 + c.d * 10 + e.d AS n FROM
  (SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) a,
  (SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) b,
  (SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) c,
  (SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) e
) seq WHERE n < 5003`, itest.TenantID, itest.FarmID, old)
	ins := `INSERT INTO rule_shadow_diff (tenant_id, farm_id, run_id, advice_id, item, agreed, rule_output, model_output, rule_ver, created_at)
VALUES (?, ?, ?, 0, ?, ?, '{}', '{}', 'v', ?)`
	l6Exec(t, slot.DB, ins, itest.TenantID, itest.FarmID, 1, "tank_mix", true, old)
	l6Exec(t, slot.DB, ins, itest.TenantID, itest.FarmID, 2, "tank_mix", false, old)
	l6Exec(t, slot.DB, ins, itest.TenantID, 2, 3, "operation_window", true, old)
	l6Exec(t, slot.DB, ins, itest.TenantID, itest.FarmID, 4, "operation_window", false, fresh)
	l6Exec(t, slot.DB, ins, itest.TenantID, itest.FarmID, 5, "pest_priority", true, fresh)
	l6Exec(t, slot.DB, ins, itest.TenantID, 3, 6, "tank_mix", true, fresh) // farm 3 只有新数据：不在清理列表

	rates, err := repo.DiffRates(ctx, l4Scope, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 2 || rates[model.RuleItemOperationWindow] != 1 || rates[model.RuleItemPestPriority] != 0 {
		t.Fatalf("rates since fresh = %v, want operation_window 1, pest_priority 0", rates)
	}
	if rates, err = repo.DiffRates(ctx, l4Scope, old); err != nil || rates[model.RuleItemTankMix] != 0.5 {
		t.Fatalf("rates since old = %v, %v; want tank_mix 0.5", rates, err)
	}

	before := t0.Add(-180 * 24 * time.Hour)
	farms, err := repo.ShadowFarms(ctx, sys, before)
	if err != nil || len(farms) != 2 || farms[0] != (repository.FarmRef{TenantID: itest.TenantID, FarmID: itest.FarmID}) {
		t.Fatalf("farms = %v, %v; want farm 1 and 2 (farm 3 has fresh rows only)", farms, err)
	}
	farm := farms[0]
	for _, want := range []int64{repository.ShadowPurgeBatch, 3, 0} {
		if n, err := repo.PurgeBatch(ctx, sys, farm, model.RuleItemOperationWindow, before); err != nil || n != want {
			t.Fatalf("purge batch = %d, %v; want %d", n, err, want)
		}
	}
	for _, c := range []struct {
		name string
		q    string
		want int64
	}{
		{"farm 1 operation_window keeps fresh row", "SELECT COUNT(*) FROM rule_shadow_diff WHERE farm_id=1 AND item='operation_window'", 1},
		{"farm 1 tank_mix untouched", "SELECT COUNT(*) FROM rule_shadow_diff WHERE farm_id=1 AND item='tank_mix'", 2},
		{"farm 2 untouched", "SELECT COUNT(*) FROM rule_shadow_diff WHERE farm_id=2", 1},
	} {
		if n := itest.Count(t, slot.DB, c.q); n != c.want {
			t.Errorf("%s: %d, want %d", c.name, n, c.want)
		}
	}
	if _, err := repo.PurgeBatch(ctx, repository.SystemScope{}, farm, model.RuleItemTankMix, before); err == nil {
		t.Fatal("zero SystemScope must be rejected")
	}
	if _, err := repo.DiffRates(ctx, model.Scope{}, old); err == nil {
		t.Fatal("zero Scope must be rejected")
	}
}
