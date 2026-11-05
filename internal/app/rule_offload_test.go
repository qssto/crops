package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

type atClock struct{ t time.Time }

func (c atClock) Now() time.Time { return c.t }

func testKeys(t *testing.T) rediskey.Keys {
	t.Helper()
	k, err := rediskey.New("dev")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeShadowStore 记录每次 PurgeBatch 调用，按 left[项] 余量返回本批删除数。
type fakeShadowStore struct {
	farms  []repository.FarmRef
	left   map[repository.FarmRef]map[model.RuleOffloadItem]int64
	before []time.Time
	calls  []string
	err    error
}

func (s *fakeShadowStore) ShadowFarms(_ context.Context, _ repository.SystemScope, before time.Time) ([]repository.FarmRef, error) {
	s.before = append(s.before, before)
	return s.farms, nil
}

func (s *fakeShadowStore) PurgeBatch(_ context.Context, sys repository.SystemScope, farm repository.FarmRef,
	item model.RuleOffloadItem, before time.Time) (int64, error) {
	if sys.Task() != LoopRuleShadowPurge {
		return 0, errors.New("unexpected system scope " + sys.Task())
	}
	s.calls = append(s.calls, string(item))
	if s.err != nil {
		return 0, s.err
	}
	n := min(s.left[farm][item], repository.ShadowPurgeBatch)
	s.left[farm][item] -= n
	return n, nil
}

type fakeLease struct{ renews, releases int }

func (l *fakeLease) Renew(context.Context) error   { l.renews++; return nil }
func (l *fakeLease) Release(context.Context) error { l.releases++; return nil }

type fakeLocker struct {
	lease *fakeLease
	err   error
	key   string
	ttl   time.Duration
}

func (f *fakeLocker) AcquireLease(_ context.Context, key string, ttl time.Duration) (agent.Lease, error) {
	f.key, f.ttl = key, ttl
	if f.err != nil {
		return nil, f.err
	}
	return f.lease, nil
}

// TestShadowPurger_Tick（§6.5）：保留期 = now - 180 天；逐农场逐项分批，满批（5000）继续、不足一批停；每批续租，结束释放锁。
func TestShadowPurger_Tick(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)
	f1, f2 := repository.FarmRef{TenantID: 1, FarmID: 1}, repository.FarmRef{TenantID: 1, FarmID: 2}
	store := &fakeShadowStore{farms: []repository.FarmRef{f1, f2}, left: map[repository.FarmRef]map[model.RuleOffloadItem]int64{
		f1: {model.RuleItemOperationWindow: 10003},
		f2: {model.RuleItemTankMix: 5000},
	}}
	lock := &fakeLocker{lease: &fakeLease{}}
	p := &shadowPurger{store: store, locks: lock, key: testKeys(t).LockP2Job(LoopRuleShadowPurge), clock: atClock{now},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), keep: config.DefaultRuleOffload().Retention(), every: time.Hour}
	if err := p.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-180 * 24 * time.Hour); len(store.before) != 1 || !store.before[0].Equal(want) {
		t.Fatalf("before = %v, want %v", store.before, want)
	}
	// f1：operation_window 5000+5000+3，其余两项各 1 批；f2：tank_mix 5000+0，其余两项各 1 批。
	want := []string{"operation_window", "operation_window", "operation_window", "tank_mix", "pest_priority",
		"operation_window", "tank_mix", "tank_mix", "pest_priority"}
	if !slices.Equal(store.calls, want) {
		t.Fatalf("calls = %v, want %v", store.calls, want)
	}
	if store.left[f1][model.RuleItemOperationWindow] != 0 || store.left[f2][model.RuleItemTankMix] != 0 {
		t.Fatalf("left = %v", store.left)
	}
	if lock.key != "as:dev:lock:p2job:rule-shadow-purge" || lock.ttl != rediskey.LockP2JobTTL {
		t.Fatalf("lock %s ttl %s", lock.key, lock.ttl)
	}
	if lock.lease.renews != len(want) || lock.lease.releases != 1 {
		t.Fatalf("renews %d releases %d", lock.lease.renews, lock.lease.releases)
	}
}

// TestShadowPurger_LockHeldAndErrors：锁被持有时跳过本轮（不删）；删除失败返回错误且仍释放锁。
func TestShadowPurger_LockHeldAndErrors(t *testing.T) {
	f1 := repository.FarmRef{TenantID: 1, FarmID: 1}
	store := &fakeShadowStore{farms: []repository.FarmRef{f1}, left: map[repository.FarmRef]map[model.RuleOffloadItem]int64{f1: {}}}
	held := &fakeLocker{err: repository.ErrLockHeld}
	p := &shadowPurger{store: store, locks: held, clock: atClock{time.Now()},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), keep: time.Hour, every: time.Hour}
	if err := p.tick(t.Context()); err != nil || len(store.calls) != 0 {
		t.Fatalf("held: err %v calls %v", err, store.calls)
	}
	boom := errors.New("boom")
	store.err = boom
	lock := &fakeLocker{lease: &fakeLease{}}
	p.locks = lock
	if err := p.tick(t.Context()); !errors.Is(err, boom) || lock.lease.releases != 1 {
		t.Fatalf("err %v releases %d, want boom and release", err, lock.lease.releases)
	}
}

// TestRuleOffloadLoops：P2-L4 已登记；依赖未装配不起任务；配置非法报错；零值取缺省并起一个任务。
func TestRuleOffloadLoops(t *testing.T) {
	if !slices.ContainsFunc(p2Loops, func(e p2LoopEntry) bool { return e.name == "P2-L4" }) {
		t.Fatal("P2-L4 not registered")
	}
	if loops, err := ruleOffloadLoops(&App{}); err != nil || loops != nil {
		t.Fatalf("unwired = %v, %v", loops, err)
	}
	a := &App{DB: &repository.DB{}, Locker: repository.NewLocker(nil), Keys: testKeys(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	loops, err := ruleOffloadLoops(a)
	if err != nil || len(loops) != 1 || loops[0].Name != LoopRuleShadowPurge || loops[0].Run == nil {
		t.Fatalf("loops = %v, %v", loops, err)
	}
	a.ruleOffload = config.RuleOffload{ShadowDiffDays: -1, PurgeEvery: time.Hour}
	if _, err := ruleOffloadLoops(a); !errors.Is(err, config.ErrBadRuleOffload) {
		t.Fatalf("bad config err = %v", err)
	}
}
