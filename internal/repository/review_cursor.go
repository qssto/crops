package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/model"
	"agri-steward/internal/repository/rediskey"
)

// ReviewEnqueueCursor 是 P2-L2b 线上 judge 专家队列入队的逐农场续扫游标
// （cursor:review_enqueue:{tenant_id}:{farm_id}，值为最后读到的 online_judge_result.id，TTL rediskey.ReviewEnqueueCursorTTL）。
type ReviewEnqueueCursor struct {
	rdb  redis.UniversalClient
	keys rediskey.Keys
}

// NewReviewEnqueueCursor 构造 ReviewEnqueueCursor。
func NewReviewEnqueueCursor(rdb redis.UniversalClient, keys rediskey.Keys) ReviewEnqueueCursor {
	return ReviewEnqueueCursor{rdb: rdb, keys: keys}
}

// EnqueueCursor 读农场的续扫位置；键不存在（扫完已删 / 过期 / 首轮）返回 0。
func (c ReviewEnqueueCursor) EnqueueCursor(ctx context.Context, scope model.Scope) (uint64, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	key := c.keys.ReviewEnqueueCursor(scope.TenantID, scope.FarmID)
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("repository: get %s: %w", key, err)
	}
	after, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("repository: parse %s: %w", key, err)
	}
	return after, nil
}

// SetEnqueueCursor 记下农场的续扫位置（带 TTL）。
func (c ReviewEnqueueCursor) SetEnqueueCursor(ctx context.Context, scope model.Scope, after uint64) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	key := c.keys.ReviewEnqueueCursor(scope.TenantID, scope.FarmID)
	if err := c.rdb.Set(ctx, key, strconv.FormatUint(after, 10), rediskey.ReviewEnqueueCursorTTL).Err(); err != nil {
		return fmt.Errorf("repository: set %s: %w", key, err)
	}
	return nil
}

// ResetEnqueueCursor 删除农场的续扫位置（扫到末尾，下一轮从头扫）。
func (c ReviewEnqueueCursor) ResetEnqueueCursor(ctx context.Context, scope model.Scope) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	key := c.keys.ReviewEnqueueCursor(scope.TenantID, scope.FarmID)
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("repository: del %s: %w", key, err)
	}
	return nil
}
