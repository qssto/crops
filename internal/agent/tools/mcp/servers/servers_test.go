package servers

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/config"
)

// repoRoot 是本包到仓库根的相对路径。
var repoRoot = filepath.Join("..", "..", "..", "..", "..")

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// testNow 落在 8 月中旬（抽穗前后），各地块都有已发生的农事与调查。
var testNow = time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)

var testFields = kit.StaticFields{1: "field-001", 2: "field-002", 3: "field-003", 4: "field-004", 5: "field-005", 6: "field-006"}

func testConfig(mode string) *config.Tools {
	s := config.ToolServer{Mode: mode, Timeout: 3 * time.Second, Level: "read"}
	return &config.Tools{MockDir: filepath.Join(repoRoot, "configs", "mock"), Weather: s, Crop: s, Field: s, Device: s}
}

func buildMock(t *testing.T) map[string]mcp.ToolSpec {
	t.Helper()
	srvs, err := Build(testConfig(config.ToolModeMock), fixedClock{testNow}, testFields)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]mcp.ToolSpec{}
	for _, s := range srvs {
		for _, sp := range s.Tools() {
			specs[sp.Name] = sp
		}
	}
	return specs
}

func invoke(t *testing.T, tool string, args string) (mcp.Result, error) {
	t.Helper()
	srvs, err := Build(testConfig(config.ToolModeMock), fixedClock{testNow}, testFields)
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := strings.Cut(tool, ".")
	for _, s := range srvs {
		if s.Name() == name {
			return s.Invoke(context.Background(), tool, json.RawMessage(args))
		}
	}
	t.Fatalf("no server for %s", tool)
	return mcp.Result{}, nil
}

func compile(t *testing.T, name string, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("%s: schema is not JSON: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(name, doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	sch, err := c.Compile(name)
	if err != nil {
		t.Fatalf("%s: compile: %v", name, err)
	}
	return sch
}

// TestBuild_AllToolsDeclared 锁定工具清单：增删工具须同步 docs/mock-real-status.md。
func TestBuild_AllToolsDeclared(t *testing.T) {
	want := []string{"crop.pest_spectrum", "crop.phenology", "crop.threshold", "crop.variety",
		"device.list", "device.status", "field.history", "field.pest_survey", "field.profile", "field.soil",
		"weather.alert", "weather.forecast", "weather.history", "weather.realtime"}
	specs := buildMock(t)
	list := make([]mcp.ToolSpec, 0, len(specs))
	for _, sp := range specs {
		list = append(list, sp)
	}
	if got := kit.SortedNames(list); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

// timeSensitive 是应标记 TimeSensitive 的工具：天气全部 + 设备状态。
var timeSensitive = map[string]bool{"weather.realtime": true, "weather.forecast": true, "weather.alert": true,
	"weather.history": true, "device.status": true}

// TestSpecs_SelfDescribing 检查每个工具的 schema 自描述完整：有用途说明、两份 schema 均可编译、
// 根为封闭 object、required 均已声明、入参每个字段都有 description；只读工具一律幂等；时间敏感标记正确。
func TestSpecs_SelfDescribing(t *testing.T) {
	for name, sp := range buildMock(t) {
		t.Run(name, func(t *testing.T) {
			if sp.Description == "" {
				t.Error("description is empty")
			}
			if !sp.Idempotent {
				t.Error("read tools must be idempotent")
			}
			if sp.TimeSensitive != timeSensitive[name] {
				t.Errorf("TimeSensitive = %v, want %v", sp.TimeSensitive, timeSensitive[name])
			}
			wantCache := strings.HasPrefix(name, "weather.") || strings.HasPrefix(name, "crop.")
			if sp.Cacheable != wantCache {
				t.Errorf("Cacheable = %v, want %v", sp.Cacheable, wantCache)
			}
			for kind, raw := range map[string]json.RawMessage{"input": sp.InputSchema, "output": sp.OutputSchema} {
				compile(t, name+"."+kind, raw)
				var root map[string]any
				if err := json.Unmarshal(raw, &root); err != nil {
					t.Fatalf("%s schema: %v", kind, err)
				}
				checkObject(t, kind, root, kind == "input")
			}
		})
	}
}

func checkObject(t *testing.T, where string, node map[string]any, needDesc bool) {
	t.Helper()
	if node["type"] != "object" || node["additionalProperties"] != false {
		t.Errorf("%s: must be object with additionalProperties=false", where)
	}
	props, _ := node["properties"].(map[string]any)
	if len(props) == 0 {
		t.Errorf("%s: properties must be non-empty", where)
	}
	req, _ := node["required"].([]any)
	for _, r := range req {
		if _, ok := props[r.(string)]; !ok {
			t.Errorf("%s: required %q not in properties", where, r)
		}
	}
	for k, v := range props {
		p, ok := v.(map[string]any)
		if !ok || p["type"] == nil {
			t.Errorf("%s.%s: type is missing", where, k)
			continue
		}
		if needDesc && p["description"] == nil {
			t.Errorf("%s.%s: description is missing", where, k)
		}
	}
}

// TestBuild_RealMode 验证 mock → real 只改配置：工具清单不变，调用统一返回 UPSTREAM。
func TestBuild_RealMode(t *testing.T) {
	mock := buildMock(t)
	srvs, err := Build(testConfig(config.ToolModeReal), fixedClock{testNow}, testFields)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range srvs {
		for _, sp := range s.Tools() {
			n++
			if string(sp.OutputSchema) != string(mock[sp.Name].OutputSchema) {
				t.Errorf("%s: real schema differs from mock", sp.Name)
			}
			_, err := s.Invoke(context.Background(), sp.Name, json.RawMessage(`{"field_id":1}`))
			var te *mcp.ToolError
			if !errors.As(err, &te) || te.Code != mcp.ErrUpstream {
				t.Errorf("%s: err = %v, want UPSTREAM", sp.Name, err)
			}
		}
	}
	if n != len(mock) {
		t.Fatalf("real tools = %d, mock tools = %d", n, len(mock))
	}
}

func TestBuild_Errors(t *testing.T) {
	missing := testConfig(config.ToolModeMock)
	missing.MockDir = t.TempDir()
	badMode := testConfig(config.ToolModeMock)
	badMode.Crop.Mode = "stub"
	tests := []struct {
		name string
		cfg  *config.Tools
		dir  kit.FieldDirectory
	}{
		{"nil config", nil, testFields},
		{"nil directory", testConfig(config.ToolModeMock), nil},
		{"missing mock files", missing, testFields},
		{"bad mode", badMode, testFields},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build(tt.cfg, fixedClock{testNow}, tt.dir); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

// TestInvoke_OutputMatchesSchema 对每个工具用合法入参调用 mock，输出须通过自身 OutputSchema，
// 且带数据源、非负新鲜度与 RFC3339 采集时间。
func TestInvoke_OutputMatchesSchema(t *testing.T) {
	tests := []struct{ tool, args string }{
		{"weather.realtime", `{"field_id":1}`},
		{"weather.forecast", `{"field_id":2,"days":7}`},
		{"weather.forecast", `{"field_id":2}`},
		{"weather.alert", `{"field_id":3}`},
		{"weather.history", `{"field_id":6}`},
		{"weather.history", `{"field_id":6,"start_date":"2026-08-01","end_date":"2026-08-10"}`},
		{"crop.phenology", `{"field_id":1}`},
		{"crop.variety", `{"field_id":4}`},
		{"crop.pest_spectrum", `{"field_id":5}`},
		{"crop.pest_spectrum", `{"field_id":5,"stage":"分蘖期"}`},
		{"crop.threshold", `{"target":"planthopper","stage":"抽穗扬花期"}`},
		{"field.profile", `{"field_id":3}`},
		{"field.history", `{"field_id":1}`},
		{"field.history", `{"field_id":1,"kind":"fertilize","since":"2026-06-10"}`},
		{"field.history", `{"field_id":1,"kind":"spray"}`},
		{"field.pest_survey", `{"field_id":2}`},
		{"field.pest_survey", `{"field_id":1,"target":"sheath_blight"}`},
		{"field.soil", `{"field_id":4}`},
		{"device.list", `{"field_id":3}`},
		{"device.list", `{"field_id":3,"kind":"drone"}`},
		{"device.status", `{"device_id":"uav-p100-01"}`},
		{"device.status", `{"device_id":"pump-01"}`},
	}
	specs := buildMock(t)
	covered := map[string]bool{}
	for _, tt := range tests {
		covered[tt.tool] = true
		t.Run(tt.tool+tt.args, func(t *testing.T) {
			res, err := invoke(t, tt.tool, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(res.Data)))
			if err != nil {
				t.Fatal(err)
			}
			if err := compile(t, tt.tool, specs[tt.tool].OutputSchema).Validate(doc); err != nil {
				t.Fatalf("output %s does not match schema: %v", res.Data, err)
			}
			if res.Source == "" || res.DataFreshnessSec < 0 {
				t.Errorf("source=%q freshness=%d", res.Source, res.DataFreshnessSec)
			}
			if _, err := time.Parse(time.RFC3339, res.CollectedAt); err != nil {
				t.Errorf("collected_at: %v", err)
			}
		})
	}
	for name := range specs {
		if !covered[name] {
			t.Errorf("tool %s has no output case", name)
		}
	}
}

// TestInvoke_Errors 覆盖失败路径：入参非法 → REJECTED，无数据 → EMPTY。
func TestInvoke_Errors(t *testing.T) {
	tests := []struct {
		name, tool, args string
		want             mcp.ErrorCode
	}{
		{"missing field_id", "weather.realtime", `{}`, mcp.ErrRejected},
		{"unknown arg", "weather.forecast", `{"field_id":1,"lat":31.2}`, mcp.ErrRejected},
		{"wrong type", "crop.phenology", `{"field_id":"field-001"}`, mcp.ErrRejected},
		{"trailing data", "field.profile", `{"field_id":1} {}`, mcp.ErrRejected},
		{"days out of range", "weather.forecast", `{"field_id":1,"days":8}`, mcp.ErrRejected},
		{"history in future", "weather.history", `{"field_id":1,"end_date":"2026-08-25"}`, mcp.ErrRejected},
		{"history too long", "weather.history", `{"field_id":1,"start_date":"2026-06-01","end_date":"2026-08-10"}`, mcp.ErrRejected},
		{"bad date", "field.history", `{"field_id":1,"since":"0610"}`, mcp.ErrRejected},
		{"bad stage", "crop.threshold", `{"target":"planthopper","stage":"抽穗期"}`, mcp.ErrRejected},
		{"unknown tool", "weather.radar", `{"field_id":1}`, mcp.ErrRejected},
		{"unknown field", "field.profile", `{"field_id":99}`, mcp.ErrEmpty},
		{"no survey", "field.pest_survey", `{"field_id":5}`, mcp.ErrEmpty},
		{"no threshold", "crop.threshold", `{"target":"bakanae","stage":"成熟期"}`, mcp.ErrEmpty},
		{"no soil test", "field.soil", `{"field_id":6}`, mcp.ErrEmpty},
		{"unknown device", "device.status", `{"device_id":"uav-x"}`, mcp.ErrEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := invoke(t, tt.tool, tt.args)
			var te *mcp.ToolError
			if !errors.As(err, &te) || te.Code != tt.want {
				t.Fatalf("err = %v, want code %s", err, tt.want)
			}
		})
	}
}
