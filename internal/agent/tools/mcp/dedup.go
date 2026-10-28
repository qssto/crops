package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/model"
)

// idem:tool 值的两种形态（母文档 Redis 键约定 5）。
const (
	pendingPrefix = "PENDING:"
	donePrefix    = "DONE:"
	tokenBytes    = 16
)

// claim 是本次调用持有的 PENDING 占位；key 为空表示没拿到占位（Redis 不可用或等待超时后直接重做）。
type claim struct {
	key, value string
}

func (c claim) held() bool { return c.key != "" }

// claim 抢占 idem:tool:{call_key}（P0 §6.3 流程 2）：
//   - 抢到占位：再查 MySQL OK 行（Redis 丢失场景），有则改写 DONE 并返回该行；
//   - 命中 DONE:{id}：回查 OK 行返回；行不存在（Redis 与 MySQL 不一致）按值删键后重抢；
//   - 命中 PENDING：同 key 调用在途，轮询等到占位 TTL，仍未完成则按 MySQL 判断，没有就不占位直接重做；
//   - Redis 出错：按 MySQL 判断，没有就不占位直接重做。
//
// 返回的 hit 非 nil 表示去重命中；error 只来自 MySQL 回查或 ctx 取消。
func (g *Gateway) claim(ctx context.Context, c *callState) (claim, *model.ToolCallHit, error) {
	key := g.opt.Keys.IdemTool(c.callKey)
	token, err := newToken()
	if err != nil {
		return claim{}, nil, err
	}
	mine := claim{key: key, value: pendingPrefix + token}
	ttl := c.tool.pendingTTL()
	maxPolls := int(ttl/g.opt.PendingPoll) + 1
	for polls := 0; polls < maxPolls; polls++ {
		ok, err := g.opt.Idem.SetNX(ctx, key, mine.value, ttl)
		if err != nil {
			g.opt.Logger.WarnContext(ctx, "mcp idem unavailable, fall back to mysql", "key", key, "err", err)
			return g.claimFromMySQL(ctx, c, claim{})
		}
		if ok {
			return g.claimFromMySQL(ctx, c, mine)
		}
		v, found, err := g.opt.Idem.Get(ctx, key)
		if err != nil {
			g.opt.Logger.WarnContext(ctx, "mcp idem unavailable, fall back to mysql", "key", key, "err", err)
			return g.claimFromMySQL(ctx, c, claim{})
		}
		switch {
		case !found: // 占位刚过期或被删，重抢
			continue
		case strings.HasPrefix(v, donePrefix):
			hit, ok, err := g.findOK(ctx, c)
			if err != nil || ok {
				return claim{}, hit, err
			}
			g.opt.Logger.WarnContext(ctx, "mcp idem DONE without OK row, reclaim", "key", key, "value", v)
			if _, err := g.opt.Idem.DeleteIf(ctx, key, v); err != nil {
				return g.claimFromMySQL(ctx, c, claim{})
			}
			continue
		}
		if err := sleep(ctx, g.opt.PendingPoll); err != nil {
			return claim{}, nil, err
		}
	}
	// 在途调用超过占位 TTL 仍未完成：按 MySQL 判断，没有就不占位直接重做。
	return g.claimFromMySQL(ctx, c, claim{})
}

// claimFromMySQL 回查 OK 行：有则返回命中（持有占位时改写为 DONE），没有则带着 held 继续调用。
func (g *Gateway) claimFromMySQL(ctx context.Context, c *callState, held claim) (claim, *model.ToolCallHit, error) {
	hit, ok, err := g.findOK(ctx, c)
	if err != nil {
		c.claim = held
		g.release(ctx, c)
		return claim{}, nil, err
	}
	if !ok {
		return held, nil, nil
	}
	if held.held() {
		g.markDone(ctx, held, hit.ID)
	}
	return claim{}, hit, nil
}

func (g *Gateway) findOK(ctx context.Context, c *callState) (*model.ToolCallHit, bool, error) {
	hit, ok, err := g.opt.Calls.FindOK(ctx, c.scope, c.callKey)
	if err != nil {
		return nil, false, fmt.Errorf("find ok tool_call %s: %w", c.callKey, err)
	}
	if !ok {
		return nil, false, nil
	}
	return &hit, true, nil
}

// markDone 把占位改写为 DONE:{id}（24h）；失败只记 warn，丢失时以 MySQL OK 行为准。
func (g *Gateway) markDone(ctx context.Context, cl claim, id uint64) {
	done := donePrefix + strconv.FormatUint(id, 10)
	ok, err := g.opt.Idem.Replace(ctx, cl.key, cl.value, done, doneTTL)
	if err != nil || !ok {
		g.opt.Logger.WarnContext(ctx, "mcp idem mark done failed", "key", cl.key, "replaced", ok, "err", err)
	}
}

// release 按 token 删除本次占位（失败结果与中途出错都不参与去重）；ctx 已取消时仍尽量删除。
func (g *Gateway) release(ctx context.Context, c *callState) {
	if !c.claim.held() {
		return
	}
	if _, err := g.opt.Idem.DeleteIf(context.WithoutCancel(ctx), c.claim.key, c.claim.value); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp idem release failed", "key", c.claim.key, "err", err)
	}
}

func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("idem token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// sleep 等待 d 或 ctx 取消。
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
