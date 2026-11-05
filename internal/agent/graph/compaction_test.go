package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/harness/checkpoint"
	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/model"
)

// P2 L6 长对话压缩接入主链路的测试（验收 4、5；token 累计）。

// promptRecorder 在桩模型上记录每次调用的 user 消息原文，可注入 token 用量。
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
	usage   *schema.TokenUsage
}

var _ einomodel.BaseChatModel = (*promptRecorder)(nil)

func (p *promptRecorder) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.User {
			p.mu.Lock()
			p.prompts = append(p.prompts, msgs[i].Content)
			p.mu.Unlock()
			break
		}
	}
	out, err := llm.Stub{}.Generate(ctx, msgs, opts...)
	if err != nil || p.usage == nil {
		return out, err
	}
	u := *p.usage
	out.ResponseMeta = &schema.ResponseMeta{Usage: &u}
	return out, nil
}

func (p *promptRecorder) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := p.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}

func (p *promptRecorder) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

// compactionFlags 是 FarmFlagReader：enable_compaction 取 on，err 非空时读失败。
type compactionFlags struct {
	on  bool
	err error
}

func (f compactionFlags) Bool(_ context.Context, key model.FlagKey, _ model.Scope) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return key == model.FlagEnableCompaction && f.on, nil
}

func (f compactionFlags) Mode(context.Context, model.FlagKey, model.Scope) (model.FlagMode, error) {
	return model.FlagModeOff, f.err
}

// sessionSource 是 compaction.Source：返回带 n 轮历史的会话原料，记录读取次数。
type sessionSource struct {
	mu         sync.Mutex
	turns      int
	calls      int
	rejectText string // 非空时带一张以此为驳回原因的作业单（rejection_reason.reason_text 落库未脱敏）
	truncated  bool   // 会话链被截断：带一个更早 run 的约束轮次
}

func (s *sessionSource) LoadSession(_ context.Context, _ model.Scope, runID uint64) (model.SessionSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	src := model.SessionSource{RunID: runID, FieldID: 1}
	for i := range s.turns {
		src.Turns = append(src.Turns, model.SessionTurn{RunID: uint64(100 + i),
			Question: fmt.Sprintf("第%d轮：稻飞虱发生情况怎么样，最近要不要打药，旁边是蜂场", i+1),
			Answer:   fmt.Sprintf("第%d轮回答：先查虫口密度再定是否用药", i+1)})
	}
	if s.truncated {
		src.Truncated, src.EarlierRuns = true, 1
		src.EarlierTurns = []model.SessionTurn{{RunID: 50, Question: "这块地不能用毒死蜱", Answer: "好"}}
	}
	if s.rejectText != "" {
		src.Orders = []model.SessionOrderSource{{SessionOrder: model.SessionOrder{OrderID: 7, Status: model.OrderRejected,
			TicketID: 8, TicketStatus: model.TicketRejected, RejectReasonCode: "OTHER", RejectText: s.rejectText}, RunID: 100}}
	}
	return src, nil
}

func (s *sessionSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// memRaw 是 RawStore：记录转存的原文；err 非空时转存失败。
type memRaw struct {
	mu   sync.Mutex
	puts []model.RawPut
	err  error
}

func (m *memRaw) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return model.RawObject{}, m.err
	}
	m.puts = append(m.puts, p)
	return model.RawObject{ID: uint64(500 + len(m.puts)), Kind: p.Kind}, nil
}

// tokenSink 是 RunUsageStore：记录 AddTokens。
type tokenSink struct {
	mu     sync.Mutex
	calls  int
	prompt int
	compl  int
	err    error
}

func (t *tokenSink) SetLoaderStrategy(context.Context, model.Scope, uint64, string) error { return nil }

func (t *tokenSink) AddTokens(_ context.Context, _ model.Scope, _ uint64, prompt, completion int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	t.prompt += prompt
	t.compl += completion
	return t.err
}

// carrierRuns 包装 memStore，记录 FinishRun 时 ctx 载体里的压缩状态（即终态 UPDATE 会写入的值）。
type carrierRuns struct {
	*memStore
	mu       sync.Mutex
	finState map[uint64]*model.CompactionState
	finCount map[uint64]int
}

func (c *carrierRuns) FinishRun(ctx context.Context, scope model.Scope, runID uint64, st model.RunStatus, reason string) error {
	s, n, _ := agent.CompactionCarrierFrom(ctx).Get()
	c.mu.Lock()
	c.finState[runID], c.finCount[runID] = s, n
	c.mu.Unlock()
	return c.memStore.FinishRun(ctx, scope, runID, st, reason)
}

func newEngine(t *testing.T, src compaction.Source, raw agent.RawStore, clk *env.FrozenClock, trigger int) *compaction.Engine {
	t.Helper()
	lex, err := compaction.LoadLexicon(repoRoot + "/policy/user_constraint_lexicon.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, err := compaction.NewEngine(compaction.Deps{Config: compaction.Config{TriggerTokens: trigger, KeepRecentTurns: 2,
		SummaryMaxTokens: trigger / 3, MaxBlockTokens: 100000, SummaryTimeout: time.Second}, Lexicon: lex, Source: src, Raw: raw, Clock: clk, Mask: pii.Mask})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// askPrompts 在给定配置下跑一次样本，返回各次模型调用的 user 消息与 Answer。
func askPrompts(t *testing.T, sampleID string, store *memStore, cfg func(*graph.Config)) ([]string, agent.Answer) {
	t.Helper()
	return askWith(t, sampleID, &promptRecorder{}, store, cfg)
}

// askWith 同 askPrompts，模型为给定的 promptRecorder（可注入 token 用量）。
func askWith(t *testing.T, sampleID string, rec *promptRecorder, store *memStore, cfg func(*graph.Config)) ([]string, agent.Answer) {
	t.Helper()
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sampleID)
	clk := sampleClock(t, s)
	a := h.newAgent(t, clk, deps{model: rec, tools: fixtureTools(t, s, clk), store: store, cfg: cfg})
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := a.Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	return rec.all(), ans
}

// 验收 5（先写）：enable_compaction 关 / 读失败 / 未装配 Reader 时，组装出的 prompt 与 P1（未配置 Compaction）逐字节相同，
// 且不进 compaction 包任何分支（Source 零读取、零事件）。
func TestCompaction_OffPromptByteIdentical(t *testing.T) {
	base, baseAns := askPrompts(t, sOperation, newMemStore(), nil)
	if len(base) < 2 {
		t.Fatalf("baseline made %d llm calls, want >= 2", len(base))
	}
	for name, flags := range map[string]agent.FarmFlagReader{
		"flag off":         compactionFlags{on: false},
		"flag read failed": compactionFlags{err: errors.New("redis down")},
		"no flag reader":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			src, ev := &sessionSource{turns: 8}, &recordingEvents{}
			got, ans := askPrompts(t, sOperation, newMemStore(), func(c *graph.Config) {
				c.Flags, c.Events = flags, ev
				c.Compaction = newEngine(t, src, &memRaw{}, sampleClock(t, findSample(t, loadSamples(t), sOperation)), 80)
			})
			if strings.Join(got, "\x00") != strings.Join(base, "\x00") {
				t.Fatalf("prompts differ from P1:\n got=%q\nwant=%q", got, base)
			}
			if src.count() != 0 {
				t.Fatalf("compaction source read %d times with switch off", src.count())
			}
			for _, typ := range ev.list() {
				if typ == model.EventCompactionApplied || typ == model.EventCompactionFailed {
					t.Fatalf("compaction event %s with switch off", typ)
				}
			}
			if ans.Outcome != baseAns.Outcome || ans.Card.Conclusion != baseAns.Card.Conclusion {
				t.Fatalf("answer %s/%q, want %s/%q", ans.Outcome, ans.Card.Conclusion, baseAns.Outcome, baseAns.Card.Conclusion)
			}
		})
	}
}

func newCarrierRuns(m *memStore) *carrierRuns {
	return &carrierRuns{memStore: m, finState: map[uint64]*model.CompactionState{}, finCount: map[uint64]int{}}
}

func (c *carrierRuns) finished(runID uint64) (*model.CompactionState, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finState[runID], c.finCount[runID]
}

// onCfg 装配 enable_compaction 开的压缩（trigger 80：8 轮合成历史必触发）；runs 非空时作 Runs。
func onCfg(t *testing.T, src compaction.Source, raw *memRaw, ev agent.EventSink, runs *carrierRuns) func(*graph.Config) {
	t.Helper()
	e := newEngine(t, src, raw, sampleClock(t, findSample(t, loadSamples(t), sOperation)), 80)
	return func(c *graph.Config) {
		c.Flags, c.Events, c.Compaction = compactionFlags{on: true}, ev, e
		if runs != nil {
			c.Runs = runs
		}
	}
}

// taskInput 解出 user 消息里的任务入参（逐键原始字节）。
func taskInput(t *testing.T, prompt string) map[string]json.RawMessage {
	t.Helper()
	var msg struct {
		Input map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(prompt), &msg); err != nil {
		t.Fatalf("decode prompt %q: %v", prompt, err)
	}
	return msg.Input
}

// 验收 4（代码 + 合成数据）：开关开且超过阈值时，本 run 每次模型调用都带同一会话块（只组装一次），
// 其余入参逐字节同 P1；被压缩原文转存 COMPACTED_MSGS，发 COMPACTION_APPLIED，终态写入的压缩状态带摘要与原文引用。
func TestCompaction_OnAppendsSessionOnce(t *testing.T) {
	base, baseAns := askPrompts(t, sOperation, newMemStore(), nil)
	src, raw, ev := &sessionSource{turns: 8}, &memRaw{}, &recordingEvents{}
	runs := newCarrierRuns(newMemStore())
	got, ans := askPrompts(t, sOperation, runs.memStore, onCfg(t, src, raw, ev, runs))

	if len(got) != len(base) {
		t.Fatalf("llm calls = %d, want %d", len(got), len(base))
	}
	var block json.RawMessage
	for i := range got {
		in, want := taskInput(t, got[i]), taskInput(t, base[i])
		sess, ok := in["session"]
		if !ok {
			t.Fatalf("call %d has no session block: %s", i, got[i])
		}
		if block == nil {
			block = sess
		} else if string(sess) != string(block) {
			t.Fatalf("call %d session block differs from first call", i)
		}
		delete(in, "session")
		if len(in) != len(want) {
			t.Fatalf("call %d input keys = %d, want %d", i, len(in), len(want))
		}
		for k, v := range want {
			if string(in[k]) != string(v) {
				t.Fatalf("call %d input %q = %s, want %s", i, k, in[k], v)
			}
		}
	}
	var b compaction.Block
	if err := json.Unmarshal(block, &b); err != nil {
		t.Fatalf("decode session block: %v", err)
	}
	if b.Summary == nil || len(b.Turns) != 2 || !strings.Contains(b.Turns[1].Question, "第8轮") {
		t.Fatalf("session block summary=%v turns=%+v, want summary + last 2 turns", b.Summary, b.Turns)
	}
	if n := src.count(); n != 2 {
		t.Fatalf("source reads = %d, want 2 (assemble + assert reread, once per run)", n)
	}
	if len(raw.puts) != 1 || raw.puts[0].Kind != model.RawCompactedMsgs {
		t.Fatalf("raw puts = %+v, want one COMPACTED_MSGS", raw.puts)
	}
	if a, f := countType(ev.list(), model.EventCompactionApplied), countType(ev.list(), model.EventCompactionFailed); a != 1 || f != 0 {
		t.Fatalf("APPLIED=%d FAILED=%d, want 1/0", a, f)
	}
	if ans.Outcome != baseAns.Outcome {
		t.Fatalf("outcome = %s, want %s", ans.Outcome, baseAns.Outcome)
	}
	st, n := runs.finished(ans.RunID)
	if st == nil || st.Summary == nil || len(st.RawRefs) != 1 || st.CompactionVersion != model.CompactionVersion || n != 1 {
		t.Fatalf("finish carrier state=%+v count=%d, want compacted state with 1 raw ref and count 1", st, n)
	}
}

// 失败关闭（红线）：原文转存失败时放弃压缩，发 COMPACTION_FAILED，会话块退回完整上下文（全部轮次原文），不写压缩状态。
func TestCompaction_FailureFallsBackToFullContext(t *testing.T) {
	src, raw, ev := &sessionSource{turns: 8}, &memRaw{err: errors.New("minio down")}, &recordingEvents{}
	runs := newCarrierRuns(newMemStore())
	got, ans := askPrompts(t, sOperation, runs.memStore, onCfg(t, src, raw, ev, runs))
	if ans.Card == nil {
		t.Fatal("no card after compaction failure")
	}
	var b compaction.Block
	if err := json.Unmarshal(taskInput(t, got[0])["session"], &b); err != nil {
		t.Fatalf("decode session block: %v", err)
	}
	if b.Summary != nil || len(b.Turns) != 8 {
		t.Fatalf("fallback block summary=%v turns=%d, want full context (no summary, 8 turns)", b.Summary, len(b.Turns))
	}
	if a, f := countType(ev.list(), model.EventCompactionApplied), countType(ev.list(), model.EventCompactionFailed); a != 0 || f != 1 {
		t.Fatalf("APPLIED=%d FAILED=%d, want 0/1", a, f)
	}
	if st, n := runs.finished(ans.RunID); st != nil || n != 0 {
		t.Fatalf("finish carrier state=%+v count=%d, want none", st, n)
	}
}

// 脱敏（P2 L6 / PII）：驳回原因原文带手机号时，会话块、终态写入的 compaction_state 与 COMPACTION_FAILED 原因都不含原号。
const testPhone = "13812345678"

func TestCompaction_PIIMasked(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		src, raw, ev := &sessionSource{turns: 8, rejectText: "联系人" + testPhone + "说药不对"}, &memRaw{}, &recordingEvents{}
		runs := newCarrierRuns(newMemStore())
		got, ans := askPrompts(t, sOperation, runs.memStore, onCfg(t, src, raw, ev, runs))
		for i, p := range got {
			if strings.Contains(p, testPhone) {
				t.Fatalf("prompt %d leaks phone: %s", i, p)
			}
		}
		st, _ := runs.finished(ans.RunID)
		if st == nil || len(st.Facts.Orders) != 1 || st.Facts.Orders[0].RejectText != pii.Mask(src.rejectText) {
			t.Fatalf("finish state = %+v, want masked reject text", st)
		}
		body, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), testPhone) {
			t.Fatalf("compaction_state leaks phone: %s", body)
		}
	})
	t.Run("failed reason", func(t *testing.T) {
		src, ev := &sessionSource{turns: 8}, &recordingEvents{}
		raw := &memRaw{err: errors.New("minio down, operator " + testPhone)}
		askPrompts(t, sOperation, newMemStore(), onCfg(t, src, raw, ev, nil))
		ev.mu.Lock()
		defer ev.mu.Unlock()
		n := 0
		for _, e := range ev.events {
			if e.Type != model.EventCompactionFailed {
				continue
			}
			n++
			if strings.Contains(string(e.Payload), testPhone) || !strings.Contains(string(e.Payload), "minio down") ||
				!strings.Contains(string(e.Payload), `"kind":"`+compaction.KindFallback+`"`) {
				t.Fatalf("COMPACTION_FAILED payload = %s, want masked reason", e.Payload)
			}
		}
		if n != 1 {
			t.Fatalf("COMPACTION_FAILED events = %d, want 1", n)
		}
	})
}

// 会话链截断：照常压缩（APPLIED），另发一条 kind=SESSION_TRUNCATED 的 COMPACTION_FAILED；会话块带更早 run 的约束。
func TestCompaction_TruncatedChainEvent(t *testing.T) {
	src, ev := &sessionSource{turns: 8, truncated: true}, &recordingEvents{}
	got, _ := askPrompts(t, sOperation, newMemStore(), onCfg(t, src, &memRaw{}, ev, nil))
	if !strings.Contains(string(taskInput(t, got[0])["session"]), "这块地不能用毒死蜱") {
		t.Fatalf("session block lacks earlier constraint: %s", got[0])
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	applied, kinds := 0, []string{}
	for _, e := range ev.events {
		switch e.Type {
		case model.EventCompactionApplied:
			applied++
		case model.EventCompactionFailed:
			var p struct{ Kind, Reason string }
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode payload %s: %v", e.Payload, err)
			}
			kinds = append(kinds, p.Kind)
		}
	}
	if applied != 1 || len(kinds) != 1 || kinds[0] != compaction.KindTruncated {
		t.Fatalf("APPLIED=%d FAILED kinds=%v, want 1 / [%s]", applied, kinds, compaction.KindTruncated)
	}
}

// 首轮（会话无上一轮）开关开也不追加会话块：prompt 与 P1 逐字节相同，无压缩事件。
func TestCompaction_FirstTurnNoSession(t *testing.T) {
	base, _ := askPrompts(t, sOperation, newMemStore(), nil)
	src, ev := &sessionSource{}, &recordingEvents{}
	got, _ := askPrompts(t, sOperation, newMemStore(), onCfg(t, src, &memRaw{}, ev, nil))
	if strings.Join(got, "\x00") != strings.Join(base, "\x00") {
		t.Fatalf("first-turn prompts differ from P1:\n got=%q\nwant=%q", got, base)
	}
	if n := countType(ev.list(), model.EventCompactionApplied) + countType(ev.list(), model.EventCompactionFailed); n != 0 {
		t.Fatalf("compaction events = %d on first turn", n)
	}
}

// token 累计：每次模型调用后以 LLM 返回的用量调用 AddTokens；用量为 0（stub）不写；写失败不让 run 失败。
func TestCompaction_AddTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		usage     *schema.TokenUsage
		sinkErr   error
		wantCalls bool
	}{
		"usage":      {usage: &schema.TokenUsage{PromptTokens: 11, CompletionTokens: 3}, wantCalls: true},
		"zero usage": {usage: nil},
		"store fail": {usage: &schema.TokenUsage{PromptTokens: 5, CompletionTokens: 2}, sinkErr: errors.New("db down"), wantCalls: true},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &tokenSink{err: tc.sinkErr}
			got, ans := askWith(t, sOperation, &promptRecorder{usage: tc.usage}, newMemStore(),
				func(c *graph.Config) { c.Usage = sink })
			if ans.Card == nil {
				t.Fatal("no card")
			}
			if !tc.wantCalls {
				if sink.calls != 0 {
					t.Fatalf("AddTokens calls = %d, want 0", sink.calls)
				}
				return
			}
			n := len(got)
			if sink.calls != n || sink.prompt != n*tc.usage.PromptTokens || sink.compl != n*tc.usage.CompletionTokens {
				t.Fatalf("AddTokens calls=%d prompt=%d completion=%d, want %d/%d/%d", sink.calls, sink.prompt, sink.compl,
					n, n*tc.usage.PromptTokens, n*tc.usage.CompletionTokens)
			}
		})
	}
}

// 中断 / 续跑（代码 + 注入时钟）：中断时压缩状态随检查点快照落盘；新建 agent（模拟进程重启）Resume 后由快照恢复载体，
// 终态写入同一压缩状态与计数；续跑不再读会话原料、不调模型、已完成节点零重复。
func TestCompaction_StateSurvivesResume(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	clk := sampleClock(t, s)
	store, hitl, ev := newMemStore(), newHITLStore(), &recordingEvents{}
	runs, tools := newCarrierRuns(store), newCountingTools(fixtureTools(t, s, clk))
	src, raw, rec := &sessionSource{turns: 8}, &memRaw{}, &promptRecorder{}
	newA := func() *graph.Agent {
		on := onCfg(t, src, raw, ev, runs)
		return h.newAgent(t, clk, deps{model: rec, tools: tools, store: store, cfg: func(c *graph.Config) {
			withHITL(hitl, ev)(c)
			on(c)
		}})
	}
	q, err := questionOf(s)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := newA().Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if ans.Ticket == nil {
		t.Fatal("operation sample did not open a ticket")
	}
	hitl.mu.Lock()
	cps := hitl.checkpoints[checkpoint.ID(ans.RunID)]
	hitl.mu.Unlock()
	if len(cps) != 1 || !strings.Contains(string(cps[0]), "compaction_state") {
		t.Fatalf("checkpoints = %d, want 1 carrying compaction_state", len(cps))
	}
	if st, _ := runs.finished(ans.RunID); st != nil {
		t.Fatal("interrupted run must not be finished")
	}
	reads, calls, toolCalls := src.count(), len(rec.all()), tools.total()

	hitl.approve(ans.RunID)
	if _, err := newA().Resume(context.Background(), testScope, ans.RunID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if src.count() != reads || len(rec.all()) != calls || tools.total() != toolCalls {
		t.Fatalf("resume re-read source %d→%d / llm %d→%d / tools %d→%d, want no change",
			reads, src.count(), calls, len(rec.all()), toolCalls, tools.total())
	}
	for _, n := range []string{graph.NodeClassify, graph.NodeExplain, graph.NodeToOrder} {
		if c := startedCount(store, ans.RunID, n); c != 1 {
			t.Fatalf("node %s started %d times, want 1", n, c)
		}
	}
	st, n := runs.finished(ans.RunID)
	if st == nil || st.Summary == nil || len(st.RawRefs) != 1 || n != 1 {
		t.Fatalf("finish carrier after resume state=%+v count=%d, want restored compacted state, count 1", st, n)
	}
	if len(raw.puts) != 1 {
		t.Fatalf("raw puts = %d, want 1 (no re-compaction on resume)", len(raw.puts))
	}
}
