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
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
)

// 结果来源（trace 的 from 字段）。
const (
	fromInvoke = "invoke"
	fromCache  = "cache"
	// fromFallback：调用 TIMEOUT / UPSTREAM 后回落到 max_staleness 内的缓存（source=cache，P1 §6.7）。
	fromFallback = "cache_fallback"
	fromDedup    = "dedup"
	fromPolicy   = "policy"
)

// errNotOwned 表示归属入参畸形（field_id 非正整数、device_id 非字符串等），按 REJECTED 返回。
var errNotOwned = errors.New("scoped argument malformed")

// scopedArgs 是需要做租户校验的入参：field_id（field_profile.id）与 device_id。
type scopedArgs struct {
	FieldID  *json.RawMessage `json:"field_id"`
	DeviceID *string          `json:"device_id"`
}

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

// alertActuateFromAgent 是告警日志的 alert 字段：模型发起了 actuate 调用（执行动作只允许 service/workorder 经 adapter 调用）。
const alertActuateFromAgent = "ACTUATE_FROM_AGENT"

// alertActuate 以 ERROR 级别记录来自模型的 actuate 调用（兜底拒绝已由 policy.Before 完成，这里只告警）；
// 配置了 Alerts 时另发 P0 告警 model_actuate（run_id 下钻）。
func (g *Gateway) alertActuate(ctx context.Context, scope model.Scope, call agent.ToolCall, err error) {
	g.opt.Logger.ErrorContext(ctx, "mcp actuate call from agent rejected",
		"alert", alertActuateFromAgent, "tool", call.Tool, "tenant_id", scope.TenantID, "farm_id", scope.FarmID,
		"run_id", call.RunID, "step_id", call.StepID, "err", err)
	if g.opt.Alerts != nil {
		g.opt.Alerts.Fire(ctx, model.Alert{Name: model.AlertModelActuate, Level: model.AlertP0,
			Scope: model.Scope{TenantID: scope.TenantID, FarmID: scope.FarmID}, RunID: call.RunID,
			Detail: "actuate tool " + call.Tool + " rejected by policy"})
	}
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
		if errors.Is(err, policy.ErrActuateDenied) {
			g.alertActuate(ctx, scope, call, err)
		}
		return g.finish(ctx, c, failed(ErrRejected, fromPolicy, "%v", err))
	}
	if err := g.checkScope(ctx, scope, call); errors.Is(err, errNotOwned) {
		return g.finish(ctx, c, failed(ErrRejected, fromPolicy, "%v", err))
	} else if errors.Is(err, agent.ErrScopeDenied) {
		return g.finish(ctx, c, failed(ErrScopeDenied, fromPolicy, "%v", err))
	} else if err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", call.Tool, err)
	}
	cl, hit, err := g.claim(ctx, c)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", call.Tool, err)
	}
	if hit != nil {
		if res, ok := g.replay(ctx, c, *hit); ok {
			return res, nil
		}
		// 取不回原始结果：不占位直接重做（P0 模型可调工具只有 read 级），落一行新 attempt。
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

// checkScope 是租户隔离红线：入参带 field_id / device_id 时必须属于 scope 的 tenant_id / farm_id，
// 由 gateway 统一校验，不依赖各 Server 自己实现。入参畸形为 errNotOwned；越界写 CROSS_FARM_DENIED 审计
// （field 经 Fields.CheckField，设备经 Devices.OwnsDevice + Fields.Deny）并返回 agent.ErrScopeDenied。
// 入参已经 policy.Before 校验为 JSON 对象。
func (g *Gateway) checkScope(ctx context.Context, scope model.Scope, call agent.ToolCall) error {
	if len(bytes.TrimSpace(call.Args)) == 0 {
		return nil
	}
	var a scopedArgs
	if err := json.Unmarshal(call.Args, &a); err != nil {
		return fmt.Errorf("%w: %v", errNotOwned, err)
	}
	if a.FieldID != nil {
		if err := g.checkField(ctx, scope, call.RunID, *a.FieldID); err != nil {
			return err
		}
	}
	if a.DeviceID != nil {
		return g.checkDevice(ctx, scope, call.RunID, *a.DeviceID)
	}
	return nil
}

func (g *Gateway) checkField(ctx context.Context, scope model.Scope, runID uint64, raw json.RawMessage) error {
	id, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad field_id %s", errNotOwned, raw)
	}
	err = g.opt.Fields.CheckField(ctx, scope, runID, id)
	if err != nil && !errors.Is(err, agent.ErrScopeDenied) {
		return fmt.Errorf("check field scope: %w", err)
	}
	return err
}

// checkDevice：设备不存在与属于他场同样 SCOPE_DENIED 并写审计（不暴露设备存在性）。
func (g *Gateway) checkDevice(ctx context.Context, scope model.Scope, runID uint64, deviceID string) error {
	owns, err := g.opt.Devices.OwnsDevice(ctx, scope, deviceID)
	if err != nil {
		return fmt.Errorf("check device scope %q: %w", deviceID, err)
	}
	if owns {
		return nil
	}
	if err := g.opt.Fields.Deny(ctx, scope, runID, deviceTarget(deviceID)); err != nil {
		return fmt.Errorf("audit scope denied: %w", err)
	}
	return fmt.Errorf("%w: device %q not in farm (%d,%d)", agent.ErrScopeDenied, deviceID, scope.TenantID, scope.FarmID)
}

// fetch 先读新鲜期内的缓存（Bypass 的调用不读：时效工具 epoch>0、下发前复检、pre_dispatch），未命中再调 Server；
// 调用重试后仍 TIMEOUT / UPSTREAM 时回落到 max_staleness 内的缓存（同样不覆盖 bypassCache 的调用）。error 只表示 ctx 取消。
// Server 一律拿到带 Scope 的 ctx（mcp.WithScope）；返回 SCOPE_DENIED 时补写越界审计。
func (g *Gateway) fetch(ctx context.Context, c *callState) (outcome, error) {
	t := c.tool
	key := c.cacheKey(g.opt.Keys)
	useCache := t.cacheTTL > 0 && !c.bypassCache()
	if useCache {
		if e, ok := g.cacheGet(ctx, key); ok && g.fresh(e, t.cacheTTL) {
			return outcome{res: e.Result, from: fromCache}, nil
		}
	}
	o, err := g.invoke(WithScope(ctx, c.scope), t, c.call.Args)
	if err != nil {
		return o, err
	}
	if useCache && (o.code == ErrTimeout || o.code == ErrUpstream) {
		return g.fallback(ctx, c, key, o), nil
	}
	if o.code != ErrScopeDenied {
		return o, nil
	}
	if derr := g.opt.Fields.Deny(ctx, c.scope, c.call.RunID, deniedTarget(c.call)); derr != nil {
		return outcome{}, fmt.Errorf("audit scope denied: %w", derr)
	}
	return o, nil
}

// deniedTarget 是 Server 返回 SCOPE_DENIED 时的审计 target：带 device_id 记 device:{id}，否则记 tool:{name}。
func deniedTarget(call agent.ToolCall) string {
	var a scopedArgs
	if json.Unmarshal(call.Args, &a) == nil && a.DeviceID != nil && *a.DeviceID != "" {
		return deviceTarget(*a.DeviceID)
	}
	return "tool:" + call.Tool
}

func deviceTarget(id string) string { return "device:" + id }

// cacheKey 是本次调用的 cache:mcp 键（按租户 + 农场隔离），读写共用。
func (c *callState) cacheKey(k Keys) string {
	return k.CacheMCP(c.call.Tool, c.scope.TenantID, c.scope.FarmID, c.digest)
}

// bypassCache 见 Bypass（与评测夹具同一判定）。
func (c *callState) bypassCache() bool { return Bypass(c.call, c.timeSense) }

// cacheEntry 是 cache:mcp 的值：标准化 Result 与写入时刻（正常读按写入时刻判新鲜期，回落按 collected_at 判陈旧度）。
type cacheEntry struct {
	Result
	CachedAt time.Time `json:"cached_at"`
}

func (g *Gateway) cacheGet(ctx context.Context, key string) (cacheEntry, bool) {
	b, ok, err := g.opt.Cache.Get(ctx, key)
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache get failed", "key", key, "err", err)
		return cacheEntry{}, false
	}
	if !ok {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache entry undecodable", "key", key, "err", err)
		return cacheEntry{}, false
	}
	return e, true
}

// fresh 表示缓存值写入后未超过新鲜期（缓存 TTL 已延长到 max_staleness，正常读仍只认新鲜期内的值）。
func (g *Gateway) fresh(e cacheEntry, ttl time.Duration) bool {
	return !e.CachedAt.IsZero() && g.opt.Clock.Now().Sub(e.CachedAt) <= ttl
}

// fallback：调用 TIMEOUT / UPSTREAM 后读同一 cache:mcp 键，now - collected_at ≤ max_staleness 才回落：
// source 改为 cache、collected_at 保留原采集时间、data_freshness_sec 按当前时钟重算（即 staleness）。
// 无缓存、采集时间缺失或超限时返回原失败（由 graph 按数据缺失拒答）。
func (g *Gateway) fallback(ctx context.Context, c *callState, key string, failure outcome) outcome {
	e, ok := g.cacheGet(ctx, key)
	if !ok {
		g.logFallback(ctx, c, failure, "no cache entry", 0)
		return failure
	}
	at, err := time.Parse(time.RFC3339, e.CollectedAt)
	if err != nil {
		g.logFallback(ctx, c, failure, "cache entry has no collected_at", 0)
		return failure
	}
	age, ok := g.cache.FallbackAge(c.tool.server.Name(), c.tool.spec, c.call, g.opt.Clock.Now(), at)
	if !ok {
		g.logFallback(ctx, c, failure, "cache older than max_staleness", age)
		return failure
	}
	res := e.Result
	res.Source, res.DataFreshnessSec = model.ToolSourceCache, int(age/time.Second)
	g.logFallback(ctx, c, failure, "", age)
	return outcome{res: res, from: fromFallback}
}

// logFallback 记录回落结果（reason 为空表示已回落）；告警名取故障矩阵 MCP_TIMEOUT 的告警项由 L7 接入。
func (g *Gateway) logFallback(ctx context.Context, c *callState, failure outcome, reason string, age time.Duration) {
	args := []any{"tool", c.call.Tool, "run_id", c.call.RunID, "step_id", c.call.StepID, "error_code", failure.code,
		"fault", model.FaultMCPTimeout, "staleness_sec", int(age / time.Second), "max_staleness", c.tool.maxStale}
	if reason == "" {
		g.opt.Logger.WarnContext(ctx, "mcp cache fallback", args...)
		return
	}
	g.opt.Logger.WarnContext(ctx, "mcp cache fallback unavailable", append(args, "reason", reason)...)
}

// cachePut 写缓存，TTL 为 max_staleness（正常读另按新鲜期判断）。
func (g *Gateway) cachePut(ctx context.Context, c *callState, res Result) {
	b, err := json.Marshal(cacheEntry{Result: res, CachedAt: g.opt.Clock.Now()})
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache encode failed", "tool", c.call.Tool, "err", err)
		return
	}
	key := c.cacheKey(g.opt.Keys)
	if err := g.opt.Cache.Set(ctx, key, b, c.tool.maxStale); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache set failed", "key", key, "err", err)
	}
}
