package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
)

const (
	// toolNameMax 是 tool_call.tool_name varchar(64)；未注册的超长工具名截断后落行。
	toolNameMax = 64
	// logSummaryMax 是 trace 日志里出参摘要的最大字节数。
	logSummaryMax  = 512
	rawContentType = "application/json"
	rawNamePrefix  = "tool-"
)

// failureSummary 是失败结果的 result_summary。
type failureSummary struct {
	ErrorCode ErrorCode `json:"error_code"`
	Message   string    `json:"message"`
}

// finish 是流程 4：policy.After → RawStore.Put（仅 OK）→ ToolCallStore.Save → 改写 / 删除 idem 键 → 回写缓存 → trace。
func (g *Gateway) finish(ctx context.Context, c *callState, o outcome) (agent.ToolResult, error) {
	o = g.after(ctx, c, o)
	status := statusOf(o.code)
	summary, err := g.summary(o)
	if err != nil {
		g.release(ctx, c)
		return agent.ToolResult{}, fmt.Errorf("mcp %s: %w", c.call.Tool, err)
	}
	row := model.ToolCallRow{
		TenantID: c.scope.TenantID, FarmID: c.scope.FarmID, RunID: c.call.RunID, StepID: c.call.StepID,
		CallKey: c.callKey, Epoch: c.epoch, ToolName: truncate(c.call.Tool, toolNameMax), ArgsDigest: c.digest,
		Status: status, ErrorCode: string(o.code), TimeSensitive: c.timeSense, ResultSummary: summary,
		CreatedAt: c.start,
	}
	if c.tool != nil {
		row.PolicyLevel = string(c.tool.level)
	}
	if o.code == "" {
		fresh := o.res.DataFreshnessSec
		row.Source, row.DataFreshnessSec = o.res.Source, &fresh
		row.RawObjectID = g.putRaw(ctx, c, o.res)
	}
	row.LatencyMS = int(g.opt.Clock.Now().Sub(c.start) / time.Millisecond)
	id, err := g.opt.Calls.Save(ctx, row)
	if err != nil {
		g.release(ctx, c)
		return agent.ToolResult{}, fmt.Errorf("mcp %s: save tool_call: %w", c.call.Tool, err)
	}
	if o.code == "" {
		if c.claim.held() {
			g.markDone(ctx, c.claim, id)
		}
		if c.tool.cacheTTL > 0 && o.from == fromInvoke {
			g.cachePut(ctx, c, o.res)
		}
	} else {
		g.release(ctx, c)
	}
	res := agent.ToolResult{ToolCallID: id, Status: status, ErrorCode: string(o.code)}
	if o.code == "" {
		res.Source, res.DataFreshnessSec, res.Data = o.res.Source, o.res.DataFreshnessSec, o.res.Data
	}
	g.trace(ctx, c, res, summary, row.LatencyMS, o)
	return res, nil
}

// after 是 policy.After：OK 缺数据源降为 MALFORMED，失败缺错误码补 UPSTREAM。
func (g *Gateway) after(ctx context.Context, c *callState, o outcome) outcome {
	got, reason := g.policy.After(c.call.Tool, policy.Outcome{Status: statusOf(o.code), ErrorCode: string(o.code), Source: o.res.Source})
	if reason == "" {
		return o
	}
	g.opt.Logger.WarnContext(ctx, "mcp policy after corrected result", "tool", c.call.Tool, "reason", reason)
	return failed(ErrorCode(got.ErrorCode), o.from, "%s", reason)
}

// summary 生成脱敏后的 result_summary：OK 为标准化 Result，失败为错误码 + 信息。
func (g *Gateway) summary(o outcome) (json.RawMessage, error) {
	var v any = o.res
	if o.code != "" {
		v = failureSummary{ErrorCode: o.code, Message: o.msg}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode result_summary: %w", err)
	}
	return g.mask(b), nil
}

// putRaw 把原始返回写 MinIO（tool-{call_key}）；失败只记 warn，raw_object_id 留 NULL，不让调用失败。
func (g *Gateway) putRaw(ctx context.Context, c *callState, res Result) *uint64 {
	body, err := json.Marshal(res)
	if err == nil {
		var obj model.RawObject
		obj, err = g.opt.Raw.Put(ctx, model.RawPut{TenantID: c.scope.TenantID, FarmID: c.scope.FarmID, RunID: c.call.RunID,
			Kind: model.RawToolRaw, Name: rawNamePrefix + c.callKey, At: c.start, Body: body, ContentType: rawContentType})
		if err == nil {
			return &obj.ID
		}
	}
	g.opt.Logger.WarnContext(ctx, "mcp raw put failed", "tool", c.call.Tool, "call_key", c.callKey, "err", err)
	return nil
}

// replay 返回去重命中的 OK 行：结果取自 raw 对象（未脱敏的标准化 Result；未配置 Mask 时 result_summary 即原值），
// 新鲜度按 collected_at 相对当前时钟重算；trace 仍只写脱敏摘要。取不回原值时返回 false，由调用方重做，
// 不把脱敏后的数据交给模型。
func (g *Gateway) replay(ctx context.Context, c *callState, hit model.ToolCallHit) (agent.ToolResult, bool) {
	body, err := g.original(ctx, c, hit)
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp dedup hit not replayable, invoke again", "tool", c.call.Tool,
			"tool_call_id", hit.ID, "err", err)
		return agent.ToolResult{}, false
	}
	var r Result
	if err := json.Unmarshal(body, &r); err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp dedup result undecodable, invoke again", "tool", c.call.Tool,
			"tool_call_id", hit.ID, "err", err)
		return agent.ToolResult{}, false
	}
	res := agent.ToolResult{ToolCallID: hit.ID, Status: model.ToolStatusOK, Source: r.Source, Data: r.Data,
		DataFreshnessSec: r.DataFreshnessSec}
	if at, err := time.Parse(time.RFC3339, r.CollectedAt); err == nil {
		if d := g.opt.Clock.Now().Sub(at); d > 0 {
			res.DataFreshnessSec = int(d / time.Second)
		}
	}
	latency := int(g.opt.Clock.Now().Sub(c.start) / time.Millisecond)
	g.trace(ctx, c, res, hit.ResultSummary, latency, outcome{from: fromDedup})
	return res, true
}

// errNoRaw 表示命中行没有 raw_object_id（写 raw 失败），且 result_summary 已脱敏不能当原值。
var errNoRaw = errors.New("hit has no raw object and result_summary is masked")

// original 取命中行的原始 Result JSON：有 raw 对象读对象；没有时仅在未配置 Mask 时用 result_summary。
func (g *Gateway) original(ctx context.Context, c *callState, hit model.ToolCallHit) ([]byte, error) {
	if hit.RawObjectID != nil && g.opt.RawReader != nil {
		body, err := g.opt.RawReader.Get(ctx, c.scope, *hit.RawObjectID)
		if err != nil {
			return nil, fmt.Errorf("read raw object %d: %w", *hit.RawObjectID, err)
		}
		return body, nil
	}
	if g.opt.Mask == nil {
		return hit.ResultSummary, nil
	}
	return nil, errNoRaw
}

// trace 是验收 3：每次调用输出 tool_call_id / 入参摘要 / 出参摘要 / 耗时 / 数据源，并发 TOOL_CALL_END。
func (g *Gateway) trace(ctx context.Context, c *callState, res agent.ToolResult, summary json.RawMessage, latencyMS int, o outcome) {
	g.opt.Logger.InfoContext(ctx, "tool_call",
		"tool_call_id", res.ToolCallID, "tool", c.call.Tool, "run_id", c.call.RunID, "step_id", c.call.StepID,
		"args_digest", c.digest, "result_summary", truncate(string(summary), logSummaryMax), "latency_ms", latencyMS,
		"source", res.Source, "status", res.Status, "error_code", res.ErrorCode, "from", o.from)
	if g.opt.Events == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{"tool": c.call.Tool, "status": res.Status, "error_code": res.ErrorCode,
		"source": res.Source, "latency_ms": latencyMS, "args_digest": c.digest, "from": o.from})
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp event encode failed", "err", err)
		return
	}
	id := res.ToolCallID
	_, err = g.opt.Events.Emit(ctx, model.RunEvent{TenantID: c.scope.TenantID, FarmID: c.scope.FarmID, RunID: c.call.RunID,
		StepID: c.call.StepID, ToolCallID: &id, Type: model.EventToolCallEnd, Actor: model.ActorAgent,
		Payload: payload, CreatedAt: g.opt.Clock.Now()})
	if err != nil {
		g.opt.Logger.WarnContext(ctx, "mcp emit TOOL_CALL_END failed", "tool_call_id", id, "err", err)
	}
}

// truncate 按字符截断到最多 n 字节（不切断 UTF-8 字符）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
