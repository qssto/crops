package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// loaderFlags 是 enable_loader_v1 的开关桩：err 非空时读失败。
type loaderFlags struct {
	on  bool
	err error
}

func (f loaderFlags) Bool(_ context.Context, key model.FlagKey, _ model.Scope) (bool, error) {
	if key != model.FlagEnableLoaderV1 {
		return false, nil
	}
	return f.on, f.err
}

func (f loaderFlags) Mode(context.Context, model.FlagKey, model.Scope) (model.FlagMode, error) {
	return model.FlagModeOff, nil
}

// loadRecorder 记 SetLoaderStrategy 与 ReplaceKBLoadItems 调用。
type loadRecorder struct {
	mu         sync.Mutex
	strategies []string
	items      [][]model.KBLoadItem
}

func (r *loadRecorder) SetLoaderStrategy(_ context.Context, _ model.Scope, _ uint64, s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strategies = append(r.strategies, s)
	return nil
}

func (r *loadRecorder) AddTokens(context.Context, model.Scope, uint64, int, int) error { return nil }

func (r *loadRecorder) ReplaceKBLoadItems(_ context.Context, _ model.Scope, items []model.KBLoadItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, slices.Clone(items))
	return nil
}

func repoLoading(t *testing.T, h *harness) *app.KnowledgeLoaders {
	t.Helper()
	snap, err := h.kb.Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	l, err := app.LoadKnowledgeLoaders(filepath.Join(repoRoot, app.DefaultLoaderDir), h.tx, snap)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// toolCallIDs 匹配答案 JSON 里的 tool_call_id：FixtureMCP 按调用完成次序编号，并行工具调用下次序不定，比较前抹掉。
var toolCallIDs = regexp.MustCompile(`"tool_call_id":\d+`)

// loadRun 是一次 Ask 的可比结果：答案 JSON（抹掉 tool_call_id）、错误与 KB_LOADED payload（无 KB_LOADED 时 nil）。
type loadRun struct {
	answer string
	err    string
	kb     *model.KBLoadedPayload
	card   *model.AdviceCard
}

func (h *harness) askWithLoading(t *testing.T, s *sample.Sample, cfg func(*graph.Config)) loadRun {
	t.Helper()
	clk := sampleClock(t, s)
	chat, err := env.NewFaultModel(llm.Stub{}, s.Fixtures.Model)
	if err != nil {
		t.Fatal(err)
	}
	ev := &recordingEvents{}
	a := h.newAgent(t, clk, deps{model: chat, tools: fixtureTools(t, s, clk), store: newMemStore(), events: ev, cfg: cfg})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, aerr := a.Ask(context.Background(), q)
	var out loadRun
	if aerr != nil {
		out.err = aerr.Error()
	}
	b, err := json.Marshal(ans)
	if err != nil {
		t.Fatal(err)
	}
	out.answer, out.card = toolCallIDs.ReplaceAllString(string(b), `"tool_call_id":_`), ans.Card
	for _, e := range ev.events {
		if e.Type != model.EventKBLoaded {
			continue
		}
		if out.kb != nil {
			t.Fatalf("%s: KB_LOADED emitted twice", s.ID)
		}
		var p model.KBLoadedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("%s: KB_LOADED %s: %v", s.ID, e.Payload, err)
		}
		out.kb = &p
	}
	return out
}

func sitOf(p *model.KBLoadedPayload) knowledge.Situation {
	return knowledge.Situation{Crop: p.Situation.Crop, Stage: p.Situation.Stage, Region: p.Situation.Region,
		Category: p.QuestionType, Topic: p.Situation.Topic, Target: p.Situation.Target, RankTopic: p.Situation.RankTopic}
}

// 开关关（§6.6 验收 1 前提）：全量回归 / 红队样本下，装配 configs/loader 且 enable_loader_v1=off 与未装配 Loading（P1）
// 答案逐字节一致、KB_LOADED 条目一致且 = knowledge.Select(…, 8)；首次执行不写 loader_strategy（列默认 v0），
// 明细逐条写入、名次连续。
func TestLoadKnowledge_FlagOffEqualsP1(t *testing.T) {
	h := newHarness(t)
	l := repoLoading(t, h)
	snap, err := h.kb.Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	loaded := 0
	for _, s := range loadSamples(t) {
		p1 := h.askWithLoading(t, s, nil)
		rec := &loadRecorder{}
		off := h.askWithLoading(t, s, func(c *graph.Config) {
			c.Flags, c.Usage = loaderFlags{}, rec
			c.Loading = &graph.KnowledgeLoading{V0: l.V0, V1: l.V1, Items: rec}
		})
		if off.answer != p1.answer || off.err != p1.err {
			t.Fatalf("%s: answer differs with loader configured (flag off)\n p1: %s %s\noff: %s %s", s.ID, p1.answer, p1.err, off.answer, off.err)
		}
		if (p1.kb == nil) != (off.kb == nil) {
			t.Fatalf("%s: KB_LOADED p1=%v off=%v", s.ID, p1.kb != nil, off.kb != nil)
		}
		if off.kb == nil {
			if len(rec.items) != 0 {
				t.Fatalf("%s: kb_load_item written without KB_LOADED", s.ID)
			}
			continue
		}
		loaded++
		kb := off.kb
		if !slices.Equal(kb.Entries, p1.kb.Entries) || kb.Strategy != "v0" || kb.LoadSeq != 0 || len(kb.Items) != len(kb.Entries) {
			t.Fatalf("%s: KB_LOADED off %+v, p1 entries %v", s.ID, kb, p1.kb.Entries)
		}
		hits, err := knowledge.Select(snap, h.tx, sitOf(kb), 8)
		if err != nil {
			t.Fatalf("%s: Select(%+v): %v", s.ID, sitOf(kb), err)
		}
		want := make([]string, 0, len(hits))
		for _, e := range hits {
			want = append(want, e.Ref())
		}
		if !slices.Equal(kb.Entries, want) {
			t.Fatalf("%s: entries %v, P1 Select %v", s.ID, kb.Entries, want)
		}
		if len(rec.strategies) != 0 || len(rec.items) != 1 || len(rec.items[0]) != len(kb.Entries) {
			t.Fatalf("%s: strategies %v items %d", s.ID, rec.strategies, len(rec.items))
		}
		for i, it := range rec.items[0] {
			if it.RankNo != i || it.Strategy != "v0" || knowledge.CitationRef(it.KBID, it.KBVersion) != kb.Entries[i] ||
				it.PickReason != kb.Items[i].PickReason || it.PickReason == model.PickReasonMustInclude ||
				it.QuestionType != kb.QuestionType || it.TenantID != testScope.TenantID || it.FarmID != testScope.FarmID || it.RunID == 0 {
				t.Fatalf("%s: item %d = %+v, KB_LOADED %+v", s.ID, i, it, kb.Items[i])
			}
		}
	}
	if loaded < 100 {
		t.Fatalf("only %d samples loaded knowledge", loaded)
	}
}

// 开关开取 v1；读失败或 v1 未装配回落 v0（失败关闭）。答案与开关无关（卡片引用由规则层产生，s.hits 只判空）。
func TestLoadKnowledge_FlagSelectsStrategy(t *testing.T) {
	h := newHarness(t)
	l := repoLoading(t, h)
	s := findSample(t, loadSamples(t), sOperation)
	base := h.askWithLoading(t, s, nil)
	tests := []struct {
		name  string
		flags agent.FarmFlagReader
		v1    *knowledge.Strategy
		want  string
	}{
		{name: "on", flags: loaderFlags{on: true}, v1: l.V1, want: "v1"},
		{name: "off", flags: loaderFlags{}, v1: l.V1, want: "v0"},
		{name: "read error", flags: loaderFlags{on: true, err: errors.New("redis down")}, v1: l.V1, want: "v0"},
		{name: "no flag reader", v1: l.V1, want: "v0"},
		{name: "v1 not configured", flags: loaderFlags{on: true}, want: "v0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &loadRecorder{}
			got := h.askWithLoading(t, s, func(c *graph.Config) {
				c.Flags, c.Usage = tt.flags, rec
				c.Loading = &graph.KnowledgeLoading{V0: l.V0, V1: tt.v1, Items: rec}
			})
			// 首次执行取 v0 时不写 loader_strategy（列默认值即 v0）。
			wantSet := []string{tt.want}
			if tt.want == model.LoaderStrategyV0 {
				wantSet = nil
			}
			if got.kb == nil || got.kb.Strategy != tt.want || !slices.Equal(rec.strategies, wantSet) {
				t.Fatalf("KB_LOADED %+v strategies %v, want %s", got.kb, rec.strategies, tt.want)
			}
			if len(rec.items) != 1 || len(rec.items[0]) != len(got.kb.Entries) || rec.items[0][0].Strategy != tt.want {
				t.Fatalf("items %+v", rec.items)
			}
			if got.answer != base.answer || got.err != base.err {
				t.Fatalf("answer differs from P1:\n%s\n%s", got.answer, base.answer)
			}
			if tt.want == "v1" && len(got.kb.Entries) > l.V1.Limit() {
				t.Fatalf("v1 entries %d > limit %d", len(got.kb.Entries), l.V1.Limit())
			}
		})
	}
}

// 验收 §6.6 第 2 条（桩模型部分）：v1 在全量回归 / 红队样本上答案与 v0 一致（桩模型下卡片由规则层产生），
// 加载命中率（样本 must_cite 中被加载到的比例）不低于 v0。卡片 kb 引用的加载覆盖率只记录不设门禁。
func TestLoadKnowledge_V1HitRateNotBelowV0(t *testing.T) {
	h := newHarness(t)
	l := repoLoading(t, h)
	type tally struct{ total, v0, v1 int }
	var must, cited tally
	count := func(tl *tally, refs []string, v0, v1 *model.KBLoadedPayload, id, what string) {
		for _, ref := range refs {
			tl.total++
			in0, in1 := slices.Contains(v0.Entries, ref), slices.Contains(v1.Entries, ref)
			if in0 {
				tl.v0++
			}
			if in1 {
				tl.v1++
			}
			if in0 && !in1 {
				t.Logf("%s: v1 drops %s %s; v1 = %v", id, what, ref, v1.Entries)
			}
		}
	}
	for _, s := range loadSamples(t) {
		run := func(on bool) loadRun {
			return h.askWithLoading(t, s, func(c *graph.Config) {
				c.Flags = loaderFlags{on: on}
				c.Loading = &graph.KnowledgeLoading{V0: l.V0, V1: l.V1}
			})
		}
		v0, v1 := run(false), run(true)
		if v0.answer != v1.answer || v0.err != v1.err || (v0.kb == nil) != (v1.kb == nil) {
			t.Fatalf("%s: v1 answer differs from v0\nv0: %s %s\nv1: %s %s", s.ID, v0.answer, v0.err, v1.answer, v1.err)
		}
		if v0.kb == nil {
			continue
		}
		if a := s.Expect.Advice; a != nil {
			count(&must, a.MustCite, v0.kb, v1.kb, s.ID, "must_cite")
		}
		count(&cited, kbCited(v0.card), v0.kb, v1.kb, s.ID, "card citation")
	}
	t.Logf("must_cite %d: loaded v0 %d, v1 %d; card kb citations %d: loaded v0 %d, v1 %d",
		must.total, must.v0, must.v1, cited.total, cited.v0, cited.v1)
	if must.total == 0 || must.v1 < must.v0 {
		t.Fatalf("v1 hit rate drops: must_cite %+v, card citations %+v", must, cited)
	}
}

// 下发前复检（runner 外）不写 loader_strategy / kb_load_item（复检不写 run 侧数据）。
func TestLoadKnowledge_RecheckWritesNothing(t *testing.T) {
	h := newHarness(t)
	l := repoLoading(t, h)
	s := findSample(t, loadSamples(t), sForecastOp)
	clk := sampleClock(t, s)
	tools := fixtureTools(t, s, clk)
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := h.newAgent(t, clk, deps{tools: tools, store: newMemStore()}).Ask(context.Background(), q)
	if err != nil || ans.Card == nil {
		t.Fatalf("ask: %v", err)
	}
	q.RunID = ans.RunID
	rec := &loadRecorder{}
	re := h.newAgent(t, clk, deps{tools: tools, store: newMemStore(), cfg: func(c *graph.Config) {
		c.Flags, c.Usage = loaderFlags{on: true}, rec
		c.Loading = &graph.KnowledgeLoading{V0: l.V0, V1: l.V1, Items: rec}
	}})
	if _, err := re.Recheck(context.Background(), agent.RecheckRequest{Question: q, Card: *ans.Card, Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if len(rec.strategies) != 0 || len(rec.items) != 0 {
		t.Fatalf("recheck wrote strategies %v items %d", rec.strategies, len(rec.items))
	}
}

// kbCited 返回卡片 kb 引用（kb:<kb_id>@<version>，去重）。
func kbCited(c *model.AdviceCard) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, ci := range c.Citations {
		if ref := knowledge.CitationRef(ci.KBID, ci.KBVersion); ci.Type == model.CitationKB && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}
