package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// ---- P2 L4 注入 / 影子比对三态测试（rules_offload_inject.go）----

// modeFlags 是按键返回固定档位的开关读取（err 非空时一律报错，模拟读取失败）。
type modeFlags struct {
	modes map[model.FlagKey]model.FlagMode
	err   error
}

func (f modeFlags) Bool(context.Context, model.FlagKey, model.Scope) (bool, error) {
	return false, f.err
}

func (f modeFlags) Mode(_ context.Context, k model.FlagKey, _ model.Scope) (model.FlagMode, error) {
	if f.err != nil {
		return "", f.err
	}
	if m, ok := f.modes[k]; ok {
		return m, nil
	}
	return model.FlagModeOff, nil
}

// shadowStore 在 memStore 上实现 graph.ShadowAdviceStore，记下同事务写入的影子行（AdviceID 按卡片结局填）。
type shadowStore struct {
	*memStore
	mu    sync.Mutex
	diffs []model.RuleShadowDiff
}

var _ graph.ShadowAdviceStore = (*shadowStore)(nil)

func (s *shadowStore) SaveAdviceWithShadow(ctx context.Context, scope model.Scope, c model.AdviceCard, diffs []model.RuleShadowDiff) (uint64, error) {
	id, err := s.SaveAdvice(ctx, scope, c)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range diffs {
		if c.Outcome == model.OutcomeAdvice {
			d.AdviceID = id
		}
		s.diffs = append(s.diffs, d)
	}
	return id, nil
}

func (s *shadowStore) rows() []model.RuleShadowDiff {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RuleShadowDiff(nil), s.diffs...)
}

// offloadModeCase 是一档开关取值：flags 为 nil 表示未装配开关读取。
type offloadModeCase struct {
	name   string
	flags  *modeFlags
	inject bool // explain 入参带 constraints
	shadow bool // 写影子行并发 RULE_SHADOW_DIFF
}

// offloadModeCases 是某下沉项的三态与两种失败关闭（未装配、读失败）。
func offloadModeCases(k model.FlagKey) []offloadModeCase {
	only := func(m model.FlagMode) *modeFlags { return &modeFlags{modes: map[model.FlagKey]model.FlagMode{k: m}} }
	return []offloadModeCase{
		{name: "unwired"},
		{name: "read error", flags: &modeFlags{err: errors.New("redis down")}},
		{name: "off", flags: only(model.FlagModeOff)},
		{name: "shadow", flags: only(model.FlagModeShadow), shadow: true},
		{name: "on", flags: only(model.FlagModeOn), inject: true},
	}
}

// offloadRun 是一次三态用例的运行结果。
type offloadRunResult struct {
	outcome model.Outcome
	reason  string
	orders  int
	checks  []string
	explain []string
	rows    []model.RuleShadowDiff
	events  int
}

// runOffload 用给定开关与 explain 回复（空则走桩模型）跑样本 s。
func runOffload(t *testing.T, s *sample.Sample, mc offloadModeCase, explain string) offloadRunResult {
	t.Helper()
	h := newHarness(t)
	m := &recordingModel{}
	if explain != "" {
		m.scriptedModel = scriptedModel{override: map[llm.Task]string{llm.TaskExplain: explain}}
	}
	store := &shadowStore{memStore: newMemStore()}
	ev := &recordingEvents{}
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: m, tools: fixtureTools(t, s, clk), store: store.memStore, events: ev,
		cfg: func(c *graph.Config) {
			c.Advice = store
			if mc.flags != nil {
				c.Flags = *mc.flags
			}
		}})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	r := offloadRunResult{outcome: ans.Outcome, reason: ans.Card.RefuseReason, orders: store.orderCount(),
		checks: validationChecks(t, ev), explain: append([]string(nil), m.explain...), rows: store.rows()}
	ev.mu.Lock()
	for _, e := range ev.events {
		if e.Type == model.EventRuleShadowDiff {
			r.events++
		}
	}
	ev.mu.Unlock()
	return r
}

// offloadItemCase 是一个下沉项的三态用例：conflict 是与规则冲突的 explain 回复，inject 是 on 时 explain 入参应含的片段。
type offloadItemCase struct {
	item     model.RuleOffloadItem
	sample   func(t *testing.T) *sample.Sample
	conflict string
	kind     model.RuleDiffKind
	inject   string
}

// offloadItemCases 是已接入注入的下沉项。
var offloadItemCases = []offloadItemCase{
	{item: model.RuleItemOperationWindow, sample: operationSample,
		conflict: `{"conclusion":"达到防治条件，下周再统一喷药","reasons":[]}`, kind: model.DiffWindowOutside,
		inject: `"operation_window":{"days":["今天","明天"],"open":true}`},
	{item: model.RuleItemTankMix, sample: operationSample,
		conflict: `{"conclusion":"达到防治条件，建议在作业窗口内施药，可将己唑醇与咪鲜胺桶混一起喷","reasons":[]}`,
		kind:     model.DiffForbiddenMix, inject: `"tank_mix":{"selected":"吡蚜酮","tank_limit":true}`},
	{item: model.RuleItemPestPriority, sample: twoPestSample,
		conflict: `{"conclusion":"达到防治条件，优先防治稻飞虱","reasons":[]}`, kind: model.DiffOrderMismatch,
		inject: `"pest_priority":{"order":["稻纵卷叶螟","稻飞虱"]}`},
}

func operationSample(t *testing.T) *sample.Sample { return findSample(t, loadSamples(t), sOperation) }

// twoPestSample：虫情调查同时给出稻飞虱（1300，指标 1000）与稻纵卷叶螟（150，超标倍数更高）。
func twoPestSample(t *testing.T) *sample.Sample {
	return withToolValue(operationSample(t), toolPestSurvey, "larvae_per_100_hills", 150.0)
}

// TestAgentAsk_OffloadModes（P2 L4 验收 1 / 4）：每个下沉项的三态表驱动（另含未装配、读失败两种失败关闭）。
//   - 冲突结论：五档都 REFUSED / VALIDATION_FAILED（check=项名）；shadow 另写一行 agreed=false、advice_id=0
//     （拒答未出卡）并发 RULE_SHADOW_DIFF。
//   - 一致结论（桩模型）：五档都出卡；只有 on 的 explain 入参带 constraints，其余与未装配逐字节相同；
//     shadow 写一行 agreed=true、advice_id 为卡片 id。读失败按 off。
func TestAgentAsk_OffloadModes(t *testing.T) {
	for _, ic := range offloadItemCases {
		s := ic.sample(t)
		var baseline string
		for _, mc := range offloadModeCases(ic.item.Flag()) {
			t.Run(string(ic.item)+"/"+mc.name+"/conflict", func(t *testing.T) {
				r := runOffload(t, s, mc, ic.conflict)
				if r.outcome != model.OutcomeRefused || r.reason != "VALIDATION_FAILED" || r.orders != 0 {
					t.Fatalf("outcome %s/%q orders %d, want VALIDATION_FAILED", r.outcome, r.reason, r.orders)
				}
				if len(r.checks) != 1 || r.checks[0] != string(ic.item) {
					t.Fatalf("checks %v, want [%s]", r.checks, ic.item)
				}
				checkShadowRows(t, ic, mc, r, false)
			})
			t.Run(string(ic.item)+"/"+mc.name+"/agreed", func(t *testing.T) {
				r := runOffload(t, s, mc, "")
				if r.outcome != model.OutcomeAdvice || len(r.checks) != 0 {
					t.Fatalf("outcome %s/%q checks %v, want ADVICE", r.outcome, r.reason, r.checks)
				}
				if len(r.explain) != 1 {
					t.Fatalf("explain calls %d, want 1", len(r.explain))
				}
				in := r.explain[0]
				if got := strings.Contains(in, `"constraints"`); got != mc.inject {
					t.Fatalf("explain input has constraints = %v, want %v: %s", got, mc.inject, in)
				}
				if mc.inject && !strings.Contains(in, ic.inject) {
					t.Fatalf("explain input lacks %s: %s", ic.inject, in)
				}
				if mc.flags == nil {
					baseline = in
				} else if !mc.inject && in != baseline {
					t.Fatalf("explain input differs from unwired:\n%s\n%s", in, baseline)
				}
				checkShadowRows(t, ic, mc, r, true)
			})
		}
	}
}

// checkShadowRows：shadow 档写且只写一行该项（agreed 与期望一致，拒答 advice_id=0）并发一条事件；其余档不写。
func checkShadowRows(t *testing.T, ic offloadItemCase, mc offloadModeCase, r offloadRunResult, agreed bool) {
	t.Helper()
	if !mc.shadow {
		if len(r.rows) != 0 || r.events != 0 {
			t.Fatalf("rows %d events %d, want none outside shadow", len(r.rows), r.events)
		}
		return
	}
	if len(r.rows) != 1 || r.events != 1 {
		t.Fatalf("rows %+v events %d, want one row and one RULE_SHADOW_DIFF", r.rows, r.events)
	}
	d := r.rows[0]
	if d.Item != ic.item || d.Agreed != agreed || d.RuleVer == "" || d.CreatedAt.IsZero() {
		t.Fatalf("row %+v, want %s agreed=%v", d, ic.item, agreed)
	}
	var m map[string]any
	if err := json.Unmarshal(d.ModelOutput, &m); err != nil {
		t.Fatalf("model_output %s: %v", d.ModelOutput, err)
	}
	if agreed {
		if d.DiffKind != "" || d.AdviceID == 0 || len(m) != 0 {
			t.Fatalf("agreed row %+v model_output %s, want no diff kind, advice_id set", d, d.ModelOutput)
		}
		return
	}
	if d.DiffKind != ic.kind || d.AdviceID != 0 || m["snippet"] == nil {
		t.Fatalf("conflict row %+v model_output %s, want %s, advice_id 0, snippet", d, d.ModelOutput, ic.kind)
	}
}
