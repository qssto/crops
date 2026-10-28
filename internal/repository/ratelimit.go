package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/repository/rediskey"
)

// AskRateWindow 是 rl:farm:{farm_id}:ask 的固定窗口（P0 L4，1min）。
const AskRateWindow = time.Minute

// fixedWindowScript 原子执行 INCR + 首次 PEXPIRE，返回窗口内计数。
var fixedWindowScript = redis.NewScript(`local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n`)

// AskLimiter 是单农场提问限流：每个 AskRateWindow 窗口最多 limit 次。
type AskLimiter struct {
	rdb   redis.UniversalClient
	keys  rediskey.Keys
	limit int64
}

// NewAskLimiter 构造 AskLimiter；limit 为每窗口允许次数。
func NewAskLimiter(rdb redis.UniversalClient, keys rediskey.Keys, limit int64) *AskLimiter {
	return &AskLimiter{rdb: rdb, keys: keys, limit: limit}
}

// Allow 计一次提问，超限返回 false。
func (l *AskLimiter) Allow(ctx context.Context, farmID uint64) (bool, error) {
	key := l.keys.RateFarmAsk(farmID)
	n, err := fixedWindowScript.Run(ctx, l.rdb, []string{key}, AskRateWindow.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("repository: rate %s: %w", key, err)
	}
	return n <= l.limit, nil
}
