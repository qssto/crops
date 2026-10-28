package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 比较整值后改写 / 删除（idem:tool 的 PENDING:{token} → DONE:{id}，失败按 token 删除）。
var (
	replaceIfScript = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3]) and 1 else return 0 end`)
	deleteIfScript  = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`)
)

// RedisIdem 是 mcp.IdemStore 的 Redis 实现；键由调用方经 rediskey 拼好。
type RedisIdem struct {
	rdb redis.UniversalClient
}

// NewRedisIdem 构造 RedisIdem。
func NewRedisIdem(rdb redis.UniversalClient) *RedisIdem { return &RedisIdem{rdb: rdb} }

// SetNX 是 SET key value NX PX ttl。
func (r *RedisIdem) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	err := r.rdb.SetArgs(ctx, key, value, redis.SetArgs{Mode: "NX", TTL: ttl}).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("repository: idem setnx %s: %w", key, err)
	}
	return true, nil
}

// Get 读值，不存在返回 false。
func (r *RedisIdem) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := r.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("repository: idem get %s: %w", key, err)
	}
	return v, true, nil
}

// Replace 仅当当前值等于 expect 时改写为 value 并设 TTL（Lua 原子）。
func (r *RedisIdem) Replace(ctx context.Context, key, expect, value string, ttl time.Duration) (bool, error) {
	n, err := replaceIfScript.Run(ctx, r.rdb, []string{key}, expect, value, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("repository: idem replace %s: %w", key, err)
	}
	return n == 1, nil
}

// DeleteIf 仅当当前值等于 expect 时删除（Lua 原子）。
func (r *RedisIdem) DeleteIf(ctx context.Context, key, expect string) (bool, error) {
	n, err := deleteIfScript.Run(ctx, r.rdb, []string{key}, expect).Int()
	if err != nil {
		return false, fmt.Errorf("repository: idem delete %s: %w", key, err)
	}
	return n == 1, nil
}

// RedisCache 是 mcp.Cache 的 Redis 实现（cache:mcp:{tool}:{args_digest}）。
type RedisCache struct {
	rdb redis.UniversalClient
}

// NewRedisCache 构造 RedisCache。
func NewRedisCache(rdb redis.UniversalClient) *RedisCache { return &RedisCache{rdb: rdb} }

// Get 读缓存，不存在返回 false。
func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("repository: cache get %s: %w", key, err)
	}
	return b, true, nil
}

// Set 写缓存并设 TTL。
func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("repository: cache set %s: %w", key, err)
	}
	return nil
}
