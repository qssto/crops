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
	fromDedup  = "dedup"
	fromPolicy = "policy"
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

// alertActuate 以 ERROR 级别记录来自模型的 actuate 调用（兜底拒绝已由 policy.Before 完成，这里只告警）。
func (g *Gateway) alertActuate(ctx context.Context, scope model.Scope, call agent.ToolCall, err error) {
	g.opt.Logger.ErrorContext(ctx, "mcp actuate call from agent rejected",
		"alert", alertActuateFromAgent, "tool", call.Tool, "tenant_id", scope.TenantID, "farm_id", scope.FarmID,
		"run_id", call.RunID, "step_id", call.StepID, "err", err)
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

// fetch 先读缓存（时效工具 epoch>0 与 pre_dispatch 复检不读），未命中再调 Server；error 只表示 ctx 取消。
// Server 一律拿到带 Scope 的 ctx（mcp.WithScope）；返回 SCOPE_DENIED 时补写越界审计。
func (g *Gateway) fetch(ctx context.Context, c *callState) (outcome, error) {
	t := c.tool
	key := c.cacheKey(g.opt.Keys)
	if t.cacheTTL > 0 && !c.bypassCache() {
		if res, ok := g.cacheGet(ctx, key); ok {
			return outcome{res: res, from: fromCache}, nil
		}
	}
	o, err := g.invoke(WithScope(ctx, c.scope), t, c.call.Args)
	if err != nil || o.code != ErrScopeDenied {
		return o, err
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
	key := c.cacheKey(g.opt.Keys)
	if err := g.opt.Cache.Set(ctx, key, b, c.tool.cacheTTL); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp cache set failed", "key", key, "err", err)
	}
}
