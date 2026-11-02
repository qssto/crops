package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/model"
)

// collectedAge 是 okResult 的 collected_at 相对 t0 的提前量（缓存写入时数据已采集 1 分钟）。
const collectedAge = time.Minute

// last 返回最后写入的 raw 对象正文（未脱敏的标准化 Result）。
func (r *memRaw) last(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.names) == 0 {
		t.Fatal("no raw object written")
	}
	return r.bodies[uint64(100+len(r.names))]
}

func upstreamHandler(context.Context, json.RawMessage, int) (mcp.Result, error) {
	return mcp.Result{}, errors.New("connection refused")
}

func timeoutHandler(ctx context.Context, _ json.RawMessage, _ int) (mcp.Result, error) {
	<-ctx.Done()
	return mcp.Result{}, ctx.Err()
}

// MCP 超时回落（P1 §6.7 技术实现第 3 条）：TIMEOUT / UPSTREAM 后读同一缓存键，now - collected_at ≤ max_staleness
// （天气 1h、作物 24h）才回落，source=cache、collected_at 为原值；恰好等于上限可用，超 1s 拒（应失败：超限仍回落 / 不回落）；
// 无缓存、不可缓存工具（field / device）、epoch>0 的时效调用、下发前复检（ToolCall.Recheck，非时效的作物工具也一样）
// 与 pre_dispatch 选设备一律不回落。
func TestCall_CacheFallback(t *testing.T) {
	weather := agent.ToolCall{Tool: "weather.now", Args: fieldArgs}
	crop := agent.ToolCall{Tool: "crop.info", Args: json.RawMessage(`{"target":"稻瘟病"}`)}
	field := agent.ToolCall{Tool: "field.get", Args: fieldArgs}
	device := agent.ToolCall{Tool: "dev.status", Args: json.RawMessage(`{"device_id":"uav-1"}`)}
	tests := []struct {
		name    string
		warm    bool // 先成功调用一次写缓存
		first   agent.ToolCall
		failing func(context.Context, json.RawMessage, int) (mcp.Result, error)
		age     time.Duration // 回落时 now - collected_at
		step    string
		epoch   int
		recheck bool
		want    mcp.ErrorCode // 空表示回落成功
	}{
		{name: "weather upstream at max_staleness", warm: true, first: weather, failing: upstreamHandler, age: time.Hour},
		{name: "weather upstream 1s over max_staleness", warm: true, first: weather, failing: upstreamHandler,
			age: time.Hour + time.Second, want: mcp.ErrUpstream},
		{name: "weather timeout within max_staleness", warm: true, first: weather, failing: timeoutHandler, age: 20 * time.Minute},
		{name: "crop upstream at max_staleness", warm: true, first: crop, failing: upstreamHandler, age: 24 * time.Hour},
		{name: "crop upstream 1s over", warm: true, first: crop, failing: upstreamHandler, age: 24*time.Hour + time.Second,
			want: mcp.ErrUpstream},
		{name: "no cache entry", first: weather, failing: upstreamHandler, age: 20 * time.Minute, want: mcp.ErrUpstream},
		{name: "time-sensitive epoch>0 no fallback", warm: true, first: weather, failing: upstreamHandler, age: 20 * time.Minute,
			epoch: 1, want: mcp.ErrUpstream},
		{name: "recheck crop no fallback", warm: true, first: crop, failing: upstreamHandler, age: 2 * time.Hour,
			step: "enrich", epoch: 2, recheck: true, want: mcp.ErrUpstream},
		{name: "recheck weather timeout no fallback", warm: true, first: weather, failing: timeoutHandler, age: 20 * time.Minute,
			step: "enrich", recheck: true, want: mcp.ErrTimeout},
		{name: "epoch>0 crop still falls back outside recheck", warm: true, first: crop, failing: upstreamHandler,
			age: 2 * time.Hour, step: "enrich", epoch: 2},
		{name: "pre_dispatch step no fallback", warm: true, first: crop, failing: upstreamHandler, age: 2 * time.Hour,
			step: mcp.PreDispatchStep, want: mcp.ErrUpstream},
		{name: "field not cached", warm: true, first: field, failing: upstreamHandler, age: 20 * time.Minute, want: mcp.ErrUpstream},
		{name: "device never cached", warm: true, first: device, failing: upstreamHandler, age: 20 * time.Minute,
			want: mcp.ErrUpstream},
	}
	// 作物默认新鲜期与 max_staleness 同为 24h（新鲜读总先命中）；测试把新鲜期缩到 1h 以走到回落分支。
	shortCrop := func(o *mcp.Options) {
		o.CacheTTL = map[string]time.Duration{"weather": 10 * time.Minute, "crop": time.Hour}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, shortCrop)
			server := map[string]string{"weather.now": "weather", "crop.info": "crop", "field.get": "field", "dev.status": "dev"}[tt.first.Tool]
			if tt.warm {
				c := tt.first
				c.RunID = 1
				if res := e.call(t, c); res.Status != model.ToolStatusOK {
					t.Fatalf("warm call = %+v", res)
				}
			}
			e.srv[server].handle(tt.first.Tool, tt.failing)
			e.clk.advance(tt.age - collectedAge)
			c := tt.first
			c.RunID, c.StepID, c.Epoch, c.Recheck = 2, tt.step, tt.epoch, tt.recheck
			res := e.call(t, c)
			if tt.want != "" {
				if res.Status == model.ToolStatusOK || res.ErrorCode != string(tt.want) || res.Source != "" {
					t.Fatalf("res = %+v, want %s without fallback", res, tt.want)
				}
				return
			}
			if res.Status != model.ToolStatusOK || res.Source != model.ToolSourceCache ||
				res.DataFreshnessSec != int(tt.age/time.Second) {
				t.Fatalf("res = %+v, want cache fallback with staleness %v", res, tt.age)
			}
			row := e.calls.rows[len(e.calls.rows)-1].row
			if row.Source != model.ToolSourceCache || row.Status != model.ToolStatusOK {
				t.Errorf("tool_call row source=%q status=%s, want cache/OK", row.Source, row.Status)
			}
			var got mcp.Result
			if err := json.Unmarshal(e.raw.last(t), &got); err != nil {
				t.Fatal(err)
			}
			if want := t0.Add(-collectedAge).Format(time.RFC3339); got.CollectedAt != want {
				t.Errorf("collected_at = %q, want original %q", got.CollectedAt, want)
			}
		})
	}
}

// 复检不沿用原 run 的回落结果（P1 §6.7）：同 call_key 的去重命中是 source=cache 时重调 Server——仍失败即返回失败
// （应失败：复检拿到陈旧缓存当实时数据）；Server 恢复则取实时值。非复检的同 key 调用照常复用命中。
func TestCall_RecheckSkipsFallbackReplay(t *testing.T) {
	crop := agent.ToolCall{RunID: 2, StepID: "enrich", Tool: "crop.info", Args: json.RawMessage(`{"target":"稻瘟病"}`)}
	tests := []struct {
		name    string
		recheck bool
		recover bool // 复检前 Server 恢复
		want    string
		source  string
	}{
		{name: "replay for normal call", want: model.ToolStatusOK, source: model.ToolSourceCache},
		{name: "recheck refetches and fails", recheck: true, want: model.ToolStatusError},
		{name: "recheck refetches live value", recheck: true, recover: true, want: model.ToolStatusOK, source: "mock"},
	}
	shortCrop := func(o *mcp.Options) { o.CacheTTL = map[string]time.Duration{"crop": time.Hour} }
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, shortCrop)
			warm := crop
			warm.RunID = 1
			if res := e.call(t, warm); res.Status != model.ToolStatusOK {
				t.Fatalf("warm = %+v", res)
			}
			e.srv["crop"].handle(crop.Tool, upstreamHandler)
			e.clk.advance(2 * time.Hour)
			if res := e.call(t, crop); res.Source != model.ToolSourceCache {
				t.Fatalf("ask call = %+v, want cache fallback", res)
			}
			if tt.recover {
				e.srv["crop"].handle(crop.Tool, okHandler)
			}
			before := e.srv["crop"].count(crop.Tool)
			c := crop
			c.Epoch, c.Recheck = 2, tt.recheck
			res := e.call(t, c)
			if res.Status != tt.want || res.Source != tt.source {
				t.Fatalf("res = %+v, want status %s source %q", res, tt.want, tt.source)
			}
			if invoked := e.srv["crop"].count(crop.Tool) > before; invoked != tt.recheck {
				t.Fatalf("server invoked = %t, want %t", invoked, tt.recheck)
			}
		})
	}
}

// 只配置部分 server 的时间窗时，其余可缓存 server 仍用默认窗（应失败：只配 weather 时 crop 悄悄失去缓存与回落）。
func TestNewCacheRule_PartialOverridesKeepDefaults(t *testing.T) {
	tests := []struct {
		name               string
		fresh, maxStale    map[string]time.Duration
		server             string
		wantFresh, wantMax time.Duration
	}{
		{name: "defaults", server: "crop", wantFresh: 24 * time.Hour, wantMax: 24 * time.Hour},
		{name: "weather only keeps crop", fresh: map[string]time.Duration{"weather": 5 * time.Minute},
			maxStale: map[string]time.Duration{"weather": 30 * time.Minute}, server: "crop",
			wantFresh: 24 * time.Hour, wantMax: 24 * time.Hour},
		{name: "weather override applied", fresh: map[string]time.Duration{"weather": 5 * time.Minute},
			maxStale: map[string]time.Duration{"weather": 30 * time.Minute}, server: "weather",
			wantFresh: 5 * time.Minute, wantMax: 30 * time.Minute},
		{name: "fresh only keeps default max", fresh: map[string]time.Duration{"crop": time.Hour}, server: "crop",
			wantFresh: time.Hour, wantMax: 24 * time.Hour},
		{name: "new server max defaults to fresh", fresh: map[string]time.Duration{"soil": time.Hour}, server: "soil",
			wantFresh: time.Hour, wantMax: time.Hour},
		{name: "field never cached", server: "field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := mcp.NewCacheRule(tt.fresh, tt.maxStale)
			if err != nil {
				t.Fatal(err)
			}
			if f, m := r.Window(tt.server); f != tt.wantFresh || m != tt.wantMax {
				t.Fatalf("Window(%s) = %v/%v, want %v/%v", tt.server, f, m, tt.wantFresh, tt.wantMax)
			}
		})
	}
	if _, err := mcp.NewCacheRule(nil, map[string]time.Duration{"crop": time.Hour}); err == nil {
		t.Fatal("crop max_staleness 1h < default fresh 24h must be rejected")
	}
}

// 网关按合并后的时间窗工作：只配 weather 时作物工具仍写缓存（TTL 24h）并能回落。
func TestNewGateway_PartialCacheWindows(t *testing.T) {
	e := newEnv(t, func(o *mcp.Options) {
		o.CacheTTL = map[string]time.Duration{"weather": 5 * time.Minute}
		o.MaxStaleness = map[string]time.Duration{"weather": 30 * time.Minute}
	})
	c := agent.ToolCall{RunID: 1, StepID: "enrich", Tool: "crop.info", Args: json.RawMessage(`{"target":"稻瘟病"}`)}
	e.call(t, c)
	key := e.keys.CacheMCP(c.Tool, scope.TenantID, scope.FarmID, mcp.ArgsDigest(mcp.Canonical(c.Args)))
	if got := e.cache.ttl[key]; got != 24*time.Hour {
		t.Fatalf("crop cache ttl = %v, want default 24h", got)
	}
}

// 缓存写入 TTL 延长到 max_staleness 后，正常读仍只认新鲜期（天气 10min）：超出即重新调用（应失败：陈旧值被当新鲜值返回）。
func TestCall_CacheFreshWindowUnchanged(t *testing.T) {
	e := newEnv(t, nil)
	c := agent.ToolCall{RunID: 1, Tool: "weather.now", Args: fieldArgs}
	e.call(t, c)
	e.clk.advance(10 * time.Minute)
	c.RunID = 2
	if res := e.call(t, c); e.srv["weather"].count("weather.now") != 1 || res.Source == model.ToolSourceCache {
		t.Fatalf("within fresh window: invocations = %d source = %q, want cache hit", e.srv["weather"].count("weather.now"), res.Source)
	}
	e.clk.advance(time.Second)
	c.RunID = 3
	if res := e.call(t, c); e.srv["weather"].count("weather.now") != 2 || res.Source != "mock" {
		t.Fatalf("after fresh window: invocations = %d source = %q, want re-invoke", e.srv["weather"].count("weather.now"), res.Source)
	}
}

// max_staleness 小于新鲜期即构造失败。
func TestNewGateway_MaxStalenessBelowTTL(t *testing.T) {
	e := newEnv(t, nil)
	_, err := mcp.NewGateway([]mcp.Server{e.srv["weather"], e.srv["crop"], e.srv["field"], e.srv["dev"]}, mcp.Options{
		Clock: e.clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Keys: e.keys,
		ToolPolicy: toolPolicy(t, testToolPolicy), Idem: newMemIdem(), Cache: newMemCache(), Calls: &memCalls{}, Raw: &memRaw{},
		Fields: e.fields, Devices: e.devices, MaxStaleness: map[string]time.Duration{"weather": time.Minute},
	})
	if err == nil || !strings.Contains(err.Error(), "max_staleness") {
		t.Fatalf("err = %v, want max_staleness below cache ttl rejected", err)
	}
}

// 网关错误码与故障矩阵一致：TIMEOUT / UPSTREAM 都归 MCP_TIMEOUT（policy 包不反向依赖 mcp，一致性在此断言）。
func TestFaultMatrix_ToolErrorCodes(t *testing.T) {
	m, err := policy.LoadFaultMatrix("../../../../policy/fault_matrix.yaml", "../../../../configs/texts/zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []mcp.ErrorCode{mcp.ErrTimeout, mcp.ErrUpstream} {
		if f, ok := m.ForToolError(string(code)); !ok || f.Code != model.FaultMCPTimeout {
			t.Errorf("ForToolError(%s) = %v, %v", code, f.Code, ok)
		}
	}
	for _, code := range []mcp.ErrorCode{mcp.ErrEmpty, mcp.ErrMalformed, mcp.ErrRejected, mcp.ErrScopeDenied} {
		if _, ok := m.ForToolError(string(code)); ok {
			t.Errorf("ForToolError(%s) must not map to a fault", code)
		}
	}
}
