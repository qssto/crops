//go:build integration

package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// insertAttempt 插入一条 dispatch_attempt（status / receipt_due_at 由用例指定），返回 id。
func insertAttempt(t *testing.T, db *repository.DB, orderID uint64, attempt int, status string, due time.Time) uint64 {
	t.Helper()
	if err := db.WithContext(t.Context()).Exec(`INSERT INTO dispatch_attempt (tenant_id, farm_id, order_id, ticket_id, attempt,
  device_type, device_id, idem_key, device_job_id, mode, status, command_raw_id, flag_read_at, sent_at, receipt_due_at, updated_at)
  VALUES (?, ?, ?, 1, ?, 'DRONE', 'dev-uav-01', ?, ?, 'REAL', ?, 1, ?, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, orderID, attempt, "it:"+status+":"+due.String(), "job-"+status+"-"+due.String(), status,
		t0, t0, due, t0).Error; err != nil {
		t.Fatal(err)
	}
	return uint64(itest.Count(t, db, `SELECT id FROM dispatch_attempt WHERE order_id = ? AND attempt = ?`, orderID, attempt))
}

func receiptRow(orderID uint64, receiptID string, at time.Time) repository.ReceiptRow {
	area := 8.0
	return repository.ReceiptRow{TenantID: itest.TenantID, FarmID: itest.FarmID, ReceiptID: receiptID, OrderID: orderID, Attempt: 1,
		DeviceJobID: "job-1", NormStatus: model.AttemptDone, Effect: model.OutcomeApplied, DoneAreaMu: &area, DeviceTS: at,
		Source: model.SourceWebhook, CreatedAt: at}
}

// §6.5 验收 2「回执重放」：同一 receipt_id 第二次 INSERT IGNORE 影响 0 行（uk_receipt），表里仍只有 1 行；
// uk_receipt 含 tenant / farm，别的农场同 receipt_id 不冲突。
func TestDeviceReceipt_ReplayUK_Integration(t *testing.T) {
	e := itest.Setup(t, "receipt")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	tx := repository.NewDecisionRepo(slot.DB, slot.Clock)
	repo := repository.DeviceReceiptRepo{DB: slot.DB}
	orderID := itest.InsertOrder(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "DISPATCHED", t0)
	insert := func(r repository.ReceiptRow) bool {
		t.Helper()
		var inserted bool
		if err := tx.InTx(ctx, func(db *gorm.DB) error {
			var err error
			inserted, err = repo.InsertReceiptTx(ctx, db, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return inserted
	}
	row := receiptRow(orderID, "rcpt-replay-1", t0)
	for i, want := range []bool{true, false, false} {
		if got := insert(row); got != want {
			t.Fatalf("insert #%d = %t, want %t", i, got, want)
		}
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM device_receipt WHERE receipt_id = ?`, row.ReceiptID); n != 1 {
		t.Fatalf("device_receipt rows after replay = %d, want 1", n)
	}
	other := row
	other.FarmID = itest.FarmID + 1
	if !insert(other) {
		t.Fatal("same receipt_id on another farm treated as replay")
	}
	got, err := repo.ReceiptsByOrder(ctx, model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID})
	if err != nil || len(got) != 1 || got[0].DoneAreaMu == nil || *got[0].DoneAreaMu != 8 {
		t.Fatalf("ReceiptsByOrder = %+v, %v", got, err)
	}
}

// 回执入库与 Transition 同一事务：事务内任一步失败，device_receipt 与 work_order_transition 都不留行，
// 重投同一 receipt_id 仍能入库（不被误判为重放）。
func TestDeviceReceipt_TxRollback_Integration(t *testing.T) {
	e := itest.Setup(t, "receipt")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	tx := repository.NewDecisionRepo(slot.DB, slot.Clock)
	repo := repository.DeviceReceiptRepo{DB: slot.DB}
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "DISPATCHED", t0)
	ref := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID}
	row := receiptRow(orderID, "rcpt-rollback-1", t0)
	boom := errors.New("boom after transition")
	err := tx.InTx(ctx, func(db *gorm.DB) error {
		if _, err := repo.InsertReceiptTx(ctx, db, row); err != nil {
			return err
		}
		if err := (repository.WorkOrderRepo{}).InsertTransition(ctx, db, model.OrderTransition{Ref: ref, RunID: runID,
			From: model.OrderDispatched, To: model.OrderDone, Actor: model.Actor{Type: model.ActorDevice},
			CreatedAt: t0}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx err = %v, want boom", err)
	}
	for q, want := range map[string]int64{
		`SELECT COUNT(*) FROM device_receipt WHERE order_id = ?`:                                 0,
		`SELECT COUNT(*) FROM work_order_transition WHERE order_id = ? AND to_status = 'DONE'`:   0,
		`SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'DISPATCHED' AND row_ver = 0`: 1,
	} {
		if n := itest.Count(t, slot.DB, q, orderID); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	if err := tx.InTx(ctx, func(db *gorm.DB) error {
		ok, err := repo.InsertReceiptTx(ctx, db, row)
		if err == nil && !ok {
			t.Error("redelivery after rollback treated as replay")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 回执超时扫描互斥：lock:scan:receipt_due 同时只有一个持有者（并发取锁恰好 1 个成功）；
// DueAttempts 只取在途且过期的尝试，须持 SystemScope。
func TestReceiptDueScan_Integration(t *testing.T) {
	e := itest.Setup(t, "receipt")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	locker := repository.NewLocker(e.Redis)
	key := slot.Keys.LockScan(rediskey.ScanReceiptDue)
	const workers = 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  []*repository.Lock
		held int
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk, err := locker.Acquire(ctx, key, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won = append(won, lk)
			case errors.Is(err, repository.ErrLockHeld):
				held++
			default:
				t.Errorf("acquire: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(won) != 1 || held != workers-1 {
		t.Fatalf("scan lock winners = %d, held = %d; want 1 / %d", len(won), held, workers-1)
	}
	if err := won[0].Release(context.WithoutCancel(ctx)); err != nil {
		t.Fatal(err)
	}

	runID := itest.InsertRun(t, slot.DB, t0)
	dueOrder := itest.InsertOrder(t, slot.DB, runID, "DISPATCHED", t0)
	dueID := insertAttempt(t, slot.DB, dueOrder, 1, "EXECUTING", t0.Add(-time.Minute))
	insertAttempt(t, slot.DB, itest.InsertOrder(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "DISPATCHED", t0), 1, "SENT",
		t0.Add(time.Hour))
	insertAttempt(t, slot.DB, itest.InsertOrder(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "DISPATCHED", t0), 1, "NO_RECEIPT",
		t0.Add(-time.Hour))
	repo := repository.DeviceReceiptRepo{DB: slot.DB}
	due, err := repo.DueAttempts(ctx, repository.NewSystemScope(rediskey.ScanReceiptDue), t0)
	if err != nil || len(due) != 1 || due[0].ID != dueID || due[0].OrderID != dueOrder {
		t.Fatalf("DueAttempts = %+v, %v; want only attempt %d", due, err, dueID)
	}
	if _, err := repo.DueAttempts(ctx, repository.SystemScope{}, t0); err == nil {
		t.Fatal("DueAttempts without SystemScope succeeded")
	}
}
