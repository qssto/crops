package graph_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/agent/tools/mcp/servers/device"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/grader/code"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const (
	repoRoot  = "../../.."
	tierStd   = llm.Tier("standard")
	testCrop  = "rice"
	testReg   = "jiangsu"
	tenantID  = 1
	farmID    = 1
	userID    = 7
	fieldCode = "field-"
)

var testScope = model.Scope{TenantID: tenantID, FarmID: farmID, UserID: userID}

// harness 持有跨样本共享的知识库、taxonomy 与提示词。
type harness struct {
	kb      *knowledge.Store
	tx      *knowledge.Taxonomy
	prompts *llm.Prompts
	// reg 是约束注册表：主链路与 reviewGate 的 Reviewer 共用同一实例（同 app 装配）。
	reg *constraint.Registry
	// faults 是仓库的故障降级矩阵与文案（L6）。
	faults *policy.FaultMatrix
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tx, err := knowledge.LoadTaxonomy(repoRoot + "/eval/taxonomy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := llm.LoadPrompts(repoRoot+"/prompts", "v1")
	if err != nil {
		t.Fatal(err)
	}
	faults, err := app.LoadFaults(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{kb: knowledge.NewStore(knowledge.NewRepoReader(repoRoot)), tx: tx, prompts: p, reg: constraint.Default(),
		faults: faults}
}

// resolveField 把 field-00N 解析为 N（测试种子的 field_profile.id）。
func resolveField(_ context.Context, c string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(c, fieldCode), 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("bad field code %q", c)
	}
	return n, nil
}

// seedFields 是 evalkit.FieldLister：测试农场 (1,1) 的地块为 field-001..006（id 1..6），其余农场为空。
type seedFields struct{}

func (seedFields) FieldIDs(_ context.Context, s model.Scope) ([]uint64, error) {
	if s.TenantID != tenantID || s.FarmID != farmID {
		return []uint64{}, nil
	}
	return []uint64{1, 2, 3, 4, 5, 6}, nil
}

// deps 是一次构造主链路的可替换依赖。
type deps struct {
	model  einomodel.BaseChatModel
	tools  agent.ToolProvider
	store  *memStore
	locker agent.RunLocker
	events agent.EventSink // L5：VALIDATION_FAILED 等图内事件
	logger *slog.Logger    // 为空时用 testLogOut 的文本日志
	renew  time.Duration
	cfg    func(*graph.Config) // 可空：在 graph.New 前改写配置（L6 注入 Gate / Checkpoints / Resumes / Events）
}

func (h *harness) newAgent(t *testing.T, clk *env.FrozenClock, d deps) *graph.Agent {
	t.Helper()
	if d.model == nil {
		d.model = llm.Stub{}
	}
	if d.locker == nil {
		d.locker = fakeLocker{}
	}
	if d.logger == nil {
		d.logger = slog.New(slog.NewTextHandler(testLogOut(), nil))
	}
	c, err := llm.New(llm.Config{Model: d.model, Prompts: h.prompts,
		Tiers: map[llm.Tier]llm.TierConfig{tierStd: {MaxTokens: 1024, Timeout: 5 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := graph.Config{LLM: c, Tier: tierStd, Tools: d.tools,
		Runs: d.store, Advice: d.store, Drafts: d.store, Locker: d.locker, Clock: clk, KB: h.kb,
		Taxonomy: h.tx, Crop: testCrop, Region: testReg, Logger: d.logger,
		RenewEvery: d.renew, Events: d.events, Constraints: h.reg, Faults: h.faults}
	if d.cfg != nil {
		d.cfg(&cfg)
	}
	a, err := graph.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// testLogOut：设置 GRAPH_TEST_LOG 时把主链路日志打到 stderr，便于排查样本。
func testLogOut() io.Writer {
	if os.Getenv("GRAPH_TEST_LOG") != "" {
		return os.Stderr
	}
	return io.Discard
}

// sampleClock 是样本 fixtures.clock 的冻结时钟。
func sampleClock(t *testing.T, s *sample.Sample) *env.FrozenClock {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s.Fixtures.Clock)
	if err != nil {
		t.Fatalf("%s clock: %v", s.ID, err)
	}
	return env.NewFrozenClock(at)
}

// fixtureTools 为样本构造 FixtureMCP。
func fixtureTools(t *testing.T, s *sample.Sample, clk *env.FrozenClock) *env.FixtureMCP {
	t.Helper()
	f, err := env.NewFixtureMCP(context.Background(), s.Fixtures.MCP, resolveField, clk)
	if err != nil {
		t.Fatalf("%s fixtures: %v", s.ID, err)
	}
	return f
}

// questionOf 由样本构造提问（Scope 固定为测试农场，FieldID 按编码解析）。
func questionOf(s *sample.Sample) (agent.Question, error) {
	q := evalkit.QuestionOf(s)
	q.Scope = testScope
	if s.Context.Field != "" {
		id, err := resolveField(context.Background(), s.Context.Field)
		if err != nil {
			return q, err
		}
		q.FieldID = id
	}
	return q, nil
}

// suiteCapability 是待毕业的新能力样本：失败是已登记的能力缺口，只经 cmd/eval graduate（路线 §6.2）进 regression 后才受红线约束，
// 这里的样本级测试不加载它。
const suiteCapability = "capability"

func loadSamples(t *testing.T) []*sample.Sample {
	t.Helper()
	all, problems, err := sample.LoadDir(repoRoot + "/eval/suites")
	if err != nil {
		t.Fatal(err)
	}
	// 解析失败的样本会悄悄退出红线测试，必须报出。
	if len(problems) > 0 {
		t.Fatalf("%d sample file(s) fail to parse, first: %v", len(problems), problems[0])
	}
	ss := make([]*sample.Sample, 0, len(all))
	for _, s := range all {
		if s.Suite != suiteCapability {
			ss = append(ss, s)
		}
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].ID < ss[j].ID })
	return ss
}

func findSample(t *testing.T, ss []*sample.Sample, id string) *sample.Sample {
	t.Helper()
	for _, s := range ss {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("sample %s not found", id)
	return nil
}

// runExperiment 用 evalkit.Runner + code.All 打分器跑样本（每个样本新建 FixtureMCP、主链路、
// 带捕获的脱敏日志与 RecordingAdapter；无 trial 库，stop_at_approval 的审批状态不可观测）。
func (h *harness) runExperiment(t *testing.T, ss []*sample.Sample) *evalkit.Experiment {
	t.Helper()
	schema, err := code.NewSchemaValid(repoRoot + "/contracts")
	if err != nil {
		t.Fatal(err)
	}
	tp, err := policy.Load(repoRoot + "/policy/tool_policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	envFn := func(_ context.Context, s *sample.Sample, _ int) (*app.App, func() error, error) {
		clk := sampleClock(t, s)
		tools := fixtureTools(t, s, clk)
		logger, err := app.NewTappedLogger(testLogOut(), "debug", env.NewLogCapture().Tap)
		if err != nil {
			return nil, nil, err
		}
		// P1 L6：样本声明 fixtures.model.unavailable 时包装桩模型（与 cmd/eval 同一包装）。
		chat, err := env.NewFaultModel(llm.Stub{}, s.Fixtures.Model)
		if err != nil {
			return nil, nil, err
		}
		a := h.newAgent(t, clk, deps{model: chat, tools: tools, store: newMemStore(), logger: logger})
		return &app.App{Agent: a, Tools: tools, Logger: logger, Adapter: &env.RecordingAdapter{}}, func() error { return nil }, nil
	}
	devices, err := device.LoadBindings(repoRoot + "/configs/mock/device.yaml")
	if err != nil {
		t.Fatal(err)
	}
	question := func(_ context.Context, _ *app.App, s *sample.Sample) (agent.Question, error) { return questionOf(s) }
	r, err := evalkit.NewRunner(envFn, nil, evalkit.WithGraders(h.kb, code.All(schema, tp.Actuate())...),
		evalkit.WithQuestion(question), evalkit.WithFields(seedFields{}), evalkit.WithDevices(devices))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.Run(context.Background(), ss, evalkit.Options{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	return exp
}
