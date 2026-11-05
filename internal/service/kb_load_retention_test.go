package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// fakeKBPurger 按农场记剩余行数，每次 PurgeKBLoadItems 删 min(剩余, 5000)。
type fakeKBPurger struct {
	left     map[uint64]int64
	before   time.Time
	calls    int
	purgeErr error
}

func (f *fakeKBPurger) KBLoadFarms(_ context.Context, _ repository.SystemScope, before time.Time) ([]model.Scope, error) {
	f.before = before
	var out []model.Scope
	for _, sc := range []model.Scope{{TenantID: 1, FarmID: 1}, {TenantID: 1, FarmID: 2}} {
		if f.left[sc.FarmID] > 0 {
			out = append(out, sc)
		}
	}
	return out, nil
}

func (f *fakeKBPurger) PurgeKBLoadItems(_ context.Context, sc model.Scope, before time.Time) (int64, error) {
	f.calls++
	if f.purgeErr != nil {
		return 0, f.purgeErr
	}
	if !before.Equal(f.before) {
		return 0, errors.New("cutoff changed between farms")
	}
	n := min(f.left[sc.FarmID], repository.KBLoadPurgeBatch)
	f.left[sc.FarmID] -= n
	return n, nil
}

type kbLease struct{ renews, releases int }

func (l *kbLease) Renew(context.Context) error   { l.renews++; return nil }
func (l *kbLease) Release(context.Context) error { l.releases++; return nil }

type kbLocks struct {
	lease *kbLease
	err   error
	key   string
}

func (l *kbLocks) AcquireLease(_ context.Context, key string, _ time.Duration) (agent.Lease, error) {
	l.key = key
	if l.err != nil {
		return nil, l.err
	}
	return l.lease, nil
}

func newKBRetention(store KBLoadPurger, locks KeyLocker) *KBLoadRetention {
	return &KBLoadRetention{Store: store, Locks: locks, LockKey: "lock:p2job:" + KBLoadRetentionJob,
		Clock:  fixedClock{t: time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Days: 180}
}

// 逐农场删到不足一批为止；截止时间 = now - days；每农场续期一次，结束释放锁。
func TestKBLoadRetention_PurgesPerFarm(t *testing.T) {
	a, b := model.Scope{TenantID: 1, FarmID: 1}, model.Scope{TenantID: 1, FarmID: 2}
	store := &fakeKBPurger{left: map[uint64]int64{a.FarmID: 12001, b.FarmID: 5000}}
	locks := &kbLocks{lease: &kbLease{}}
	r := newKBRetention(store, locks)
	n, err := r.Tick(context.Background())
	if err != nil || n != 17001 {
		t.Fatalf("Tick = %d, %v; want 17001", n, err)
	}
	// a：5000 + 5000 + 2001；b：5000 + 0（满批后再删一次确认）
	if store.calls != 5 || store.left[a.FarmID] != 0 || store.left[b.FarmID] != 0 {
		t.Fatalf("calls %d left %v", store.calls, store.left)
	}
	if want := time.Date(2026, 5, 8, 3, 0, 0, 0, time.UTC); !store.before.Equal(want) {
		t.Fatalf("cutoff %v, want %v", store.before, want)
	}
	if locks.key != "lock:p2job:kb-load-retention" || locks.lease.renews != 2 || locks.lease.releases != 1 {
		t.Fatalf("lock key %q renews %d releases %d", locks.key, locks.lease.renews, locks.lease.releases)
	}
}

// 单农场单轮至多 kbLoadMaxBatchesPerFarm 次 DELETE，余量留到下一轮。
func TestKBLoadRetention_BatchCap(t *testing.T) {
	a := model.Scope{TenantID: 1, FarmID: 1}
	store := &fakeKBPurger{left: map[uint64]int64{a.FarmID: repository.KBLoadPurgeBatch * (kbLoadMaxBatchesPerFarm + 3)}}
	n, err := newKBRetention(store, nil).Tick(context.Background())
	if err != nil || n != repository.KBLoadPurgeBatch*kbLoadMaxBatchesPerFarm || store.calls != kbLoadMaxBatchesPerFarm {
		t.Fatalf("Tick = %d, %v, calls %d", n, err, store.calls)
	}
}

func TestKBLoadRetention_LockAndErrors(t *testing.T) {
	a := model.Scope{TenantID: 1, FarmID: 1}
	store := &fakeKBPurger{left: map[uint64]int64{a.FarmID: 10}}
	if n, err := newKBRetention(store, &kbLocks{err: repository.ErrLockHeld}).Tick(context.Background()); n != 0 || err != nil || store.calls != 0 {
		t.Fatalf("lock held: %d, %v, calls %d", n, err, store.calls)
	}
	boom := errors.New("redis down")
	if _, err := newKBRetention(store, &kbLocks{err: boom}).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("lock error = %v", err)
	}
	store.purgeErr = errors.New("db down")
	lease := &kbLease{}
	if _, err := newKBRetention(store, &kbLocks{lease: lease}).Tick(context.Background()); !errors.Is(err, store.purgeErr) || lease.releases != 1 {
		t.Fatalf("purge error = %v, releases %d", err, lease.releases)
	}
	r := newKBRetention(store, nil)
	r.Days = 0
	if _, err := r.Tick(context.Background()); err == nil {
		t.Fatal("days 0: want error")
	}
}
