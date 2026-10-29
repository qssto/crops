package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/model"
)

func TestCall_DoneHitReplaysRow(t *testing.T) {
	e := newEnv(t, nil)
	c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
	first := e.call(t, c)
	e.clk.advance(time.Minute)
	second := e.call(t, agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: json.RawMessage(`{ "field_id" : 1 }`)})
	if n := e.srv["field"].count("field.get"); n != 1 {
		t.Fatalf("invocations = %d, want 1 (DONE hit)", n)
	}
	if second.ToolCallID != first.ToolCallID || second.Status != model.ToolStatusOK || string(second.Data) != string(first.Data) {
		t.Fatalf("second = %+v, first = %+v", second, first)
	}
	if second.DataFreshnessSec != 120 {
		t.Fatalf("freshness = %d, want 120 (collected_at relative to clock)", second.DataFreshnessSec)
	}
	if v, _ := e.idem.value(e.idemKey(c, 0)); v != "DONE:1" {
		t.Fatalf("idem = %q, want DONE:1", v)
	}
	if rows := e.calls.all(); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

func TestCall_RedisLostFallsBackToMySQL(t *testing.T) {
	tests := []struct {
		name string
		lose func(*testEnv)
	}{
		{"key evicted", func(e *testEnv) { e.idem.clear() }},
		{"redis down", func(e *testEnv) { e.idem.fail = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
			first := e.call(t, c)
			tt.lose(e)
			second := e.call(t, c)
			if n := e.srv["field"].count("field.get"); n != 1 || second.ToolCallID != first.ToolCallID {
				t.Fatalf("invocations = %d, ids %d/%d; want 1 call and same id", n, first.ToolCallID, second.ToolCallID)
			}
		})
	}
	t.Run("evicted key rewritten to DONE", func(t *testing.T) {
		e := newEnv(t, nil)
		c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
		e.call(t, c)
		e.idem.clear()
		e.call(t, c)
		if v, _ := e.idem.value(e.idemKey(c, 0)); v != "DONE:1" {
			t.Fatalf("idem = %q, want DONE:1", v)
		}
	})
}

func TestCall_DoneWithoutRowReclaims(t *testing.T) {
	e := newEnv(t, nil)
	c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
	e.idem.set(e.idemKey(c, 0), "DONE:999")
	if res := e.call(t, c); res.Status != model.ToolStatusOK || e.srv["field"].count("field.get") != 1 {
		t.Fatalf("res = %+v, want fresh invoke", res)
	}
}

func TestCall_FailureNotDeduped(t *testing.T) {
	e := newEnv(t, nil)
	e.srv["field"].handle("field.get", func(_ context.Context, _ json.RawMessage, n int) (mcp.Result, error) {
		if n == 1 {
			return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrEmpty, Msg: "no data"}
		}
		return okResult(2), nil
	})
	c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
	if res := e.call(t, c); res.Status != model.ToolStatusEmpty {
		t.Fatalf("first = %+v", res)
	}
	if _, held := e.idem.value(e.idemKey(c, 0)); held {
		t.Fatal("idem key kept after failure")
	}
	if res := e.call(t, c); res.Status != model.ToolStatusOK {
		t.Fatalf("second = %+v", res)
	}
	rows := e.calls.all()
	if len(rows) != 2 || rows[0].attempt != 1 || rows[1].attempt != 2 || rows[0].row.CallKey != rows[1].row.CallKey {
		t.Fatalf("rows = %+v, want attempts 1,2 with same call_key", rows)
	}
}

func TestCall_PendingInFlight(t *testing.T) {
	c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
	t.Run("wait expires, no OK row: invoke without claim", func(t *testing.T) {
		e := newEnv(t, nil)
		key := e.idemKey(c, 0)
		e.idem.set(key, "PENDING:other")
		if res := e.call(t, c); res.Status != model.ToolStatusOK || e.srv["field"].count("field.get") != 1 {
			t.Fatalf("res = %+v", res)
		}
		if v, _ := e.idem.value(key); v != "PENDING:other" {
			t.Fatalf("idem = %q, other owner's claim must stay", v)
		}
	})
	t.Run("wait expires, OK row exists: replay", func(t *testing.T) {
		e := newEnv(t, nil)
		e.idem.set(e.idemKey(c, 0), "PENDING:other")
		ck := mcp.CallKey(c.RunID, c.StepID, c.Tool, mcp.Canonical(c.Args), 0, 0)
		summary, err := json.Marshal(okResult(3))
		if err != nil {
			t.Fatal(err)
		}
		id, err := e.calls.Save(context.Background(), model.ToolCallRow{TenantID: 1, FarmID: 1, CallKey: ck,
			Status: model.ToolStatusOK, ResultSummary: summary})
		if err != nil {
			t.Fatal(err)
		}
		res := e.call(t, c)
		if res.ToolCallID != id || string(res.Data) != `{"v":3}` || e.srv["field"].count("field.get") != 0 {
			t.Fatalf("res = %+v, want replay of row %d", res, id)
		}
	})
	t.Run("in-flight call finishes during wait", func(t *testing.T) {
		e := newEnv(t, func(o *mcp.Options) { o.Timeouts["field"] = time.Second })
		e.srv["field"].handle("field.get", func(ctx context.Context, _ json.RawMessage, _ int) (mcp.Result, error) {
			time.Sleep(30 * time.Millisecond)
			return okResult(1), nil
		})
		done := make(chan agent.ToolResult, 1)
		go func() {
			res, err := e.gw.Call(context.Background(), scope, c)
			if err != nil {
				t.Error(err)
			}
			done <- res
		}()
		time.Sleep(5 * time.Millisecond)
		second := e.call(t, c)
		first := <-done
		if e.srv["field"].count("field.get") != 1 || first.ToolCallID != second.ToolCallID {
			t.Fatalf("invocations = %d, ids %d/%d", e.srv["field"].count("field.get"), first.ToolCallID, second.ToolCallID)
		}
	})
}

func TestCall_Cache(t *testing.T) {
	weather := func(run uint64, step string, epoch int) agent.ToolCall {
		return agent.ToolCall{RunID: run, StepID: step, Tool: "weather.now", Args: fieldArgs, Epoch: epoch}
	}
	crop := func(run uint64, step string) agent.ToolCall {
		return agent.ToolCall{RunID: run, StepID: step, Tool: "crop.info", Args: json.RawMessage(`{"target":"稻瘟病"}`)}
	}
	field := func(run uint64) agent.ToolCall {
		return agent.ToolCall{RunID: run, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
	}
	tests := []struct {
		name      string
		calls     []agent.ToolCall
		tool      string
		wantCalls int
		wantTTL   time.Duration // 0 表示不应有缓存
	}{
		{"weather cached across runs", []agent.ToolCall{weather(1, "enrich", 0), weather(2, "enrich", 0)}, "weather.now", 1, 10 * time.Minute},
		{"crop cached 24h", []agent.ToolCall{crop(1, "enrich"), crop(2, "enrich")}, "crop.info", 1, 24 * time.Hour},
		{"time-sensitive epoch>0 bypasses read", []agent.ToolCall{weather(1, "enrich", 0), weather(1, "enrich", 1)}, "weather.now", 2, 10 * time.Minute},
		{"pre_dispatch bypasses read", []agent.ToolCall{crop(1, "enrich"), crop(1, mcp.PreDispatchStep)}, "crop.info", 2, 24 * time.Hour},
		{"field not cached", []agent.ToolCall{field(1), field(2)}, "field.get", 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			for _, c := range tt.calls {
				if res := e.call(t, c); res.Status != model.ToolStatusOK {
					t.Fatalf("res = %+v", res)
				}
			}
			server, _, _ := strings.Cut(tt.tool, ".")
			if n := e.srv[server].count(tt.tool); n != tt.wantCalls {
				t.Fatalf("invocations = %d, want %d", n, tt.wantCalls)
			}
			if tt.wantTTL == 0 {
				if e.cache.size() != 0 {
					t.Fatal("non-cacheable tool written to cache")
				}
				return
			}
			key := e.keys.CacheMCP(tt.tool, mcp.ArgsDigest(mcp.Canonical(tt.calls[0].Args)))
			if got := e.cache.ttl[key]; got != tt.wantTTL {
				t.Fatalf("cache ttl[%s] = %v, want %v", key, got, tt.wantTTL)
			}
		})
	}
}

func TestCall_EpochOnlyForTimeSensitive(t *testing.T) {
	e := newEnv(t, nil)
	a := e.call(t, agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs, Epoch: 0})
	b := e.call(t, agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs, Epoch: 3})
	if a.ToolCallID != b.ToolCallID {
		t.Fatalf("non-time-sensitive tool must ignore epoch: ids %d/%d", a.ToolCallID, b.ToolCallID)
	}
	w0 := e.call(t, agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "weather.now", Args: fieldArgs, Epoch: 0})
	w3 := e.call(t, agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "weather.now", Args: fieldArgs, Epoch: 3})
	if w0.ToolCallID == w3.ToolCallID {
		t.Fatal("time-sensitive tool must re-fetch on new epoch")
	}
	row := e.calls.last().row
	if row.Epoch != 3 || !row.TimeSensitive {
		t.Fatalf("row = %+v, want epoch 3 time_sensitive", row)
	}
}

func TestCall_CallSeqChangesKey(t *testing.T) {
	e := newEnv(t, nil)
	a := e.call(t, agent.ToolCall{Tool: "field.get", Args: fieldArgs, CallSeq: 1})
	b := e.call(t, agent.ToolCall{Tool: "field.get", Args: fieldArgs, CallSeq: 2})
	if a.ToolCallID == b.ToolCallID || e.srv["field"].count("field.get") != 2 {
		t.Fatalf("call_seq must change call_key: ids %d/%d", a.ToolCallID, b.ToolCallID)
	}
}

func TestCall_RawStore(t *testing.T) {
	t.Run("put ok", func(t *testing.T) {
		e := newEnv(t, nil)
		c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
		e.call(t, c)
		row := e.calls.last().row
		ck := mcp.CallKey(7, "enrich", "field.get", mcp.Canonical(fieldArgs), 0, 0)
		if row.RawObjectID == nil || len(e.raw.names) != 1 || e.raw.names[0] != "tool-"+ck {
			t.Fatalf("raw_object_id = %v, names = %v", row.RawObjectID, e.raw.names)
		}
	})
	t.Run("put failure keeps result", func(t *testing.T) {
		e := newEnv(t, nil)
		e.raw.fail = true
		res := e.call(t, agent.ToolCall{Tool: "field.get", Args: fieldArgs})
		if res.Status != model.ToolStatusOK || e.calls.last().row.RawObjectID != nil {
			t.Fatalf("res = %+v, raw_object_id = %v", res, e.calls.last().row.RawObjectID)
		}
	})
	t.Run("failed call not stored", func(t *testing.T) {
		e := newEnv(t, nil)
		e.call(t, agent.ToolCall{Tool: "field.get", Args: json.RawMessage(`{"field_id":2}`)})
		if len(e.raw.names) != 0 {
			t.Fatalf("raw names = %v, want none", e.raw.names)
		}
	})
}

func TestCall_RowAndEvent(t *testing.T) {
	e := newEnv(t, nil)
	res := e.call(t, agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "weather.now", Args: fieldArgs})
	row := e.calls.last().row
	want := model.ToolCallRow{TenantID: 1, FarmID: 1, RunID: 7, StepID: "enrich", ToolName: "weather.now", PolicyLevel: "read",
		Status: model.ToolStatusOK, Source: "mock", TimeSensitive: true, CreatedAt: t0}
	if row.TenantID != want.TenantID || row.FarmID != want.FarmID || row.RunID != want.RunID || row.StepID != want.StepID ||
		row.ToolName != want.ToolName || row.PolicyLevel != want.PolicyLevel || row.Status != want.Status ||
		row.Source != want.Source || row.TimeSensitive != want.TimeSensitive || !row.CreatedAt.Equal(t0) ||
		len(row.CallKey) != 32 || len(row.ArgsDigest) != 64 || row.DataFreshnessSec == nil || *row.DataFreshnessSec != 60 {
		t.Fatalf("row = %+v", row)
	}
	if len(e.events.evs) != 1 {
		t.Fatalf("events = %d, want 1", len(e.events.evs))
	}
	ev := e.events.evs[0]
	if ev.Type != model.EventToolCallEnd || ev.ToolCallID == nil || *ev.ToolCallID != res.ToolCallID || ev.RunID != 7 {
		t.Fatalf("event = %+v", ev)
	}
}

// maskAll 是改写全部内容的 Mask，便于区分「脱敏摘要」与「原始结果」。
func maskAll(json.RawMessage) json.RawMessage { return json.RawMessage(`{"masked":true}`) }

func TestCall_MaskAppliedToSummary(t *testing.T) {
	e := newEnv(t, func(o *mcp.Options) { o.Mask = maskAll })
	e.call(t, agent.ToolCall{Tool: "field.get", Args: fieldArgs})
	if s := string(e.calls.last().row.ResultSummary); s != `{"masked":true}` {
		t.Fatalf("summary = %s", s)
	}
}

func TestCall_ContextCanceled(t *testing.T) {
	t.Run("before call", func(t *testing.T) {
		e := newEnv(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := e.gw.Call(ctx, scope, agent.ToolCall{Tool: "field.get", Args: fieldArgs}); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("during invoke releases claim", func(t *testing.T) {
		e := newEnv(t, func(o *mcp.Options) { o.Timeouts["field"] = time.Second })
		e.srv["field"].handle("field.get", blockUntilDone)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
		if _, err := e.gw.Call(ctx, scope, c); err == nil {
			t.Fatal("want error")
		}
		if _, held := e.idem.value(e.idemKey(c, 0)); held || len(e.calls.all()) != 0 {
			t.Fatalf("claim kept or row saved after cancel")
		}
	})
}

func TestCanonicalAndKeys(t *testing.T) {
	a := mcp.Canonical(json.RawMessage(`{"b":2, "a":{"y":1,"x":[3, 1.50]}}`))
	b := mcp.Canonical(json.RawMessage(`{"a":{"x":[3,1.50],"y":1},"b":2}`))
	if string(a) != string(b) || string(a) != `{"a":{"x":[3,1.50],"y":1},"b":2}` {
		t.Fatalf("canonical %s / %s", a, b)
	}
	if string(mcp.Canonical(nil)) != `{}` {
		t.Fatal("empty args must canonicalize to {}")
	}
	k := mcp.CallKey(7, "enrich", "weather.now", a, 1, 0)
	if len(k) != 32 || k == mcp.CallKey(7, "enrich", "weather.now", a, 1, 1) || k != mcp.CallKey(7, "enrich", "weather.now", b, 1, 0) {
		t.Fatalf("call key %s", k)
	}
	if d := mcp.ArgsDigest(a); len(d) != 64 {
		t.Fatalf("digest %s", d)
	}
}

// 评审 P1-1 回归：去重命中时模型拿到的是 raw 对象里的原始结果，不是已脱敏的 result_summary。
func TestCall_DedupReplayReturnsUnmaskedResult(t *testing.T) {
	e := newEnv(t, func(o *mcp.Options) { o.Mask = maskAll })
	c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
	first := e.call(t, c)
	second := e.call(t, c)
	if n := e.srv["field"].count("field.get"); n != 1 {
		t.Fatalf("invocations = %d, want 1 (dedup hit)", n)
	}
	if s := string(e.calls.last().row.ResultSummary); s != `{"masked":true}` {
		t.Fatalf("stored summary = %s, want masked", s)
	}
	if e.raw.gets != 1 || second.ToolCallID != first.ToolCallID || string(second.Data) != string(first.Data) ||
		second.Source != "mock" {
		t.Fatalf("replay = %+v (gets %d), want original %+v", second, e.raw.gets, first)
	}
}

// 有 Mask 而取不回 raw（对象读失败或写 raw 失败留 NULL）：不把脱敏摘要交给模型，改为重做并落新 attempt。
func TestCall_DedupWithoutRawInvokesAgain(t *testing.T) {
	tests := []struct {
		name     string
		breakRaw func(*testEnv)
	}{
		{"raw get fails", func(e *testEnv) { e.raw.getFail = true }},
		{"raw never written", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, func(o *mcp.Options) { o.Mask = maskAll })
			if tt.breakRaw == nil {
				e.raw.fail = true
			}
			c := agent.ToolCall{RunID: 7, StepID: "enrich", Tool: "field.get", Args: fieldArgs}
			first := e.call(t, c)
			if tt.breakRaw != nil {
				tt.breakRaw(e)
			}
			second := e.call(t, c)
			if n := e.srv["field"].count("field.get"); n != 2 {
				t.Fatalf("invocations = %d, want 2 (not replayable)", n)
			}
			if string(second.Data) != string(first.Data) || string(second.Data) == `{"masked":true}` {
				t.Fatalf("second data = %s, want fresh original", second.Data)
			}
			rows := e.calls.all()
			if len(rows) != 2 || rows[1].attempt != 2 {
				t.Fatalf("rows = %+v, want 2 attempts", rows)
			}
		})
	}
}

func TestNewGateway_MaskRequiresRawReader(t *testing.T) {
	e := newEnv(t, nil)
	srvs := []mcp.Server{e.srv["field"]}
	_, err := mcp.NewGateway(srvs, mcp.Options{Clock: e.clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys: e.keys, ToolPolicy: toolPolicy(t, testToolPolicy), Idem: e.idem, Cache: e.cache, Calls: e.calls,
		Raw: e.raw, Fields: memFields{}, Mask: maskAll})
	if err == nil || !strings.Contains(err.Error(), "RawReader is required") {
		t.Fatalf("err = %v, want RawReader required", err)
	}
}
