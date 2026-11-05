package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/repository/rediskey"
)

// tokenBucketScript 是按分钟补满的令牌桶：hash {tokens, ts}，容量 ARGV[1]，按 ARGV[1]/60s 匀速补充；
// ARGV[2] 为当前毫秒（注入时钟），ARGV[3] 为键 TTL 毫秒，ARGV[4] 为 "take"（取一个，取到返回 1，否则 0）
// 或 "refund"（退还一个，不超过容量，返回 1）。
var tokenBucketScript = redis.NewScript(`local cap = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local v = redis.call("HMGET", KEYS[1], "tokens", "ts")
local tokens = tonumber(v[1]) or cap
local ts = tonumber(v[2]) or now
if now > ts then tokens = math.min(cap, tokens + (now - ts) * cap / 60000) end
local ok = 0
if ARGV[4] == "refund" then tokens = math.min(cap, tokens + 1); ok = 1
elseif tokens >= 1 then tokens = tokens - 1; ok = 1 end
redis.call("HSET", KEYS[1], "tokens", tostring(tokens), "ts", tostring(math.max(now, ts)))
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return ok`)

const (
	tokenOpTake   = "take"
	tokenOpRefund = "refund"
)

// TokenBucket 是 rl:model:{tier} / rl:judge:{tier} 的每分钟令牌桶（P2 §6.4 Redis 键表：TTL 2min）。
type TokenBucket struct {
	rdb redis.UniversalClient
}

// NewTokenBucket 构造 TokenBucket。
func NewTokenBucket(rdb redis.UniversalClient) *TokenBucket { return &TokenBucket{rdb: rdb} }

// Take 从 key（已带 as:{env}: 前缀）取一个令牌，perMinute 为容量与每分钟补充量；perMinute <= 0 表示不限，恒返回 true。
func (b *TokenBucket) Take(ctx context.Context, key string, perMinute int, now time.Time) (bool, error) {
	if perMinute <= 0 {
		return true, nil
	}
	n, err := b.run(ctx, key, perMinute, now, tokenOpTake)
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Refund 把 Take 取到但没用上的一个令牌退回 key（不超过容量）；perMinute <= 0 表示不限，无操作。
func (b *TokenBucket) Refund(ctx context.Context, key string, perMinute int, now time.Time) error {
	if perMinute <= 0 {
		return nil
	}
	_, err := b.run(ctx, key, perMinute, now, tokenOpRefund)
	return err
}

func (b *TokenBucket) run(ctx context.Context, key string, perMinute int, now time.Time, op string) (int, error) {
	n, err := tokenBucketScript.Run(ctx, b.rdb, []string{key}, perMinute, now.UnixMilli(),
		rediskey.RlTokenTTL.Milliseconds(), op).Int()
	if err != nil {
		return 0, fmt.Errorf("repository: token bucket %s %s: %w", op, key, err)
	}
	return n, nil
}
