package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// 结果来源（trace 的 from 字段）。
const (
	fromInvoke = "invoke"
	fromCache  = "cache"
	fromDedup  = "dedup"
	fromPolicy = "policy"
)

// argFieldID 是需要做租户校验的地块入参键。
const argFieldID = "field_id"

// errNotOwned 表示入参 field_id 不属于 scope（越权）。
var errNotOwned = errors.New("field not in scope")

// callState 是一次调用的上下文。
type callState struct {
	scope     model.Scope
	call      agent.ToolCall
	tool      *tool // 未注册时为 nil
	epoch     int
	digest    string
	callKey   string
	start     time.Time
	claim     claim
	timeSense bool
}

// outcome 是一次调用的结果：code 为空表示 OK。
type outcome struct {
	res  Result
	code ErrorCode
	msg  string
	from string
}

func failed(code ErrorCode, from, format string, a ...any) outcome {
	return outcome{code: code, from: from, msg: fmt.Sprintf(format, a...)}
}

// Call 实现 agent.ToolProvider（P0 §6.3 流程 1-4）。工具失败以 ToolResult.Status / ErrorCode 表达并照常落行；
// error 只用于 ctx 取消与存储失败（tool_call 无法落行、租户校验查询失败等）。
func (g *Gateway) Call(ctx context.Context, scope model.Scope, call agent.ToolCall) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", call.Tool, err)
	}
	c := g.newState(scope, call)
	if _, err := g.policy.Before(call.Tool, call.Args); err != nil {
		return g.finish(ctx, c, failed(ErrRejected, fromPolicy, "%v", err))
	}
	if err := g.checkScope(ctx, scope, call.Args); errors.Is(err, errNotOwned) {
		return g.finish(ctx, c, failed(ErrRejected, fromPolicy, "%v", err))
	} else if err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", call.Tool, err)
	}
	cl, hit, err := g.claim(ctx, c)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", call.Tool, err)
	}
	if hit != nil {
		return g.replay(ctx, c, *hit), nil
	}
	c.claim = cl
	o, err := g.fetch(ctx, c)
	if err != nil {
		g.release(ctx, c)
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", call.Tool, err)
	}
	return g.finish(ctx, c, o)
}

func (g *Gateway) newState(scope model.Scope, call agent.ToolCall) *callState {
	c := &callState{scope: scope, call: call, tool: g.tools[call.Tool], start: g.opt.Clock.Now()}
	if c.tool != nil && c.tool.spec.TimeSensitive {
		c.epoch, c.timeSense = call.Epoch, true
	}
	canon := Canonical(call.Args)
	c.digest = ArgsDigest(canon)
	c.callKey = CallKey(call.RunID, call.StepID, call.Tool, canon, call.CallSeq, c.epoch)
	return c
}

// checkScope 是租户隔离红线：入参带 field_id 时必须属于 scope 的 tenant_id / farm_id，否则 errNotOwned。
// 入参已经 policy.Before 校验为 JSON 对象。
func (g *Gateway) checkScope(ctx context.Context, scope model.Scope, args json.RawMessage) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return fmt.Errorf("%w: %v", errNotOwned, err)
	}
	raw, ok := m[argFieldID]
	if !ok {
		return nil
	}
	id, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad field_id %s", errNotOwned, raw)
	}
	owns, err := g.opt.Fields.Owns(ctx, scope, id)
	if err != nil {
		return fmt.Errorf("check field scope: %w", err)
	}
	if !owns {
		return fmt.Errorf("%w: field %d (tenant %d, farm %d)", errNotOwned, id, scope.TenantID, scope.FarmID)
	}
	return nil
}

// fetch 先读缓存（时效工具 epoch>0 与 pre_dispatch 复检不读），未命中再调 Server；error 只表示 ctx 取消。
func (g *Gateway) fetch(ctx context.Context, c *callState) (outcome, error) {
	t := c.tool
	key := g.opt.Keys.CacheMCP(t.spec.Name, c.digest)
	if t.cacheTTL > 0 && !c.bypassCache() {
		if res, ok := g.cacheGet(ctx, key); ok {
			return outcome{res: res, from: fromCache}, nil
		}
	}
	return g.invoke(ctx, t, c.call.Args)
}

// bypassCache：epoch>0 的时效调用与 pre_dispatch 复检一律不读缓存（P0 §6.3 流程 3）。
func (c *callState) bypassCache() bool {
	return (c.timeSense && c.epoch > 0) || c.call.StepID == PreDispatchStep
}

func (g *Gateway) cacheGet(ctx context.Context, key string) (Result, bool) {
	b, ok, err := g.opt.Cache.Get(ctx, key)
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache get failed", "key", key, "err", err)
		return Result{}, false
	}
	if !ok {
		return Result{}, false
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache entry undecodable", "key", key, "err", err)
		return Result{}, false
	}
	return res, true
}

func (g *Gateway) cachePut(ctx context.Context, c *callState, res Result) {
	b, err := json.Marshal(res)
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache encode failed", "tool", c.call.Tool, "err", err)
		return
	}
	key := g.opt.Keys.CacheMCP(c.call.Tool, c.digest)
	if err := g.opt.Cache.Set(ctx, key, b, c.tool.cacheTTL); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache set failed", "key", key, "err", err)
	}
}
