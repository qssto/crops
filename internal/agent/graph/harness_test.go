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
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
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
	return &harness{kb: knowledge.NewStore(knowledge.NewRepoReader(repoRoot)), tx: tx, prompts: p}
}

// resolveField 把 field-00N 解析为 N（测试种子的 field_profile.id）。
func resolveField(_ context.Context, c string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(c, fieldCode), 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("bad field code %q", c)
	}
	return n, nil
}

// deps 是一次构造主链路的可替换依赖。
type deps struct {
	model  einomodel.BaseChatModel
	tools  agent.ToolProvider
	store  *memStore
	locker agent.RunLocker
	renew  time.Duration
}

func (h *harness) newAgent(t *testing.T, clk *env.FrozenClock, d deps) *graph.Agent {
	t.Helper()
	if d.model == nil {
		d.model = llm.Stub{}
	}
	if d.locker == nil {
		d.locker = fakeLocker{}
	}
	c, err := llm.New(llm.Config{Model: d.model, Prompts: h.prompts,
		Tiers: map[llm.Tier]llm.TierConfig{tierStd: {MaxTokens: 1024, Timeout: 5 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := graph.New(context.Background(), graph.Config{LLM: c, Tier: tierStd, Tools: d.tools,
		Runs: d.store, Advice: d.store, Drafts: d.store, Locker: d.locker, Clock: clk, KB: h.kb,
		Taxonomy: h.tx, Crop: testCrop, Region: testReg, Logger: slog.New(slog.NewTextHandler(testLogOut(), nil)),
		RenewEvery: d.renew})
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

func loadSamples(t *testing.T) []*sample.Sample {
	t.Helper()
	ss, _, err := sample.LoadDir(repoRoot + "/eval/suites")
	if err != nil {
		t.Fatal(err)
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

// runExperiment 用 evalkit.Runner + code.All 打分器跑样本（每个样本新建 FixtureMCP 与主链路）。
func (h *harness) runExperiment(t *testing.T, ss []*sample.Sample) *evalkit.Experiment {
	t.Helper()
	schema, err := code.NewSchemaValid(repoRoot + "/contracts")
	if err != nil {
		t.Fatal(err)
	}
	envFn := func(_ context.Context, s *sample.Sample, _ int) (*app.App, func() error, error) {
		clk := sampleClock(t, s)
		tools := fixtureTools(t, s, clk)
		a := h.newAgent(t, clk, deps{tools: tools, store: newMemStore()})
		return &app.App{Agent: a, Tools: tools}, func() error { return nil }, nil
	}
	question := func(_ context.Context, _ *app.App, s *sample.Sample) (agent.Question, error) { return questionOf(s) }
	r, err := evalkit.NewRunner(envFn, nil, evalkit.WithGraders(h.kb, code.All(schema)...), evalkit.WithQuestion(question))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.Run(context.Background(), ss, evalkit.Options{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	return exp
}
