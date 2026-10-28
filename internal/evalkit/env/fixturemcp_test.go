package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// seededFields 模拟 trial schema 中种子灌入后的 ext_field_code → id。
var seededFields = map[string]uint64{"field-002": 12, "field-005": 15}

func resolveSeeded(_ context.Context, code string) (uint64, error) {
	if id, ok := seededFields[code]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("field %s: %w", code, model.ErrNotFound)
}

// fixturesYAML 按样本 YAML 写法解析 fixtures.mcp，保证测试走与真实样本相同的类型（int / string / map）。
func fixturesYAML(t *testing.T, src string) map[string][]sample.ToolFixture {
	t.Helper()
	var out map[string][]sample.ToolFixture
	if err := yaml.Unmarshal([]byte(src), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const fixtureSrc = `
crop.phenology:
  - match: {field: field-002}
    return: {stage: 孕穗期, collected_at: "2026-08-04T06:00:00Z", source: mock-xgeo}
  - return: {stage: 分蘖期}
weather.forecast:
  - match: {field: field-002, days: 3}
    error: timeout
  - match: {field: field-002}
    returns:
      - {wind_speed_ms: 5.5}
      - {wind_speed_ms: 2.0, source: mock-station}
field.pest_survey:
  - match: {field: field-005}
    error: empty
weather.history:
  - error: malformed
`

type step struct {
	tool, args           string
	wantStatus, wantCode string
	wantData, wantSource string
	wantFresh            int
}

func TestFixtureMCP_Call(t *testing.T) {
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	f, err := NewFixtureMCP(context.Background(), fixturesYAML(t, fixtureSrc), resolveSeeded, NewFrozenClock(now))
	if err != nil {
		t.Fatal(err)
	}
	// 顺序执行：returns 游标依赖调用次序。
	steps := []step{
		{tool: "crop.phenology", args: `{"field_id":12}`, wantStatus: StatusOK, wantSource: "mock-xgeo", wantFresh: 7200,
			wantData: `{"collected_at":"2026-08-04T06:00:00Z","source":"mock-xgeo","stage":"孕穗期"}`},
		{tool: "crop.phenology", args: `{"field_id":15}`, wantStatus: StatusOK, wantSource: DefaultFixtureSource,
			wantData: `{"stage":"分蘖期"}`}, // 无 match 的兜底条目
		{tool: "weather.forecast", args: `{"field_id":12,"days":3}`, wantStatus: StatusTimeout, wantCode: CodeTimeout},
		{tool: "weather.forecast", args: `{"field_id":12}`, wantStatus: StatusOK, wantSource: DefaultFixtureSource,
			wantData: `{"wind_speed_ms":5.5}`},
		{tool: "weather.forecast", args: `{"field_id":12,"days":5}`, wantStatus: StatusOK, wantSource: "mock-station",
			wantData: `{"source":"mock-station","wind_speed_ms":2}`},
		{tool: "weather.forecast", args: `{"field_id":12}`, wantStatus: StatusRejected, wantCode: CodeUnexpectedCall}, // returns 用完
		{tool: "weather.forecast", args: `{"field_id":15}`, wantStatus: StatusRejected, wantCode: CodeUnexpectedCall}, // 无匹配
		{tool: "field.pest_survey", args: `{"field_id":15,"target":"planthopper"}`, wantStatus: StatusEmpty, wantCode: CodeEmpty},
		{tool: "weather.history", args: ``, wantStatus: StatusError, wantCode: CodeMalformed},
		{tool: "device.list", args: `{"field_id":12}`, wantStatus: StatusRejected, wantCode: CodeUnexpectedCall}, // 未声明工具
		{tool: "crop.phenology", args: `[1]`, wantStatus: StatusRejected, wantCode: CodeUnexpectedCall},          // 入参非对象
	}
	for i, s := range steps {
		res, err := f.Call(context.Background(), model.Scope{}, agent.ToolCall{Tool: s.tool, Args: json.RawMessage(s.args)})
		if err != nil {
			t.Fatalf("step %d %s: %v", i, s.tool, err)
		}
		if res.Status != s.wantStatus || res.ErrorCode != s.wantCode || res.Source != s.wantSource ||
			string(res.Data) != s.wantData || res.DataFreshnessSec != s.wantFresh {
			t.Errorf("step %d %s %s: got %+v (data %s), want %+v", i, s.tool, s.args, res, res.Data, s)
		}
		if res.ToolCallID != uint64(i+1) {
			t.Errorf("step %d: ToolCallID = %d", i, res.ToolCallID)
		}
	}
	if got := len(f.Calls()); got != len(steps) {
		t.Errorf("calls = %d, want %d", got, len(steps))
	}
	counts := f.CallCounts()
	if counts["weather.forecast"] != 5 || counts["crop.phenology"] != 3 || counts["device.list"] != 1 {
		t.Errorf("counts = %v", counts)
	}
	var tools []string
	for _, u := range f.UnexpectedCalls() {
		tools = append(tools, u.Tool)
	}
	if got := strings.Join(tools, ","); got != "weather.forecast,weather.forecast,device.list,crop.phenology" {
		t.Errorf("unexpected = %s", got)
	}
}

func TestNewFixtureMCP_Rejects(t *testing.T) {
	failResolve := func(context.Context, string) (uint64, error) { return 0, errors.New("db down") }
	tests := []struct {
		name    string
		src     string
		resolve FieldResolver
		want    string
	}{
		{"no outcome", "a.b: [{match: {x: 1}}]", resolveSeeded, "exactly one of"},
		{"two outcomes", "a.b: [{return: {x: 1}, error: empty}]", resolveSeeded, "exactly one of"},
		{"null in returns", "a.b: [{returns: [{x: 1}, null]}]", resolveSeeded, "returns[1] is null"},
		{"unknown fault", "a.b: [{error: flaky}]", resolveSeeded, `unknown error "flaky"`},
		{"unknown field code", "a.b: [{match: {field: field-099}, return: {x: 1}}]", resolveSeeded, "not found"},
		{"field not a string", "a.b: [{match: {field: 3}, return: {x: 1}}]", resolveSeeded, "must be a field code"},
		{"field and field_id", "a.b: [{match: {field: field-002, field_id: 12}, return: {x: 1}}]", resolveSeeded, "both"},
		{"no resolver", "a.b: [{match: {field: field-002}, return: {x: 1}}]", nil, "needs a resolver"},
		{"resolver error", "a.b: [{match: {field: field-002}, return: {x: 1}}]", failResolve, "db down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFixtureMCP(context.Background(), fixturesYAML(t, tt.src), tt.resolve, NewFrozenClock(time.Time{}))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := NewFixtureMCP(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("want error for nil clock")
	}
}

func TestFixtureMCP_MatchSubset(t *testing.T) {
	src := `t.x: [{match: {field: field-002, opts: {deep: [1, 2]}}, return: {ok: true}}]`
	tests := []struct {
		args string
		want string
	}{
		{`{"field_id":12,"opts":{"deep":[1,2]},"extra":"ignored"}`, StatusOK}, // 子集匹配：多余键忽略
		{`{"field_id":12.0,"opts":{"deep":[1,2]}}`, StatusOK},                 // 数字按 JSON 数值比较
		{`{"field_id":12,"opts":{"deep":[1,2],"more":1}}`, StatusRejected},    // 嵌套值整体比较
		{`{"field_id":"12","opts":{"deep":[1,2]}}`, StatusRejected},           // 类型不同
		{`{"opts":{"deep":[1,2]}}`, StatusRejected},                           // 缺键
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			f, err := NewFixtureMCP(context.Background(), fixturesYAML(t, src), resolveSeeded, NewFrozenClock(time.Time{}))
			if err != nil {
				t.Fatal(err)
			}
			res, err := f.Call(context.Background(), model.Scope{}, agent.ToolCall{Tool: "t.x", Args: json.RawMessage(tt.args)})
			if err != nil || res.Status != tt.want {
				t.Fatalf("status = %s err = %v, want %s", res.Status, err, tt.want)
			}
		})
	}
}

func TestFixtureMCP_Canceled(t *testing.T) {
	f, err := NewFixtureMCP(context.Background(), nil, nil, NewFrozenClock(time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Call(ctx, model.Scope{}, agent.ToolCall{Tool: "a.b"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Fatal("canceled call must not be recorded")
	}
}

// 编译期保证 FixtureMCP 实现 agent.ToolProvider。
var _ agent.ToolProvider = (*FixtureMCP)(nil)
