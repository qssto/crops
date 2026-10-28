package env

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// tool_call.status 取值与故障错误码（FixtureMCP 产出的 ToolResult）。
const (
	StatusOK       = "OK"
	StatusTimeout  = "TIMEOUT"
	StatusEmpty    = "EMPTY"
	StatusError    = "ERROR"
	StatusRejected = "REJECTED"

	CodeTimeout        = "TIMEOUT"
	CodeEmpty          = "EMPTY"
	CodeMalformed      = "MALFORMED"
	CodeUnexpectedCall = "UNEXPECTED_CALL"

	// DefaultFixtureSource 是 return 中未写 source 时的数据源。
	DefaultFixtureSource = "fixture"
	// ArgFieldID 是工具入参里的地块键，match.field 解析后改写为它。
	ArgFieldID = "field_id"
)

// faults 把 fixtures.mcp.*.error 映射为 (Status, ErrorCode)。
var faults = map[string][2]string{
	sample.FaultTimeout:   {StatusTimeout, CodeTimeout},
	sample.FaultEmpty:     {StatusEmpty, CodeEmpty},
	sample.FaultMalformed: {StatusError, CodeMalformed},
}

// FieldResolver 把样本里的地块编码（field-003）解析为 field_profile.id。
// 评测装配时由 trial schema 的 repository.ProfileRepo.FieldIDByCode 提供（种子已灌入）。
type FieldResolver func(ctx context.Context, code string) (uint64, error)

// ToolCallRecord 是一次工具调用记录（must_call 打分与恢复次数断言使用）。
type ToolCallRecord struct {
	Seq       int             // 调用序号，从 1 开始
	Tool      string          // 工具全名
	Args      json.RawMessage // 原始入参
	Status    string          // 返回的 tool_call.status
	ErrorCode string
}

// stub 是一条已解析的 fixture；next 是 returns 的游标。
type stub struct {
	match map[string]any // JSON 规范化后的 match（field 已改写为 field_id）
	fx    sample.ToolFixture
	next  int
}

// FixtureMCP 是评测用 agent.ToolProvider：按样本 fixtures.mcp 回放工具返回、注入故障，并记录调用序列。
// 每个 trial 新建一个实例（returns 游标与调用记录按 trial 隔离）。并发安全。
type FixtureMCP struct {
	clk clock.Clock

	mu         sync.Mutex
	stubs      map[string][]*stub // 工具名 → 按声明顺序的 fixture
	calls      []ToolCallRecord
	unexpected []ToolCallRecord
}

// NewFixtureMCP 校验并解析 fixtures：结构非法、故障类型未知或地块编码解析失败都返回错误。
// match 中的 field: field-00x 经 resolve 改写为 field_id: <id>；clk 用于由 return.collected_at 计算新鲜度。
func NewFixtureMCP(ctx context.Context, fixtures map[string][]sample.ToolFixture, resolve FieldResolver, clk clock.Clock) (*FixtureMCP, error) {
	if clk == nil {
		return nil, fmt.Errorf("fixture mcp: clock is required")
	}
	f := &FixtureMCP{clk: clk, stubs: make(map[string][]*stub, len(fixtures))}
	for _, tool := range sortedTools(fixtures) {
		for i, fx := range fixtures[tool] {
			where := fmt.Sprintf("fixtures.mcp.%s[%d]", tool, i)
			if p := fx.Problem(); p != "" {
				return nil, fmt.Errorf("fixture mcp: %s: %s", where, p)
			}
			if _, ok := faults[fx.Error]; fx.Error != "" && !ok {
				return nil, fmt.Errorf("fixture mcp: %s: unknown error %q", where, fx.Error)
			}
			m, err := resolveMatch(ctx, fx.Match, resolve)
			if err != nil {
				return nil, fmt.Errorf("fixture mcp: %s: %w", where, err)
			}
			f.stubs[tool] = append(f.stubs[tool], &stub{match: m, fx: fx})
		}
	}
	return f, nil
}

func sortedTools(m map[string][]sample.ToolFixture) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolveMatch 把 match.field 改写为 field_id，并把值规范化为 JSON 解码形态（数字一律 float64）。
func resolveMatch(ctx context.Context, match map[string]any, resolve FieldResolver) (map[string]any, error) {
	out := make(map[string]any, len(match))
	for k, v := range match {
		out[k] = v
	}
	if code, ok := out[sample.MatchKeyField]; ok {
		if _, dup := out[ArgFieldID]; dup {
			return nil, fmt.Errorf("match has both %s and %s", sample.MatchKeyField, ArgFieldID)
		}
		s, isStr := code.(string)
		if !isStr || resolve == nil {
			return nil, fmt.Errorf("match.%s must be a field code and needs a resolver", sample.MatchKeyField)
		}
		id, err := resolve(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", s, err)
		}
		delete(out, sample.MatchKeyField)
		out[ArgFieldID] = id
	}
	return normalize(out)
}

// normalize 经 JSON 往返，使 YAML 值与调用入参可直接 DeepEqual 比较。
func normalize(v map[string]any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("normalize match: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("normalize match: %w", err)
	}
	return out, nil
}

// Call 实现 agent.ToolProvider：按工具名取第一条 match 为入参子集的 fixture 回放；
// 未声明的调用（含 returns 用完后的再次调用）返回 REJECTED / UNEXPECTED_CALL 并计入 UnexpectedCalls。
// 故障以 ToolResult.Status / ErrorCode 表达；error 只用于 ctx 取消与内部错误（如 return 无法序列化）。
// 入参不是 JSON 对象时无法匹配，同样按 UNEXPECTED_CALL 处理。
func (f *FixtureMCP) Call(ctx context.Context, _ model.Scope, call agent.ToolCall) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, fmt.Errorf("fixture mcp %s: %w", call.Tool, err)
	}
	args, argsOK := decodeArgs(call.Args)
	f.mu.Lock()
	defer f.mu.Unlock()
	res, err := f.reply(call.Tool, args, argsOK)
	if err != nil {
		return agent.ToolResult{}, err
	}
	res.ToolCallID = uint64(len(f.calls) + 1)
	rec := ToolCallRecord{Seq: len(f.calls) + 1, Tool: call.Tool, Args: append(json.RawMessage(nil), call.Args...),
		Status: res.Status, ErrorCode: res.ErrorCode}
	f.calls = append(f.calls, rec)
	if res.ErrorCode == CodeUnexpectedCall {
		f.unexpected = append(f.unexpected, rec)
	}
	return res, nil
}

// decodeArgs 解码入参为对象；空入参视为 {}，非对象视为不可匹配。
func decodeArgs(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// reply 在持锁下选出 fixture 并生成结果。
func (f *FixtureMCP) reply(tool string, args map[string]any, argsOK bool) (agent.ToolResult, error) {
	unexpected := agent.ToolResult{Status: StatusRejected, ErrorCode: CodeUnexpectedCall}
	if !argsOK {
		return unexpected, nil
	}
	var st *stub
	for _, s := range f.stubs[tool] {
		if subsetMatch(s.match, args) {
			st = s
			break
		}
	}
	switch {
	case st == nil:
		return unexpected, nil
	case st.fx.Error != "":
		fault := faults[st.fx.Error] // 构造时已校验
		return agent.ToolResult{Status: fault[0], ErrorCode: fault[1]}, nil
	case len(st.fx.Returns) > 0:
		if st.next >= len(st.fx.Returns) {
			return unexpected, nil
		}
		v := st.fx.Returns[st.next]
		st.next++
		return f.ok(tool, v)
	default:
		return f.ok(tool, st.fx.Return)
	}
}

// subsetMatch 判断 match 的每个顶层键都在 args 中且值相等（值本身整体比较）。
func subsetMatch(match, args map[string]any) bool {
	for k, want := range match {
		got, ok := args[k]
		if !ok || !reflect.DeepEqual(want, got) {
			return false
		}
	}
	return true
}

// ok 组装成功结果：source 取 return.source（缺省 fixture），新鲜度由 return.collected_at 相对当前时钟计算。
func (f *FixtureMCP) ok(tool string, v any) (agent.ToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("fixture mcp %s: marshal return: %w", tool, err)
	}
	res := agent.ToolResult{Status: StatusOK, Source: DefaultFixtureSource, Data: data}
	m, isMap := v.(map[string]any)
	if !isMap {
		return res, nil
	}
	if s, ok := m["source"].(string); ok && s != "" {
		res.Source = s
	}
	if at, ok := parseCollectedAt(m["collected_at"]); ok {
		if d := f.clk.Now().Sub(at); d > 0 {
			res.DataFreshnessSec = int(d / time.Second)
		}
	}
	return res, nil
}

// parseCollectedAt 接受 RFC3339 时刻或 YYYY-MM-DD 日期（按 UTC 零点）。
func parseCollectedAt(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Calls 返回按调用顺序的全部调用记录（副本）。
func (f *FixtureMCP) Calls() []ToolCallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ToolCallRecord(nil), f.calls...)
}

// CallCounts 返回工具名 → 调用次数（must_call 与恢复次数断言）。
func (f *FixtureMCP) CallCounts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.calls))
	for _, c := range f.calls {
		out[c.Tool]++
	}
	return out
}

// UnexpectedCalls 返回未声明的调用（报告的 unexpected_call 列表，副本）。
func (f *FixtureMCP) UnexpectedCalls() []ToolCallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ToolCallRecord(nil), f.unexpected...)
}
