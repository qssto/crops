package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/agent"
	"agri-steward/internal/repository/rediskey"
)

// 分布式锁：SET key {token} NX PX {ttl}，释放与续期用 Lua 比对 token（母文档 Redis 键约定 4）。
// 锁只由外层 service 持有；WorkOrderService.Transition 不取锁。

const lockTokenBytes = 16

var (
	// ErrLockHeld 表示锁已被他人持有。
	ErrLockHeld = errors.New("repository: lock held by another owner")
	// ErrLockLost 表示释放或续期时 token 不匹配（已过期或被他人重新获取）。
	ErrLockLost = errors.New("repository: lock lost")
)

var (
	releaseScript = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`)
	renewScript   = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("PEXPIRE", KEYS[1], ARGV[2]) else return 0 end`)
)

// Locker 基于 Redis 的 token 锁。
type Locker struct {
	rdb redis.UniversalClient
}

// NewLocker 构造 Locker。
func NewLocker(rdb redis.UniversalClient) *Locker { return &Locker{rdb: rdb} }

// Lock 是一次持有的锁。
type Lock struct {
	rdb   redis.UniversalClient
	key   string
	token string
	ttl   time.Duration
}

// Acquire 尝试获取 key（已带 as:{env}: 前缀），失败返回 ErrLockHeld；不阻塞等待。
func (l *Locker) Acquire(ctx context.Context, key string, ttl time.Duration) (*Lock, error) {
	buf := make([]byte, lockTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("repository: lock token: %w", err)
	}
	token := hex.EncodeToString(buf)
	err := l.rdb.SetArgs(ctx, key, token, redis.SetArgs{Mode: "NX", TTL: ttl}).Err()
	if errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("%w: %s", ErrLockHeld, key)
	}
	if err != nil {
		return nil, fmt.Errorf("repository: acquire %s: %w", key, err)
	}
	return &Lock{rdb: l.rdb, key: key, token: token, ttl: ttl}, nil
}

// Renew 把 TTL 重置为获取时的 ttl；持锁做外部 IO 时按 ttl/3 调用。
func (k *Lock) Renew(ctx context.Context) error {
	n, err := renewScript.Run(ctx, k.rdb, []string{k.key}, k.token, k.ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("repository: renew %s: %w", k.key, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrLockLost, k.key)
	}
	return nil
}

// Release 只删除自己持有的锁。
func (k *Lock) Release(ctx context.Context) error {
	n, err := releaseScript.Run(ctx, k.rdb, []string{k.key}, k.token).Int()
	if err != nil {
		return fmt.Errorf("repository: release %s: %w", k.key, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrLockLost, k.key)
	}
	return nil
}

// RunLockTTL 是 lock:run:{run_id} 的 TTL（P0 L4：30s，持有者每 10s 续期）。
const RunLockTTL = 30 * time.Second

// RunLocker 实现 agent.RunLocker：Locker + rediskey.LockRun。
type RunLocker struct {
	locker *Locker
	keys   rediskey.Keys
}

var _ agent.RunLocker = (*RunLocker)(nil)

// NewRunLocker 构造 RunLocker。
func NewRunLocker(l *Locker, keys rediskey.Keys) *RunLocker { return &RunLocker{locker: l, keys: keys} }

// AcquireRun 取 run 锁；已被持有返回 ErrLockHeld。
func (r *RunLocker) AcquireRun(ctx context.Context, runID uint64) (agent.Lease, error) {
	lk, err := r.locker.Acquire(ctx, r.keys.LockRun(runID), RunLockTTL)
	if err != nil {
		return nil, err
	}
	return lk, nil
}
