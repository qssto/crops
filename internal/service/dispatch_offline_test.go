package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// testOfflineTextKey 是夹具里的设备离线文案 key（与 policy/fault_matrix.yaml 一致）。
const testOfflineTextKey = "fault.device_offline.notify"

// memNotify 是 NotifyOutboxWriter 的桩：按 dedup_key 去重（与 uk_dedup 一致），err 非 nil 时写入失败。
type memNotify struct {
	rows  map[string][]byte
	calls int
	err   error
}

func (m *memNotify) InsertTx(_ context.Context, _ *repository.DB, ref model.OrderRef, dedupKey string, payload []byte,
	_ time.Time) (bool, error) {
	m.calls++
	if m.err != nil {
		return false, m.err
	}
	if ref.TenantID == 0 || ref.FarmID == 0 {
		return false, fmt.Errorf("notify: no scope")
	}
	if m.rows == nil {
		m.rows = map[string][]byte{}
	}
	if _, dup := m.rows[dedupKey]; dup {
		return false, nil
	}
	m.rows[dedupKey] = payload
	return true, nil
}

// degradedEvents 返回 DEGRADED 事件的 payload。
func (f *dispatchFixture) degradedEvents(t *testing.T) []model.DegradedPayload {
	t.Helper()
	var out []model.DegradedPayload
	for _, e := range f.events.events {
		if e.Type != model.EventDegraded {
			continue
		}
		if e.Actor != model.ActorSystem || e.RunID != 5 || e.TenantID != 1 || e.FarmID != 1 {
			t.Errorf("DEGRADED event = %+v", e)
		}
		var p model.DegradedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// 设备离线（红线 §6.7）：DRY_RUN / REAL 单都不调 Adapter、单状态不变，返回 ErrOutboxDeviceOffline；同事务写一条
// notify:DEVICE_OFFLINE:{order}:{ticket} 通知与 DEGRADED(DEVICE_OFFLINE, HOLD_AND_NOTIFY) 事件（run_id 取自 payload）。
// 同一条消息处理两次只通知一次、只有一条 DEGRADED（应失败：每次推后都发通知）；DEVICE_UNAVAILABLE 审计每次都写。
func TestDispatch_DeviceOfflineNotifiesOnce(t *testing.T) {
	for _, mode := range []model.DispatchMode{model.DispatchDryRun, model.DispatchReal} {
		t.Run(string(mode), func(t *testing.T) {
			f := newDispatchFixture(t, model.OrderApproved, false)
			f.store.order.DispatchMode = string(mode)
			f.flags.robot = true
			f.picker.err = fmt.Errorf("%w: all offline", ErrDeviceUnavailable)
			for i := range 2 {
				err := f.svc.Dispatch(context.Background(), realMsg(1))
				if !errors.Is(err, ErrOutboxDeviceOffline) {
					t.Fatalf("round %d err = %v, want ErrOutboxDeviceOffline", i, err)
				}
			}
			if f.dev.dryRuns != 0 || f.real.dispatches+f.real.archives != 0 || f.pre.calls != 0 ||
				f.store.order.Status != string(model.OrderApproved) || len(f.store.transitions) != 0 {
				t.Fatalf("dryRuns=%d dispatches=%d prechecks=%d status=%s transitions=%d", f.dev.dryRuns,
					f.real.dispatches, f.pre.calls, f.store.order.Status, len(f.store.transitions))
			}
			key := model.NotifyDedupKey(model.NotifyDeviceOffline, 3, 7)
			if f.notify.calls != 2 || len(f.notify.rows) != 1 || f.notify.rows[key] == nil {
				t.Fatalf("notify calls=%d rows=%v, want one %s", f.notify.calls, f.notify.rows, key)
			}
			var n deviceOfflineNotify
			if err := json.Unmarshal(f.notify.rows[key], &n); err != nil {
				t.Fatal(err)
			}
			if n.Kind != model.NotifyDeviceOffline || n.OrderID != 3 || n.TicketID != 7 || n.RunID != 5 ||
				n.Fault != model.FaultDeviceOffline || n.TextKey != testOfflineTextKey || n.DeviceType != "drone" {
				t.Errorf("notify payload = %+v", n)
			}
			deg := f.degradedEvents(t)
			if len(deg) != 1 || deg[0].Fault != model.FaultDeviceOffline || deg[0].Action != model.DegradeHoldNotify ||
				deg[0].TextKey != testOfflineTextKey || deg[0].OrderID != 3 || deg[0].TicketID != 7 {
				t.Fatalf("DEGRADED = %+v, want exactly one DEVICE_OFFLINE/HOLD_AND_NOTIFY", deg)
			}
			if len(f.audit.recs) != 2 {
				t.Fatalf("audit = %d DEVICE_UNAVAILABLE rows, want 2", len(f.audit.recs))
			}
		})
	}
}

// 通知写入失败：整笔回滚并按普通错误退避（应失败：仍按离线推后，通知永远不会补发）；
// 选设备传输层错误：照旧退避、不通知；等待超限：转 EXPIRED、不再通知。
func TestDispatch_DeviceOfflineFailures(t *testing.T) {
	t.Run("notify insert fails", func(t *testing.T) {
		f := newRealFixture(t, model.OrderApproved)
		f.picker.err = ErrDeviceUnavailable
		f.notify.err = errors.New("db down")
		err := f.svc.Dispatch(context.Background(), realMsg(1))
		if err == nil || errors.Is(err, ErrOutboxDeferred) {
			t.Fatalf("err = %v, want plain error (backoff)", err)
		}
		if len(f.degradedEvents(t)) != 0 {
			t.Errorf("DEGRADED written although notify failed")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		f := newRealFixture(t, model.OrderApproved)
		f.picker.err = errors.New("dial tcp: refused")
		err := f.svc.Dispatch(context.Background(), realMsg(1))
		if err == nil || errors.Is(err, ErrOutboxDeferred) || f.notify.calls != 0 {
			t.Fatalf("err = %v notify calls = %d, want backoff without notify", err, f.notify.calls)
		}
	})
	t.Run("wait limit expires", func(t *testing.T) {
		f := newDispatchFixture(t, model.OrderApproved, false)
		past := dispatchNow.Add(-time.Second)
		f.store.order.DispatchDeadlineAt = &past
		f.picker.err = ErrDeviceUnavailable
		if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
			t.Fatalf("err = %v, want nil (outbox DONE)", err)
		}
		if f.store.order.Status != string(model.OrderExpired) || f.dev.dryRuns != 0 || f.notify.calls != 0 {
			t.Fatalf("status=%s dryRuns=%d notify=%d", f.store.order.Status, f.dev.dryRuns, f.notify.calls)
		}
	})
}

// 端到端：下发器经轮询器处理设备离线 → outbox 推后 1 分钟、不计 attempts（第 4 次失败后的消息也不转 DEAD）。
func TestDispatch_DeviceOfflineThroughPoller(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	f.picker.err = ErrDeviceUnavailable
	msg := dispatchMsg(okDispatchPayload)
	msg.RowVer, msg.Attempts = 2, 4
	store := &memOutbox{msgs: []repository.OutboxMessage{msg}, retryOK: true}
	p, logs := newPoller(store, f.svc.Dispatch)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.retries)+len(store.defers)+len(store.done) != 0 || len(store.offline) != 1 ||
		store.offline[0].next.Sub(outboxNow) != OutboxDeviceOfflineDelay {
		t.Fatalf("retries=%v defers=%v done=%v offline=%v\n%s", store.retries, store.defers, store.done, store.offline, logs)
	}
	if len(f.notify.rows) != 1 || f.dev.dryRuns != 0 {
		t.Errorf("notify=%d dryRuns=%d", len(f.notify.rows), f.dev.dryRuns)
	}
}
