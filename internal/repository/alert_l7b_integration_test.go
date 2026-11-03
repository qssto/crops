//go:build integration

package repository_test

import (
	"slices"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// l7bAdvice 插一张建议卡（outcome 可为 ADVICE / REFUSED），返回 id。
func l7bAdvice(t *testing.T, db *repository.DB, runID uint64, outcome string, at time.Time) uint64 {
	t.Helper()
	l6Exec(t, db, `INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type,
  outcome, conclusion, reasons, next_action, schema_ver, created_at)
  VALUES (?, ?, 1, ?, 'rice', 'tillering', 'pest', 'observe', ?, 'c', '[]', 'NONE', 1, ?)`,
		itest.TenantID, itest.FarmID, runID, outcome, at)
	return uint64(itest.Count(t, db, `SELECT id FROM advice_card WHERE run_id=?`, runID))
}

// P1 L7-B 红线（无引用建议发现）：窗口内非 REFUSED 且无引用的卡命中；REFUSED、有引用、窗口外都不命中（应失败用例）。
// 告警审计 ALERT.advice_no_citation 写入后 AlertedRuns 返回该 run（每张建议只告警一次的去重依据）。
func TestAlertRepo_AdviceWithoutCitation_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7b")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.AlertRepo{DB: slot.DB}
	sys := repository.NewSystemScope("advice_no_citation")

	hitRun := itest.InsertRun(t, slot.DB, t0)
	hit := l7bAdvice(t, slot.DB, hitRun, "ADVICE", t0.Add(-time.Minute))
	l7bAdvice(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "REFUSED", t0.Add(-time.Minute)) // 拒答不算
	cited := l7bAdvice(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "ADVICE", t0.Add(-time.Minute))
	l6Exec(t, slot.DB, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, 0, 'conclusion', 'kb', 'kb-1', 'v1')`, itest.TenantID, itest.FarmID, cited)
	l7bAdvice(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "ADVICE", t0.Add(-10*time.Minute)) // 窗口外

	rows, err := repo.AdviceWithoutCitation(ctx, sys, t0.Add(-2*time.Minute), t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != hit || rows[0].RunID != hitRun || rows[0].TenantID != itest.TenantID ||
		rows[0].FarmID != itest.FarmID {
		t.Fatalf("rows = %+v, want only advice %d (run %d)", rows, hit, hitRun)
	}

	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	action := model.AlertAdviceNoCitation.AuditAction()
	done, err := repo.AlertedRuns(ctx, scope, action, []uint64{hitRun}, t0.Add(-2*time.Minute))
	if err != nil || done[hitRun] {
		t.Fatalf("before alert: done = %v err = %v", done, err)
	}
	run := hitRun
	if err := (repository.AuditRepo{DB: slot.DB}).Append(ctx, repository.NewAuditRecord(scope, &run,
		model.Actor{Type: model.ActorSystem}, action, "run:1", []byte(`{"alert":"advice_no_citation"}`),
		model.AuditResultFailed, t0)); err != nil {
		t.Fatal(err)
	}
	done, err = repo.AlertedRuns(ctx, scope, action, []uint64{hitRun}, t0.Add(-2*time.Minute))
	if err != nil || !done[hitRun] {
		t.Fatalf("after alert: done = %v err = %v", done, err)
	}

	list, err := repo.ListAlerts(ctx, scope, []string{string(action)}, t0.Add(-time.Hour), t0.Add(time.Second), repository.AlertCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Action != string(action) || list[0].RunID == nil || *list[0].RunID != hitRun {
		t.Fatalf("alert list = %+v", list)
	}
	other := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}
	if list, err := repo.ListAlerts(ctx, other, []string{string(action)}, t0.Add(-time.Hour), t0.Add(time.Second),
		repository.AlertCursor{}, 10); err != nil || len(list) != 0 {
		t.Fatalf("other farm list = %+v err = %v, want empty", list, err)
	}
}

// 审查修复 P2：告警列表键集分页。同一 created_at 的两行按 id 倒序切页，游标页不重不漏，最后一页之后为空。
func TestAlertRepo_ListAlertsCursor_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7b")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.AlertRepo{DB: slot.DB}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	action := model.AlertModelActuate.AuditAction()
	for _, at := range []time.Time{t0.Add(-2 * time.Minute), t0.Add(-time.Minute), t0.Add(-time.Minute)} {
		if err := (repository.AuditRepo{DB: slot.DB}).Append(ctx, repository.NewAuditRecord(scope, nil,
			model.Actor{Type: model.ActorSystem}, action, "", []byte(`{"alert":"model_actuate"}`),
			model.AuditResultFailed, at)); err != nil {
			t.Fatal(err)
		}
	}
	from, to := t0.Add(-time.Hour), t0.Add(time.Second)
	var seen []uint64
	cur := repository.AlertCursor{}
	for page := 0; page < 3; page++ {
		rows, err := repo.ListAlerts(ctx, scope, []string{string(action)}, from, to, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen = append(seen, r.ID)
		}
		last := rows[len(rows)-1]
		cur = repository.AlertCursor{At: last.CreatedAt, ID: last.ID}
	}
	if len(seen) != 3 || seen[0] < seen[1] {
		t.Fatalf("paged ids = %v, want 3 distinct rows, newest first (id desc within same created_at)", seen)
	}
	if seen[0] == seen[1] || seen[1] == seen[2] || seen[0] == seen[2] {
		t.Fatalf("paged ids repeat: %v", seen)
	}
}

// 指标预置：KnownFarms 返回 farm_profile 登记的农场（去重升序）。
func TestAlertRepo_KnownFarms_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7b")
	slot := e.Fresh(t, t0)
	ids, err := repository.AlertRepo{DB: slot.DB}.KnownFarms(t.Context(), repository.NewSystemScope("metrics_preseed"))
	if err != nil {
		t.Fatal(err)
	}
	want := itest.Count(t, slot.DB, `SELECT COUNT(DISTINCT farm_id) FROM farm_profile`)
	if int64(len(ids)) != want || !slices.IsSorted(ids) || !slices.Contains(ids, itest.FarmID) {
		t.Fatalf("known farms = %v, want %d sorted ids including %d", ids, want, itest.FarmID)
	}
}

// 审批积压：只数 OPEN 的 ORDER 票，按农场分组。
func TestAlertRepo_OpenOrderTickets_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7b")
	slot := e.Fresh(t, t0)
	for _, st := range []string{"OPEN", "OPEN", "APPROVED", "EXPIRED"} {
		runID := itest.InsertRun(t, slot.DB, t0)
		l6Ticket(t, slot.DB, runID, itest.InsertOrder(t, slot.DB, runID, "PENDING_APPROVAL", t0), st, t0)
	}
	rows, err := repository.AlertRepo{DB: slot.DB}.OpenOrderTickets(t.Context(), repository.NewSystemScope("approval_backlog"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].FarmID != itest.FarmID || rows[0].N != 2 {
		t.Fatalf("rows = %+v, want farm %d n=2", rows, itest.FarmID)
	}
}
