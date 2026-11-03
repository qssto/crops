package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// memCitations 是 CitationScanStore 桩：alerted 模拟已写入的 ALERT.advice_no_citation 审计行。
type memCitations struct {
	rows       []repository.AdviceKey
	listErr    error
	alertedErr error
	alerted    map[uint64]bool
	gotSys     repository.SystemScope
	gotFrom    time.Time
	gotTo      time.Time
	gotAction  model.AuditAction
	gotSince   time.Time
	gotFarms   []uint64
}

func (m *memCitations) AdviceWithoutCitation(_ context.Context, sys repository.SystemScope, from, to time.Time) ([]repository.AdviceKey, error) {
	m.gotSys, m.gotFrom, m.gotTo = sys, from, to
	return m.rows, m.listErr
}

func (m *memCitations) AlertedRuns(_ context.Context, scope model.Scope, action model.AuditAction, runIDs []uint64,
	since time.Time) (map[uint64]bool, error) {
	m.gotAction, m.gotSince = action, since
	m.gotFarms = append(m.gotFarms, scope.FarmID)
	if m.alertedErr != nil {
		return nil, m.alertedErr
	}
	out := map[uint64]bool{}
	for _, id := range runIDs {
		if m.alerted[id] {
			out[id] = true
		}
	}
	return out, nil
}

// auditingAlerts 模拟 AuditAlertSink：Fire 后把 run 记为已告警（下一轮 AlertedRuns 可见）。
type auditingAlerts struct {
	memAlerts
	store *memCitations
}

func (a *auditingAlerts) Fire(ctx context.Context, al model.Alert) {
	a.memAlerts.Fire(ctx, al)
	a.store.alerted[al.RunID] = true
}

func citationScanner(t *testing.T, store CitationScanStore, sink model.AlertSink, held map[string]bool, window time.Duration) *Scanner {
	t.Helper()
	f := newScanFixture(t)
	return NewScanner(ScanDeps{Tickets: f.store, Runs: f.store, Expirer: f.expirer, Orders: f.orders, Gate: f.gate,
		Resumer: f.resumer, Locks: probeLocks{held: held}, Keys: f.keys, Clock: fixedClock{decideNow},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Citations: store, Alerts: sink, CitationWindow: window})
}

// 红线：无引用建议发现。命中逐张 Fire(advice_no_citation, P0, run_id)，按农场分组查已告警；连跑两轮每张建议只告警一次；
// 应失败：已告警的跳过、lock 被持有跳过、查询失败返回错误、缺依赖不注册。
func TestScanner_AdviceNoCitation(t *testing.T) {
	rows := []repository.AdviceKey{{ID: 101, TenantID: 1, FarmID: 1, RunID: 11}, {ID: 102, TenantID: 1, FarmID: 2, RunID: 12},
		{ID: 103, TenantID: 1, FarmID: 1, RunID: 13}}
	keys := newScanFixture(t).keys

	t.Run("fires P0 once per advice across rounds", func(t *testing.T) {
		store := &memCitations{rows: rows, alerted: map[uint64]bool{}}
		sink := &auditingAlerts{store: store}
		s := citationScanner(t, store, sink, map[string]bool{}, 0)
		for range 2 {
			if err := s.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if len(sink.got) != 3 {
			t.Fatalf("alerts = %+v, want 3 (one per advice)", sink.got)
		}
		var runs []uint64
		for _, a := range sink.got {
			if a.Name != model.AlertAdviceNoCitation || a.Level != model.AlertP0 || a.OrderID != 0 || a.Scope.TenantID != 1 {
				t.Fatalf("alert = %+v", a)
			}
			runs = append(runs, a.RunID)
		}
		if !slices.Equal(runs, []uint64{11, 13, 12}) {
			t.Fatalf("runs = %v, want farm 1 first then farm 2", runs)
		}
		if sink.got[2].Scope.FarmID != 2 {
			t.Fatalf("run 12 scope = %+v, want farm 2", sink.got[2].Scope)
		}
		from := decideNow.Add(-DefaultNoCitationWindow)
		if store.gotSys != scanSystem || !store.gotFrom.Equal(from) || !store.gotTo.Equal(decideNow) ||
			store.gotAction != model.AlertAdviceNoCitation.AuditAction() || !store.gotSince.Equal(from) {
			t.Fatalf("sys = %v from = %v to = %v action = %s since = %v", store.gotSys, store.gotFrom, store.gotTo,
				store.gotAction, store.gotSince)
		}
		if !slices.Equal(store.gotFarms, []uint64{1, 2, 1, 2}) {
			t.Fatalf("AlertedRuns farms = %v, want one query per farm per round", store.gotFarms)
		}
	})
	t.Run("already alerted skipped", func(t *testing.T) {
		store := &memCitations{rows: rows, alerted: map[uint64]bool{11: true, 12: true}}
		sink := &memAlerts{}
		if err := citationScanner(t, store, sink, map[string]bool{}, 0).RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sink.got) != 1 || sink.got[0].RunID != 13 {
			t.Fatalf("alerts = %+v, want only run 13", sink.got)
		}
	})
	t.Run("duplicate row in batch alerts once", func(t *testing.T) {
		store := &memCitations{rows: []repository.AdviceKey{rows[0], rows[0]}, alerted: map[uint64]bool{}}
		sink := &memAlerts{}
		if err := citationScanner(t, store, sink, map[string]bool{}, 0).RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sink.got) != 1 {
			t.Fatalf("alerts = %+v, want 1", sink.got)
		}
	})
	t.Run("custom window", func(t *testing.T) {
		store := &memCitations{alerted: map[uint64]bool{}}
		if err := citationScanner(t, store, &memAlerts{}, map[string]bool{}, 5*time.Minute).RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !store.gotFrom.Equal(decideNow.Add(-5 * time.Minute)) {
			t.Fatalf("from = %v", store.gotFrom)
		}
	})
	t.Run("lock held skips", func(t *testing.T) {
		store := &memCitations{rows: rows, alerted: map[uint64]bool{}}
		sink := &memAlerts{}
		s := citationScanner(t, store, sink, map[string]bool{keys.LockScan(ScanAdviceNoCitation): true}, 0)
		if err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sink.got) != 0 || !store.gotTo.IsZero() {
			t.Fatalf("alerts = %+v, scanned while lock held", sink.got)
		}
	})
	t.Run("list error surfaces", func(t *testing.T) {
		store := &memCitations{listErr: errors.New("db down"), alerted: map[uint64]bool{}}
		if err := citationScanner(t, store, &memAlerts{}, map[string]bool{}, 0).RunOnce(context.Background()); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("alerted lookup error surfaces and does not fire", func(t *testing.T) {
		store := &memCitations{rows: rows, alertedErr: errors.New("db down"), alerted: map[uint64]bool{}}
		sink := &memAlerts{}
		if err := citationScanner(t, store, sink, map[string]bool{}, 0).RunOnce(context.Background()); err == nil {
			t.Fatal("want error")
		}
		if len(sink.got) != 0 {
			t.Fatalf("alerts = %+v, want none when dedup lookup fails", sink.got)
		}
	})
	t.Run("not registered without deps", func(t *testing.T) {
		for _, s := range []*Scanner{newScanFixture(t).svc,
			citationScanner(t, &memCitations{}, nil, map[string]bool{}, 0),
			citationScanner(t, nil, &memAlerts{}, map[string]bool{}, 0)} {
			for _, task := range s.tasks {
				if task.name == ScanAdviceNoCitation {
					t.Fatal("advice_no_citation registered without Citations / Alerts")
				}
			}
		}
	})
}

// slowAlerts 模拟审计短写慢的告警出口：每次 Fire 先睡 delay；cancel 非空时第一次 Fire 后取消调用方 ctx。
type slowAlerts struct {
	auditingAlerts
	delay  time.Duration
	cancel context.CancelFunc
}

func (a *slowAlerts) Fire(ctx context.Context, al model.Alert) {
	time.Sleep(a.delay)
	a.auditingAlerts.Fire(ctx, al)
	if a.cancel != nil {
		a.cancel()
	}
}

// 审查修复 P2（锁内阻塞）：单轮预算用尽即提前结束本轮（不报错），未发出的告警下一轮补发，不漏、不重；
// 应失败：调用方 ctx 取消时停止发告警并返回错误。
func TestScanner_AdviceNoCitation_RoundBudget(t *testing.T) {
	rows := []repository.AdviceKey{{ID: 101, TenantID: 1, FarmID: 1, RunID: 11}, {ID: 103, TenantID: 1, FarmID: 1, RunID: 13},
		{ID: 102, TenantID: 1, FarmID: 2, RunID: 12}}

	t.Run("budget exhausted defers rest to next round", func(t *testing.T) {
		store := &memCitations{rows: rows, alerted: map[uint64]bool{}}
		sink := &slowAlerts{auditingAlerts: auditingAlerts{store: store}, delay: 30 * time.Millisecond}
		s := citationScanner(t, store, sink, map[string]bool{}, 0)
		s.d.CitationBudget = 10 * time.Millisecond
		var perRound []int
		for range 4 {
			before := len(sink.got)
			if err := s.RunOnce(context.Background()); err != nil {
				t.Fatalf("budget exhaustion must not be an error: %v", err)
			}
			perRound = append(perRound, len(sink.got)-before)
		}
		if !slices.Equal(perRound, []int{1, 1, 1, 0}) {
			t.Fatalf("alerts per round = %v, want one per round until all three sent", perRound)
		}
		var runs []uint64
		for _, a := range sink.got {
			runs = append(runs, a.RunID)
		}
		if !slices.Equal(runs, []uint64{11, 13, 12}) {
			t.Fatalf("runs = %v, want each advice alerted exactly once", runs)
		}
	})
	t.Run("default budget is half the scan lock ttl", func(t *testing.T) {
		if DefaultCitationRoundBudget != scanLockTTL/2 || DefaultCitationRoundBudget <= 0 {
			t.Fatalf("DefaultCitationRoundBudget = %v, want %v", DefaultCitationRoundBudget, scanLockTTL/2)
		}
		store := &memCitations{rows: rows, alerted: map[uint64]bool{}}
		sink := &slowAlerts{auditingAlerts: auditingAlerts{store: store}}
		if err := citationScanner(t, store, sink, map[string]bool{}, 0).RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sink.got) != 3 {
			t.Fatalf("alerts = %d, want 3 within default budget", len(sink.got))
		}
	})
	t.Run("caller canceled stops and errors", func(t *testing.T) {
		store := &memCitations{rows: rows, alerted: map[uint64]bool{}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sink := &slowAlerts{auditingAlerts: auditingAlerts{store: store}, cancel: cancel}
		err := citationScanner(t, store, sink, map[string]bool{}, 0).RunOnce(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if len(sink.got) != 1 {
			t.Fatalf("alerts = %d, want 1 (stop after cancel)", len(sink.got))
		}
	})
}
