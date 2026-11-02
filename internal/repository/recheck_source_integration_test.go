//go:build integration

package repository_test

import (
	"errors"
	"testing"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// TestRecheckSource_Integration：下发前复检从 RUN_STARTED 读回原问题（JSON 列经 GORM 扫描）；他场读不到。
// 回归 eval 72：Scan(&[]byte) 被当成多行 uint8，复检每次报错、order.dispatch 一直 PENDING。
func TestRecheckSource_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	ev := model.RunEvent{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, Type: model.EventRunStarted,
		Actor: model.ActorUser, Payload: []byte(`{"field_id": 1, "question": "帮我安排无人机明天给3号田打稻飞虱的药"}`), CreatedAt: t0}
	if err := slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
		_, err := repository.EventRepo{}.AppendTx(t.Context(), tx, ev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	repo := repository.DispatchRepo{DB: slot.DB}
	got, err := repo.RecheckSource(t.Context(), model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != 1001 || got.FieldID != 1 || got.Question != "帮我安排无人机明天给3号田打稻飞虱的药" {
		t.Fatalf("RecheckSource = %+v", got)
	}
	_, err = repo.RecheckSource(t.Context(), model.Scope{TenantID: itest.TenantID, FarmID: 2}, runID)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other farm err = %v, want ErrNotFound", err)
	}
}
