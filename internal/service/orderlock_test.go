package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// scriptedLease 记录续租 / 释放；renewErr 非空时续租失败（模拟 Lua 校验 token 不符）。
type scriptedLease struct {
	mu       sync.Mutex
	renewErr error
	renews   int
	released bool
}

func (l *scriptedLease) Renew(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renews++
	return l.renewErr
}

func (l *scriptedLease) Release(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = true
	return nil
}

type leaseLocks struct {
	lease *scriptedLease
	key   string
	ttl   time.Duration
}

func (l *leaseLocks) AcquireLease(_ context.Context, key string, ttl time.Duration) (agent.Lease, error) {
	l.key, l.ttl = key, ttl
	return l.lease, nil
}

func withFastRenew(t *testing.T) {
	t.Helper()
	old := orderLockRenewEvery
	orderLockRenewEvery = time.Millisecond
	t.Cleanup(func() { orderLockRenewEvery = old })
}

func lockKeys(t *testing.T) rediskey.Keys {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// 续租失败：fn 的 ctx 以 ErrOrderLockLost 取消，WithOrderLock 返回 ErrOrderLockLost 并释放锁
// （应失败：续租失败后 fn 继续跑完、结果照常返回 nil）。
func TestWithOrderLock_RenewFailureStopsHolder(t *testing.T) {
	withFastRenew(t)
	lease := &scriptedLease{renewErr: repository.ErrLockLost}
	locks := &leaseLocks{lease: lease}
	keys := lockKeys(t)
	var logs bytes.Buffer
	var cause error
	err := WithOrderLock(context.Background(), locks, keys, slog.New(slog.NewTextHandler(&logs, nil)), 42,
		func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				cause = context.Cause(ctx)
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		})
	if !errors.Is(err, ErrOrderLockLost) || !errors.Is(cause, ErrOrderLockLost) {
		t.Fatalf("err = %v, cause = %v, want ErrOrderLockLost", err, cause)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.renews != 1 || !lease.released {
		t.Errorf("renews = %d released = %t, want one renew then stop", lease.renews, lease.released)
	}
	if locks.key != keys.LockOrder(42) || locks.ttl != orderLockTTL {
		t.Errorf("lock key %q ttl %s", locks.key, locks.ttl)
	}
}

// 续租成功：fn 正常返回其结果，期间持续续租，结束后释放。
func TestWithOrderLock_RenewsWhileHeld(t *testing.T) {
	withFastRenew(t)
	lease := &scriptedLease{}
	want := errors.New("fn result")
	err := WithOrderLock(context.Background(), &leaseLocks{lease: lease}, lockKeys(t), slog.New(slog.DiscardHandler), 1,
		func(ctx context.Context) error {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				lease.mu.Lock()
				n := lease.renews
				lease.mu.Unlock()
				if n >= 3 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if ctx.Err() != nil {
				t.Errorf("ctx canceled while renew ok: %v", context.Cause(ctx))
			}
			return want
		})
	if !errors.Is(err, want) || errors.Is(err, ErrOrderLockLost) {
		t.Fatalf("err = %v", err)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.renews < 3 || !lease.released {
		t.Errorf("renews = %d released = %t", lease.renews, lease.released)
	}
}

// 锁被占：返回 ErrOrderBusy，不执行 fn（应失败：fn 被调用）。
func TestWithOrderLock_Busy(t *testing.T) {
	called := false
	err := WithOrderLock(context.Background(), memLocks{held: true}, lockKeys(t), slog.New(slog.DiscardHandler), 1,
		func(context.Context) error { called = true; return nil })
	if !errors.Is(err, ErrOrderBusy) || called {
		t.Fatalf("err = %v called = %t", err, called)
	}
}
