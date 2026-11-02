//go:build integration

package repository_test

import (
	"errors"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// TestSetDispatchDeadlineTx_Integration：退回重审在同一时刻写出相同 deadline（MySQL 影响行数 0）仍成功；
// 单不在 APPROVED 时 ErrConcurrentUpdate。回归 eval 75 rice-dispatch-0001 重审 concurrent update。
func TestSetDispatchDeadlineTx_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	deadline := t0.Add(2 * time.Hour)
	set := func(orderID uint64) error {
		ref := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID}
		return repo.InTx(ctx, func(tx *repository.DB) error { return repo.SetDispatchDeadlineTx(ctx, tx, ref, deadline, t0) })
	}
	approved := itest.InsertOrder(t, slot.DB, itest.InsertRun(t, slot.DB, t0), string(model.OrderApproved), t0)
	if err := set(approved); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := set(approved); err != nil {
		t.Fatalf("same value again: %v", err)
	}
	pending := itest.InsertOrder(t, slot.DB, itest.InsertRun(t, slot.DB, t0), string(model.OrderPendingApproval), t0)
	if err := set(pending); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("not approved err = %v, want ErrConcurrentUpdate", err)
	}
}
