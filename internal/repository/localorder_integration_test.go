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

// P1-L3：LocalOrderRepo 定版 SQL：DRY_RUN 不计、排除本单、在途单不看时间、完成单按 since 边界（含）。
func TestLocalOrderRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	since := t0.AddDate(0, 0, -21)
	runID := itest.InsertRun(t, slot.DB, t0)
	type row struct {
		name, status, mode string
		field              uint64
		updated            time.Time
	}
	rows := []row{
		{name: "self", status: "DRAFT", mode: "REAL", field: 1, updated: t0},
		{name: "approved real", status: "APPROVED", mode: "REAL", field: 1, updated: t0.AddDate(0, -3, 0)},
		{name: "executing real", status: "EXECUTING", mode: "REAL", field: 1, updated: t0},
		{name: "done at boundary", status: "DONE", mode: "REAL", field: 1, updated: since},
		{name: "partially done in window", status: "PARTIALLY_DONE", mode: "REAL", field: 1, updated: since.Add(time.Hour)},
		{name: "done before window", status: "DONE", mode: "REAL", field: 1, updated: since.Add(-time.Second)},
		{name: "dispatched dry run", status: "DISPATCHED", mode: "DRY_RUN", field: 1, updated: t0},
		{name: "done dry run", status: "DONE", mode: "DRY_RUN", field: 1, updated: t0},
		{name: "rejected real", status: "REJECTED", mode: "REAL", field: 1, updated: t0},
		{name: "other field", status: "APPROVED", mode: "REAL", field: 2, updated: t0},
	}
	ids := map[string]uint64{}
	for i, r := range rows {
		res := slot.DB.WithContext(t.Context()).Exec(`INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id,
  run_id, work_type, device_type, payload, time_window_from, time_window_to, status, dispatch_mode, schema_ver,
  created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'SPRAY', 'uav', '{"product":"吡蚜酮"}', ?, ?, ?, ?, 1, ?, ?)`,
			itest.TenantID, itest.FarmID, r.field, 700000+i, runID, t0, t0.Add(time.Hour), r.status, r.mode, r.updated, r.updated)
		if res.Error != nil {
			t.Fatalf("insert %s: %v", r.name, res.Error)
		}
		var id uint64
		if err := slot.DB.WithContext(t.Context()).Raw(`SELECT id FROM work_order WHERE tenant_id=? AND farm_id=? AND advice_id=?`,
			itest.TenantID, itest.FarmID, 700000+i).Scan(&id).Error; err != nil {
			t.Fatal(err)
		}
		ids[r.name] = id
	}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	got, err := repository.LocalOrderRepo{DB: slot.DB}.RecentOrders(t.Context(), scope, 1, ids["self"], since)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []uint64
	for _, o := range got {
		gotIDs = append(gotIDs, o.OrderID)
		if o.WorkType != model.WorkSpray || string(o.Payload) == "" || o.UpdatedAt.IsZero() {
			t.Errorf("order %+v not fully scanned", o)
		}
	}
	want := []uint64{ids["approved real"], ids["executing real"], ids["done at boundary"], ids["partially done in window"]}
	slices.Sort(gotIDs)
	slices.Sort(want)
	if !slices.Equal(gotIDs, want) {
		t.Fatalf("recent orders = %v, want %v (ids %v)", gotIDs, want, ids)
	}

	if _, err := (repository.LocalOrderRepo{DB: slot.DB}).RecentOrders(t.Context(), model.Scope{}, 1, 0, since); err == nil {
		t.Fatal("empty scope accepted")
	}
}
