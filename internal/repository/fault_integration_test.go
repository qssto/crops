//go:build integration

package repository_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L6 §6.7 技术实现第 5 条：DEGRADED 事件与 audit_log(DEGRADED, target fault:{fault}, actor system) 同事务写入；
// 事务回滚时两者都不留。
func TestDegradedEventAuditSameTx_Integration(t *testing.T) {
	e := itest.Setup(t, "repofault")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	repo := repository.EventRepo{Pub: repository.RedisPublisher{Client: e.Redis}, Keys: slot.Keys}
	stale := 3600
	payload, err := json.Marshal(model.DegradedPayload{Fault: model.FaultMCPTimeout, Action: model.DegradeCacheFallback,
		StalenessSec: &stale, TextKey: "fault.mcp_timeout.refused", Tool: "weather.forecast"})
	if err != nil {
		t.Fatal(err)
	}
	ev := model.RunEvent{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, StepID: "enrich",
		Type: model.EventDegraded, Actor: model.ActorSystem, Payload: payload, CreatedAt: t0}
	tx := repository.Transactor{DB: slot.DB}

	errAbort := errors.New("abort")
	if err := tx.InTx(t.Context(), func(db *repository.DB) error {
		if _, err := repo.AppendTx(t.Context(), db, ev); err != nil {
			return err
		}
		return errAbort
	}); !errors.Is(err, errAbort) {
		t.Fatalf("tx err = %v, want abort", err)
	}
	const (
		eventQ = `SELECT COUNT(*) FROM run_event WHERE run_id=? AND event_type='DEGRADED' AND actor='system'
  AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.action'))='CACHE_FALLBACK' AND JSON_EXTRACT(payload, '$.staleness_sec')=3600`
		auditQ = `SELECT COUNT(*) FROM audit_log WHERE run_id=? AND action='DEGRADED' AND target='fault:MCP_TIMEOUT'
  AND actor_type='system' AND result='OK'`
	)
	if n := itest.Count(t, slot.DB, eventQ, runID) + itest.Count(t, slot.DB, auditQ, runID); n != 0 {
		t.Fatalf("rolled-back DEGRADED rows = %d, want 0", n)
	}

	if err := tx.InTx(t.Context(), func(db *repository.DB) error {
		_, err := repo.AppendTx(t.Context(), db, ev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, eventQ, runID); n != 1 {
		t.Errorf("DEGRADED run_event rows = %d, want 1", n)
	}
	if n := itest.Count(t, slot.DB, auditQ, runID); n != 1 {
		t.Errorf("DEGRADED audit rows = %d, want 1", n)
	}
}

// §6.7 设备离线推后 SQL：next_run_at = now+1 分钟、last_error='DEVICE_OFFLINE'、attempts 不变、row_ver+1；
// 旧 row_ver（租约已丢）或非 PENDING 影响 0 行返回 false。离线通知按 dedup_key 同票只入 outbox 一次。
func TestDeviceOfflineDeferAndNotifyDedup_Integration(t *testing.T) {
	e := itest.Setup(t, "repofault")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	const dedup = "order.dispatch:9:7:1"
	l6Exec(t, slot.DB, `INSERT INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, attempts,
  next_run_at, created_at, updated_at) VALUES (?, ?, 'order.dispatch', 9, ?, '{"order_id":9}', 'PENDING', 2, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, dedup, t0, t0, t0)
	id := uint64(itest.Count(t, slot.DB, `SELECT id FROM outbox WHERE dedup_key=?`, dedup))
	rowVer := int(itest.Count(t, slot.DB, `SELECT row_ver FROM outbox WHERE id=?`, id))
	repo := repository.OutboxRepo{DB: slot.DB}
	sys := repository.NewSystemScope("outbox")

	if ok, err := repo.DeferDeviceOffline(ctx, sys, id, rowVer+1, t0); err != nil || ok {
		t.Fatalf("stale row_ver defer = %t, %v; want false", ok, err)
	}
	if ok, err := repo.DeferDeviceOffline(ctx, sys, id, rowVer, t0); err != nil || !ok {
		t.Fatalf("defer = %t, %v; want true", ok, err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE id=? AND status='PENDING' AND attempts=2
  AND last_error='DEVICE_OFFLINE' AND next_run_at=? AND row_ver=?`, id, t0.Add(time.Minute), rowVer+1); n != 1 {
		t.Fatal("outbox not deferred 1 minute with last_error=DEVICE_OFFLINE and attempts unchanged")
	}
	l6Exec(t, slot.DB, `UPDATE outbox SET status='DONE' WHERE id=?`, id)
	if ok, err := repo.DeferDeviceOffline(ctx, sys, id, rowVer+1, t0); err != nil || ok {
		t.Fatalf("DONE defer = %t, %v; want false", ok, err)
	}
	if _, err := repo.DeferDeviceOffline(ctx, repository.SystemScope{}, id, rowVer+1, t0); err == nil {
		t.Fatal("zero SystemScope accepted")
	}

	ref := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: 9}
	key := model.NotifyDedupKey(model.NotifyDeviceOffline, 9, 7)
	var inserted []bool
	for range 2 {
		if err := (repository.Transactor{DB: slot.DB}).InTx(ctx, func(db *repository.DB) error {
			ok, err := repository.NotifyOutboxRepo{}.InsertTx(ctx, db, ref, key, []byte(`{"kind":"DEVICE_OFFLINE"}`), t0)
			inserted = append(inserted, ok)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(inserted) != 2 || !inserted[0] || inserted[1] {
		t.Fatalf("notify inserted = %v, want [true false]", inserted)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE topic='notify.send' AND dedup_key=?`, key); n != 1 {
		t.Fatalf("DEVICE_OFFLINE notify rows = %d, want 1", n)
	}
}
