package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// memExpiring 是 ExpiringScanStore 桩：记录查询时刻与凭证。
type memExpiring struct {
	rows    []repository.ExpiringTicket
	listErr error
	gotNow  time.Time
	gotSys  repository.SystemScope
	gotLim  int
	txs     int
}

func (m *memExpiring) InTx(_ context.Context, fn func(tx *repository.DB) error) error {
	m.txs++
	return fn(nil)
}

func (m *memExpiring) ListExpiring(_ context.Context, sys repository.SystemScope, now time.Time, limit int) ([]repository.ExpiringTicket, error) {
	m.gotSys, m.gotNow, m.gotLim = sys, now, limit
	return m.rows, m.listErr
}

// dedupOutbox 模拟 uk_dedup + INSERT IGNORE：同一 dedup_key 只插入一次。
type dedupOutbox struct {
	keys     map[string]bool
	inserted []string
	failKey  string
}

func (o *dedupOutbox) InsertTx(_ context.Context, _ *repository.DB, _ model.OrderRef, key string, _ []byte, _ time.Time) (bool, error) {
	if key == o.failKey {
		return false, errors.New("db down")
	}
	if o.keys[key] {
		return false, nil
	}
	o.keys[key] = true
	o.inserted = append(o.inserted, key)
	return true, nil
}

func expiringScanner(t *testing.T, store *memExpiring, out *dedupOutbox, held map[string]bool) *Scanner {
	t.Helper()
	f := newScanFixture(t)
	return NewScanner(ScanDeps{Tickets: f.store, Runs: f.store, Expirer: f.expirer, Orders: f.orders, Gate: f.gate,
		Resumer: f.resumer, Locks: probeLocks{held: held}, Keys: f.keys, Clock: fixedClock{decideNow},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Expiring: store, Notify: out})
}

// approval_expiring：命中票逐张写 EXPIRING_SOON（scanSystem、本轮 now），连跑两轮每张票只提醒一次；
// 应失败：单张写失败不影响其余且返回错误；lock:scan:approval_expiring 被持有则跳过（多实例互斥）；未配依赖不注册。
func TestScanner_ApprovalExpiring(t *testing.T) {
	rows := []repository.ExpiringTicket{{ID: 7, TenantID: 1, FarmID: 1, OrderID: 9}, {ID: 8, TenantID: 1, FarmID: 2, OrderID: 10}}
	keys := newScanFixture(t).keys

	t.Run("notifies once per ticket", func(t *testing.T) {
		store, out := &memExpiring{rows: rows}, &dedupOutbox{keys: map[string]bool{}}
		s := expiringScanner(t, store, out, map[string]bool{})
		for range 2 {
			if err := s.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{model.NotifyDedupKey(model.NotifyExpiringSoon, 9, 7), model.NotifyDedupKey(model.NotifyExpiringSoon, 10, 8)}
		if !slices.Equal(out.inserted, want) || store.gotSys != scanSystem || !store.gotNow.Equal(decideNow) || store.txs != 4 ||
			store.gotLim != repository.ExpiringBatch {
			t.Fatalf("inserted = %v sys = %v now = %v txs = %d limit = %d", out.inserted, store.gotSys, store.gotNow, store.txs,
				store.gotLim)
		}
	})
	t.Run("one insert fails, others continue", func(t *testing.T) {
		out := &dedupOutbox{keys: map[string]bool{}, failKey: model.NotifyDedupKey(model.NotifyExpiringSoon, 9, 7)}
		s := expiringScanner(t, &memExpiring{rows: rows}, out, map[string]bool{})
		if err := s.RunOnce(context.Background()); err == nil {
			t.Fatal("want error")
		}
		if !slices.Equal(out.inserted, []string{model.NotifyDedupKey(model.NotifyExpiringSoon, 10, 8)}) {
			t.Fatalf("inserted = %v", out.inserted)
		}
	})
	t.Run("lock held skips", func(t *testing.T) {
		store, out := &memExpiring{rows: rows}, &dedupOutbox{keys: map[string]bool{}}
		s := expiringScanner(t, store, out, map[string]bool{keys.LockScan(ScanApprovalExpiring): true})
		if err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(out.inserted) != 0 || store.txs != 0 {
			t.Fatalf("inserted = %v", out.inserted)
		}
	})
	t.Run("list error surfaces", func(t *testing.T) {
		s := expiringScanner(t, &memExpiring{listErr: errors.New("db down")}, &dedupOutbox{keys: map[string]bool{}}, map[string]bool{})
		if err := s.RunOnce(context.Background()); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("not registered without deps", func(t *testing.T) {
		f := newScanFixture(t)
		for _, task := range f.svc.tasks {
			if task.name == ScanApprovalExpiring {
				t.Fatal("approval_expiring registered without Expiring / Notify")
			}
		}
	})
}

// P1-L5 审查修复 15：缺 Expiring / Notify 时 approval_expiring 不注册，Run 启动必须打 Warn 并点名缺的依赖
// （应失败：静默跳过时 worker 正常起来但即将过期提醒从不发出，无人察觉）。
func TestScanner_RunWarnsWhenExpiringDisabled(t *testing.T) {
	tests := []struct {
		name     string
		expiring ExpiringScanStore
		notify   NotifyOutboxWriter
		want     string // 空表示不应告警
	}{
		{name: "both wired", expiring: &memExpiring{}, notify: &dedupOutbox{}},
		{name: "expiring missing", notify: &dedupOutbox{}, want: "missing=[Expiring]"},
		{name: "notify missing", expiring: &memExpiring{}, want: "missing=[Notify]"},
		{name: "both missing", want: "missing=\"[Expiring Notify]\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newScanFixture(t)
			var buf bytes.Buffer
			s := NewScanner(ScanDeps{Tickets: f.store, Runs: f.store, Expirer: f.expirer, Orders: f.orders, Gate: f.gate,
				Resumer: f.resumer, Locks: probeLocks{held: map[string]bool{}}, Keys: f.keys, Clock: fixedClock{decideNow},
				Logger: slog.New(slog.NewTextHandler(&buf, nil)), Expiring: tt.expiring, Notify: tt.notify})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.Run(ctx)
			got := buf.String()
			if tt.want == "" {
				if got != "" {
					t.Fatalf("unexpected log: %s", got)
				}
				return
			}
			if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "task="+ScanApprovalExpiring) ||
				!strings.Contains(got, tt.want) {
				t.Fatalf("log = %q, want WARN task=%s %s", got, ScanApprovalExpiring, tt.want)
			}
		})
	}
}
