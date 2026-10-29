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
	"agri-steward/internal/repository/rediskey"
)

const (
	fieldInput  = `{"type":"object","additionalProperties":false,"required":["field_id"],"properties":{"field_id":{"type":"integer","minimum":1}}}`
	targetInput = `{"type":"object","additionalProperties":false,"properties":{"target":{"type":"string"}}}`
	vOutput     = `{"type":"object","required":["v"],"properties":{"v":{"type":"integer"}}}`
	testTimeout = 20 * time.Millisecond
)

var (
	t0        = time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	scope     = model.Scope{TenantID: 1, FarmID: 1, UserID: 1001}
	fieldArgs = json.RawMessage(`{"field_id":1}`)
)

// testToolPolicy 是 newEnv 注册表对应的 tool_policy；dev.dispatch 是不注册的 actuate 工具。
const testToolPolicy = `version: 1
tools:
  - {name: weather.now, level: read}
  - {name: crop.info, level: read}
  - {name: field.get, level: read}
  - {name: dev.write, level: write}
  - {name: dev.once, level: read}
  - {name: dev.dispatch, level: actuate}
`

func toolPolicy(t *testing.T, src string) *policy.ToolPolicy {
	t.Helper()
	tp, err := policy.ParseToolPolicy([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func spec(name, input string, level mcp.Level, f func(*mcp.ToolSpec)) mcp.ToolSpec {
	s := mcp.ToolSpec{Name: name, InputSchema: json.RawMessage(input), OutputSchema: json.RawMessage(vOutput), Level: level}
	if f != nil {
		f(&s)
	}
	return s
}

func okResult(v int) mcp.Result {
	return mcp.Result{Data: json.RawMessage(`{"v":` + string(rune('0'+v)) + `}`), Source: "mock",
		DataFreshnessSec: 60, CollectedAt: t0.Add(-time.Minute).Format(time.RFC3339)}
}

func okHandler(context.Context, json.RawMessage, int) (mcp.Result, error) { return okResult(1), nil }

// testEnv 是一套 gateway + 内存依赖。servers：weather（时效、缓存）、crop（缓存，无 field_id）、
// field（不缓存）、dev（write / 非幂等 read）。
type testEnv struct {
	gw     *mcp.Gateway
	srv    map[string]*fakeServer
	idem   *memIdem
	cache  *memCache
	calls  *memCalls
	raw    *memRaw
	events *memEvents
	clk    *fakeClock
	keys   rediskey.Keys
}

func newEnv(t *testing.T, mod func(*mcp.Options)) *testEnv {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, specs ...mcp.ToolSpec) *fakeServer {
		s := &fakeServer{name: name, specs: specs, handlers: map[string]handler{}}
		for _, sp := range specs {
			s.handlers[sp.Name] = okHandler
		}
		return s
	}
	idem := func(s *mcp.ToolSpec) { s.Idempotent = true }
	e := &testEnv{
		srv: map[string]*fakeServer{
			"weather": mk("weather", spec("weather.now", fieldInput, mcp.LevelRead, func(s *mcp.ToolSpec) {
				s.Idempotent, s.Cacheable, s.TimeSensitive = true, true, true
			})),
			"crop": mk("crop", spec("crop.info", targetInput, mcp.LevelRead, func(s *mcp.ToolSpec) {
				s.Idempotent, s.Cacheable = true, true
			})),
			"field": mk("field", spec("field.get", fieldInput, mcp.LevelRead, idem)),
			"dev":   mk("dev", spec("dev.write", fieldInput, mcp.LevelWrite, nil), spec("dev.once", fieldInput, mcp.LevelRead, nil)),
		},
		idem: newMemIdem(), cache: newMemCache(), calls: &memCalls{}, raw: &memRaw{}, events: &memEvents{},
		clk: &fakeClock{now: t0}, keys: keys,
	}
	opt := mcp.Options{
		Clock: e.clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Keys: keys, ToolPolicy: toolPolicy(t, testToolPolicy),
		Idem: e.idem, Cache: e.cache, Calls: e.calls, Raw: e.raw, Events: e.events,
		Fields:      memFields{owner: map[uint64][2]uint64{1: {1, 1}, 2: {2, 1}, 3: {1, 2}}},
		Timeouts:    map[string]time.Duration{"weather": testTimeout, "crop": testTimeout, "field": testTimeout, "dev": testTimeout},
		PendingPoll: time.Millisecond,
	}
	if mod != nil {
		mod(&opt)
	}
	if opt.Mask != nil && opt.RawReader == nil { // 生产配了 Mask 就必须配 RawReader；测试默认读本 env 的 memRaw
		opt.RawReader = e.raw
	}
	srvs := []mcp.Server{e.srv["weather"], e.srv["crop"], e.srv["field"], e.srv["dev"]}
	if e.gw, err = mcp.NewGateway(srvs, opt); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *testEnv) call(t *testing.T, c agent.ToolCall) agent.ToolResult {
	t.Helper()
	if c.StepID == "" {
		c.StepID = "enrich"
	}
	if c.RunID == 0 {
		c.RunID = 7
	}
	res, err := e.gw.Call(context.Background(), scope, c)
	if err != nil {
		t.Fatalf("Call %s: %v", c.Tool, err)
	}
	return res
}

func (e *testEnv) idemKey(c agent.ToolCall, epoch int) string {
	return e.keys.IdemTool(mcp.CallKey(c.RunID, c.StepID, c.Tool, mcp.Canonical(c.Args), c.CallSeq, epoch))
}

func TestNewGateway_Errors(t *testing.T) {
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	full := mcp.Options{Clock: &fakeClock{}, Logger: slog.Default(), Keys: keys, Idem: newMemIdem(), Cache: newMemCache(),
		Calls: &memCalls{}, Raw: &memRaw{}, Fields: memFields{},
		ToolPolicy: toolPolicy(t, "version: 1\ntools:\n  - {name: a.x, level: read}\n  - {name: a.act, level: actuate}\n")}
	a := &fakeServer{name: "a", specs: []mcp.ToolSpec{spec("a.x", fieldInput, mcp.LevelRead, nil)}}
	with := func(specs ...mcp.ToolSpec) []mcp.Server { return []mcp.Server{&fakeServer{name: "a", specs: specs}} }
	levels := full
	levels.Levels = map[string]mcp.Level{"a": mcp.LevelWrite}
	noPolicy := full
	noPolicy.ToolPolicy = nil
	tests := []struct {
		name string
		srv  []mcp.Server
		opt  mcp.Options
		want string
	}{
		{"duplicate tool", []mcp.Server{a, &fakeServer{name: "b", specs: a.specs}}, full, "duplicate tool"},
		{"bad input schema", []mcp.Server{&fakeServer{name: "a", specs: []mcp.ToolSpec{spec("a.x", `{"type":`, mcp.LevelRead, nil)}}},
			full, "not JSON"},
		{"missing deps", []mcp.Server{a}, mcp.Options{}, "Clock is required"},
		{"missing tool policy", []mcp.Server{a}, noPolicy, "ToolPolicy is required"},
		// 验收 1：启动期自检，任一不符即构造失败。
		{"tool not declared in policy", with(spec("a.x", fieldInput, mcp.LevelRead, nil), spec("a.new", fieldInput, mcp.LevelRead, nil)),
			full, "a.new is registered but not declared"},
		{"registry level empty", with(spec("a.x", fieldInput, "", nil)), full, "a.x has no level in the registry"},
		{"level mismatch via Levels override", []mcp.Server{a}, levels, "a.x is write in registry, read in tool_policy"},
		{"actuate visible to model", with(spec("a.x", fieldInput, mcp.LevelRead, nil), spec("a.act", fieldInput, mcp.LevelActuate, nil)),
			full, "actuate tool is visible"},
		{"policy entry not registered", with(), full, "declared tool not in registry: a.x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mcp.NewGateway(tt.srv, tt.opt)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want contains %q", err, tt.want)
			}
		})
	}
}

func TestCall_PolicyBeforeRejects(t *testing.T) {
	tests := []struct {
		name, tool, args string
	}{
		{"undeclared tool", "weather.nope", `{"field_id":1}`},
		{"actuate reserved in policy", "dev.dispatch", `{"field_id":1}`},
		{"write level denied", "dev.write", `{"field_id":1}`},
		{"schema type mismatch", "weather.now", `{"field_id":"1"}`},
		{"schema missing required", "weather.now", `{}`},
		{"schema unknown property", "weather.now", `{"field_id":1,"lat":30.1}`},
		{"args not object", "weather.now", `[1]`},
		{"args not json", "weather.now", `{field_id:1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			c := agent.ToolCall{Tool: tt.tool, Args: json.RawMessage(tt.args)}
			res := e.call(t, c)
			if res.Status != model.ToolStatusRejected || res.ErrorCode != string(mcp.ErrRejected) || res.ToolCallID == 0 {
				t.Fatalf("res = %+v, want REJECTED with tool_call_id", res)
			}
			for _, s := range e.srv {
				if n := s.count(tt.tool); n != 0 {
					t.Fatalf("server invoked %d times, want 0", n)
				}
			}
			if row := e.calls.last().row; row.Status != model.ToolStatusRejected || row.ToolName != tt.tool {
				t.Fatalf("row = %+v", row)
			}
		})
	}
}

func TestCall_LevelsOverride(t *testing.T) {
	e := newEnv(t, func(o *mcp.Options) {
		o.Levels = map[string]mcp.Level{"dev": mcp.LevelRead}
		o.ToolPolicy = toolPolicy(t, strings.Replace(testToolPolicy, "{name: dev.write, level: write}", "{name: dev.write, level: read}", 1))
	})
	if res := e.call(t, agent.ToolCall{Tool: "dev.write", Args: fieldArgs}); res.Status != model.ToolStatusOK {
		t.Fatalf("res = %+v, want OK after level override", res)
	}
	if lv := e.calls.last().row.PolicyLevel; lv != "read" {
		t.Fatalf("policy_level = %q", lv)
	}
}

// TestCall_TenantIsolation 是租户隔离红线：field_id 不属于 scope 时 REJECTED 且不调 Server。
func TestCall_TenantIsolation(t *testing.T) {
	tests := []struct {
		name, tool, args string
		want             string
	}{
		{"own field", "weather.now", `{"field_id":1}`, model.ToolStatusOK},
		{"other tenant", "weather.now", `{"field_id":2}`, model.ToolStatusRejected},
		{"other farm same tenant", "field.get", `{"field_id":3}`, model.ToolStatusRejected},
		{"unknown field", "field.get", `{"field_id":99}`, model.ToolStatusRejected},
		{"no field arg", "crop.info", `{"target":"稻飞虱"}`, model.ToolStatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			res := e.call(t, agent.ToolCall{Tool: tt.tool, Args: json.RawMessage(tt.args)})
			if res.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.ErrorCode, tt.want)
			}
			server, _, _ := strings.Cut(tt.tool, ".")
			wantCalls := 0
			if tt.want == model.ToolStatusOK {
				wantCalls = 1
			}
			if n := e.srv[server].count(tt.tool); n != wantCalls {
				t.Fatalf("server invoked %d times, want %d", n, wantCalls)
			}
		})
	}
}

func TestCall_TenantCheckError(t *testing.T) {
	e := newEnv(t, func(o *mcp.Options) { o.Fields = memFields{err: errors.New("db down")} })
	_, err := e.gw.Call(context.Background(), scope, agent.ToolCall{RunID: 1, StepID: "s", Tool: "field.get", Args: fieldArgs})
	if err == nil || e.srv["field"].count("field.get") != 0 {
		t.Fatalf("err = %v, server calls %d; want error and no call", err, e.srv["field"].count("field.get"))
	}
}

func blockUntilDone(ctx context.Context, _ json.RawMessage, _ int) (mcp.Result, error) {
	<-ctx.Done()
	return mcp.Result{}, ctx.Err()
}

func TestCall_RetryAndNormalize(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		h         handler
		status    string
		code      mcp.ErrorCode
		wantCalls int
	}{
		{"timeout idempotent read retries 2", "weather.now", blockUntilDone, model.ToolStatusTimeout, mcp.ErrTimeout, 3},
		{"timeout non-idempotent no retry", "dev.once", blockUntilDone, model.ToolStatusTimeout, mcp.ErrTimeout, 1},
		{"upstream retried", "field.get", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{}, errors.New("connection reset")
		}, model.ToolStatusError, mcp.ErrUpstream, 3},
		{"upstream then ok", "field.get", func(_ context.Context, _ json.RawMessage, n int) (mcp.Result, error) {
			if n == 1 {
				return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrUpstream, Msg: "503"}
			}
			return okResult(1), nil
		}, model.ToolStatusOK, "", 2},
		{"empty not retried", "weather.now", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrEmpty, Msg: "no data"}
		}, model.ToolStatusEmpty, mcp.ErrEmpty, 1},
		{"rejected by server", "weather.now", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrRejected, Msg: "bad date"}
		}, model.ToolStatusRejected, mcp.ErrRejected, 1},
		{"data not object", "weather.now", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{Data: json.RawMessage(`[1,2]`), Source: "mock"}, nil
		}, model.ToolStatusError, mcp.ErrMalformed, 1},
		{"data not json", "weather.now", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{Data: json.RawMessage(`{"v":`), Source: "mock"}, nil
		}, model.ToolStatusError, mcp.ErrMalformed, 1},
		{"data violates output schema", "weather.now", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{Data: json.RawMessage(`{"v":"x"}`), Source: "mock"}, nil
		}, model.ToolStatusError, mcp.ErrMalformed, 1},
		{"ok without source (policy.After)", "weather.now", func(context.Context, json.RawMessage, int) (mcp.Result, error) {
			return mcp.Result{Data: json.RawMessage(`{"v":1}`)}, nil
		}, model.ToolStatusError, mcp.ErrMalformed, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			server, _, _ := strings.Cut(tt.tool, ".")
			e.srv[server].handle(tt.tool, tt.h)
			c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: tt.tool, Args: fieldArgs}
			res := e.call(t, c)
			if res.Status != tt.status || res.ErrorCode != string(tt.code) {
				t.Fatalf("res = %+v, want %s / %s", res, tt.status, tt.code)
			}
			if n := e.srv[server].count(tt.tool); n != tt.wantCalls {
				t.Fatalf("invocations = %d, want %d", n, tt.wantCalls)
			}
			row := e.calls.last().row
			if row.Status != tt.status || row.ErrorCode != string(tt.code) || len(row.ResultSummary) == 0 {
				t.Fatalf("row = %+v", row)
			}
			v, held := e.idem.value(e.idemKey(c, 0))
			if tt.code != "" && held {
				t.Fatalf("failed call left idem key %q, want deleted", v)
			}
			if tt.code == "" && !strings.HasPrefix(v, "DONE:") {
				t.Fatalf("idem = %q, want DONE:", v)
			}
		})
	}
}

// TestCall_ActuateAlert：来自模型的 actuate 调用 REJECTED、不调 Server，并以 ERROR 级别告警；其他拒绝不告警。
func TestCall_ActuateAlert(t *testing.T) {
	var buf strings.Builder
	e := newEnv(t, func(o *mcp.Options) { o.Logger = slog.New(slog.NewJSONHandler(&buf, nil)) })

	if res := e.call(t, agent.ToolCall{Tool: "dev.write", Args: fieldArgs}); res.Status != model.ToolStatusRejected {
		t.Fatalf("write res = %+v", res)
	}
	if strings.Contains(buf.String(), "ACTUATE_FROM_AGENT") {
		t.Fatalf("write rejection must not raise actuate alert: %s", buf.String())
	}
	res := e.call(t, agent.ToolCall{Tool: "dev.dispatch", Args: fieldArgs})
	if res.Status != model.ToolStatusRejected || res.ErrorCode != string(mcp.ErrRejected) {
		t.Fatalf("actuate res = %+v", res)
	}
	var alert map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["alert"] == "ACTUATE_FROM_AGENT" {
			alert = m
		}
	}
	if alert == nil || alert["level"] != "ERROR" || alert["tool"] != "dev.dispatch" || alert["run_id"] != float64(7) {
		t.Fatalf("alert = %v, logs:\n%s", alert, buf.String())
	}
}
