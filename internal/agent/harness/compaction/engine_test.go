package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/model"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// fakeSource 依次返回 srcs（最后一个重复使用）；err 非空时每次返回错误。
type fakeSource struct {
	srcs  []model.SessionSource
	calls int
	err   error
}

func (f *fakeSource) LoadSession(_ context.Context, _ model.Scope, _ uint64) (model.SessionSource, error) {
	if f.err != nil {
		return model.SessionSource{}, f.err
	}
	i := min(f.calls, len(f.srcs)-1)
	f.calls++
	return f.srcs[i], nil
}

type fakeRaw struct {
	puts []model.RawPut
	err  error
}

func (f *fakeRaw) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	if f.err != nil {
		return model.RawObject{}, f.err
	}
	f.puts = append(f.puts, p)
	return model.RawObject{ID: uint64(900 + len(f.puts)), Kind: p.Kind}, nil
}

type badSummarizer struct {
	text string
	err  error
}

func (b badSummarizer) Summarize(context.Context, string, []Turn, int) (string, string, error) {
	return b.text, "bad", b.err
}

var scope = model.Scope{TenantID: 1, FarmID: 2, UserID: 3}

func newEngine(t *testing.T, src *fakeSource, raw *fakeRaw, cfg Config, s Summarizer) *Engine {
	t.Helper()
	e, err := NewEngine(Deps{Config: cfg, Lexicon: testLexicon(t), Source: src, Raw: raw, Summarizer: s,
		Clock: fixedClock{t0}, Mask: pii.Mask})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// tightConfig 让 testSource（6 轮）超过阈值：保留最近 2 轮。
var tightConfig = Config{TriggerTokens: 80, KeepRecentTurns: 2, SummaryMaxTokens: 30, MaxBlockTokens: 100000,
	SummaryTimeout: time.Second}

// roomyConfig 让 testSource 低于阈值（不压缩）。
var roomyConfig = Config{TriggerTokens: 100000, KeepRecentTurns: 2, SummaryMaxTokens: 60, MaxBlockTokens: 200000,
	SummaryTimeout: time.Second}

func decodeBlock(t *testing.T, b []byte) Block {
	t.Helper()
	var out Block
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode block: %v", err)
	}
	return out
}

func TestNewEngine_Validates(t *testing.T) {
	if _, err := NewEngine(Deps{Config: tightConfig}); err == nil || !strings.Contains(err.Error(), "Lexicon") {
		t.Fatalf("err = %v, want missing deps", err)
	}
	for name, mutate := range map[string]func(c *Config){
		"summary >= trigger": func(c *Config) { c.SummaryMaxTokens = c.TriggerTokens },
		"max <= trigger":     func(c *Config) { c.MaxBlockTokens = c.TriggerTokens },
		"no timeout":         func(c *Config) { c.SummaryTimeout = 0 },
	} {
		bad := tightConfig
		mutate(&bad)
		if _, err := NewEngine(Deps{Config: bad, Lexicon: testLexicon(t), Source: &fakeSource{}, Raw: &fakeRaw{},
			Clock: fixedClock{t0}, Mask: pii.Mask}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAssemble_FirstTurnSendsNothing(t *testing.T) {
	src := &fakeSource{srcs: []model.SessionSource{{RunID: 1, FieldID: 9}}}
	res := newEngine(t, src, &fakeRaw{}, tightConfig, nil).Assemble(context.Background(), Request{Scope: scope, RunID: 1})
	if res.Block != nil || res.State != nil || res.Applied != nil || res.Failed != "" {
		t.Fatalf("first turn: %+v, want zero result", res)
	}
}

func TestAssemble_UnderTriggerSendsFullContext(t *testing.T) {
	src := &fakeSource{srcs: []model.SessionSource{testSource()}}
	cfg := roomyConfig
	res := newEngine(t, src, &fakeRaw{}, cfg, nil).Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	if res.Failed != "" || res.State != nil || res.Applied != nil {
		t.Fatalf("under trigger: %+v", res)
	}
	b := decodeBlock(t, res.Block)
	if b.Summary != nil || len(b.Turns) != 6 || b.Facts.FieldID != 9 {
		t.Fatalf("block = %+v, want facts + 6 turns, no summary", b)
	}
	if src.calls != 1 {
		t.Fatalf("source read %d times, want 1 (no summary, no assertion)", src.calls)
	}
}

func TestAssemble_CompactsOverTrigger(t *testing.T) {
	src, raw := &fakeSource{srcs: []model.SessionSource{testSource()}}, &fakeRaw{}
	res := newEngine(t, src, raw, tightConfig, nil).Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	if res.Failed != "" {
		t.Fatalf("failed: %s", res.Failed)
	}
	if res.Applied == nil || res.Applied.TokensAfter >= res.Applied.TokensBefore {
		t.Fatalf("applied = %+v, want tokens to drop", res.Applied)
	}
	b := decodeBlock(t, res.Block)
	if free := freeTokens(b); free > tightConfig.TriggerTokens {
		t.Fatalf("free tokens after = %d > trigger %d: would re-trigger every turn", free, tightConfig.TriggerTokens)
	}
	if b.Summary == nil || len(b.Turns) != 2 || b.Turns[0].StepID != "run-105" {
		t.Fatalf("block turns = %+v summary = %v, want summary + run-105/106", b.Turns, b.Summary)
	}
	if src.calls != 2 {
		t.Fatalf("source read %d times, want 2 (assertion re-reads)", src.calls)
	}
	if err := Assert(res.Block, testSource(), testLexicon(t)); err != nil {
		t.Fatalf("emitted block fails assertion: %v", err)
	}
	st := res.State
	if st == nil || st.SummarizedUntilStepID != "run-104" || len(st.RawRefs) != 1 || st.RawRefs[0] != 901 ||
		len(st.History) != 1 || !st.History[0].At.Equal(t0) || st.Summary.Tokens > tightConfig.SummaryMaxTokens {
		t.Fatalf("state = %+v", st)
	}
	if len(raw.puts) != 1 || raw.puts[0].Kind != model.RawCompactedMsgs || raw.puts[0].RunID != 107 ||
		raw.puts[0].Name != "compacted-107-until-run-104" {
		t.Fatalf("raw puts = %+v", raw.puts)
	}
	rec, err := DecodeRaw(raw.puts[0].Body)
	if err != nil || len(rec.Turns) != 4 || rec.Turns[0].Question != testSource().Turns[0].Question {
		t.Fatalf("raw record = %+v err=%v, want 4 original turns", rec, err)
	}
}

// 续接：上一 run 的状态复制到本 run，新增一轮后仍低于阈值，摘要 + 边界之后的原文继续使用、不再压缩。
func TestAssemble_ReusesCopiedState(t *testing.T) {
	first := &fakeSource{srcs: []model.SessionSource{testSource()}}
	cfg := tightConfig
	prev := newEngine(t, first, &fakeRaw{}, cfg, nil).Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	state, err := json.Marshal(prev.State)
	if err != nil {
		t.Fatal(err)
	}
	next := testSource()
	next.RunID, next.State = 108, state
	next.Turns = append(next.Turns, model.SessionTurn{RunID: 107, Question: "好", Answer: "嗯"})
	raw := &fakeRaw{}
	cfg.TriggerTokens, cfg.MaxBlockTokens = 100000, 200000
	res := newEngine(t, &fakeSource{srcs: []model.SessionSource{next}}, raw, cfg, nil).
		Assemble(context.Background(), Request{Scope: scope, RunID: 108})
	if res.Failed != "" || res.Applied != nil || len(raw.puts) != 0 {
		t.Fatalf("reuse: %+v puts=%d", res, len(raw.puts))
	}
	b := decodeBlock(t, res.Block)
	if b.Summary == nil || b.Summary.Text != prev.State.Summary.Text || len(b.Turns) != 3 {
		t.Fatalf("block = %+v, want carried summary + 3 turns after run-104", b)
	}
	if res.State == nil || res.State.SummarizedUntilStepID != "run-104" || len(res.State.RawRefs) != 1 {
		t.Fatalf("state = %+v, want carried", res.State)
	}
}

func TestAssemble_UnknownVersionIsUncompressed(t *testing.T) {
	src := testSource()
	src.State = json.RawMessage(`{"compaction_version":99,"facts":{"field_id":9},"summary":{"text":"x","tokens":1,"prompt_version":"v"},"summarized_until_step_id":"run-104"}`)
	cfg := roomyConfig
	res := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, cfg, nil).
		Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	b := decodeBlock(t, res.Block)
	if res.State != nil || b.Summary != nil || len(b.Turns) != 6 {
		t.Fatalf("unknown version: state=%v block=%+v, want full context", res.State, b)
	}
}

// 失败关闭：任一环节失败都放弃压缩，退回完整上下文（facts + 全部原文），不写新状态。
func TestAssemble_FailsClosed(t *testing.T) {
	changed := testSource()
	changed.Orders[1].Status = model.OrderCanceled
	for name, tc := range map[string]struct {
		src  *fakeSource
		raw  *fakeRaw
		sum  Summarizer
		want string
	}{
		"source changed before assertion": {src: &fakeSource{srcs: []model.SessionSource{testSource(), changed}},
			raw: &fakeRaw{}, want: "whitelist"},
		"summarizer error": {src: &fakeSource{srcs: []model.SessionSource{testSource()}}, raw: &fakeRaw{},
			sum: badSummarizer{err: errors.New("model down")}, want: "model down"},
		"summary over budget": {src: &fakeSource{srcs: []model.SessionSource{testSource()}}, raw: &fakeRaw{},
			sum: badSummarizer{text: strings.Repeat("摘", 31)}, want: "max"},
		"raw put error": {src: &fakeSource{srcs: []model.SessionSource{testSource()}},
			raw: &fakeRaw{err: errors.New("minio down")}, want: "minio down"},
	} {
		t.Run(name, func(t *testing.T) {
			res := newEngine(t, tc.src, tc.raw, tightConfig, tc.sum).Assemble(context.Background(), Request{Scope: scope, RunID: 107})
			if !strings.Contains(res.Failed, tc.want) {
				t.Fatalf("failed = %q, want %q", res.Failed, tc.want)
			}
			if res.State != nil || res.Applied != nil {
				t.Fatalf("state=%v applied=%v, want none", res.State, res.Applied)
			}
			b := decodeBlock(t, res.Block)
			if b.Summary != nil || len(b.Turns) != 6 {
				t.Fatalf("fallback block = %+v, want full context", b)
			}
			// 回退的完整上下文须与最后一次读出的源表一致（首读后业务表变化时不得发过时事实）。
			latest := tc.src.srcs[len(tc.src.srcs)-1]
			if err := Assert(res.Block, latest, testLexicon(t)); err != nil {
				t.Fatalf("fallback block vs latest source: %v", err)
			}
		})
	}
}

// 首读后作业单被取消、票被驳回：断言失败回退时，完整上下文按重读的源表重建，首读的过时事实不得出现。
func TestAssemble_FallbackUsesReread(t *testing.T) {
	changed := testSource()
	changed.Orders[1].Status = model.OrderCanceled
	changed.Orders[1].TicketStatus = model.TicketRejected
	src := &fakeSource{srcs: []model.SessionSource{testSource(), changed}}
	res := newEngine(t, src, &fakeRaw{}, tightConfig, nil).Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	if !strings.Contains(res.Failed, "whitelist") {
		t.Fatalf("failed = %q, want whitelist", res.Failed)
	}
	if err := Assert(res.Block, changed, testLexicon(t)); err != nil {
		t.Fatalf("fallback vs reread: %v", err)
	}
	if err := Assert(res.Block, testSource(), testLexicon(t)); !errors.Is(err, ErrWhitelist) {
		t.Fatalf("fallback still carries stale first-read facts: err = %v", err)
	}
	if b := decodeBlock(t, res.Block); b.Facts.Orders[1].Status != model.OrderCanceled {
		t.Fatalf("orders = %+v, want reread status CANCELED", b.Facts.Orders)
	}
}

func TestAssemble_LoadErrorSendsNothing(t *testing.T) {
	res := newEngine(t, &fakeSource{err: fmt.Errorf("db down")}, &fakeRaw{}, tightConfig, nil).
		Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	if res.Block != nil || !strings.Contains(res.Failed, "db down") {
		t.Fatalf("load error: %+v", res)
	}
}

func TestDeterministic_TruncatesToBudget(t *testing.T) {
	text, v, err := Deterministic{}.Summarize(context.Background(), "旧摘要", turnsOf(testSource().Turns), 20)
	if err != nil || v != DeterministicPromptVersion || EstimateTokens(text) > 20 || !strings.HasPrefix(text, "旧摘要") {
		t.Fatalf("text=%q v=%q err=%v", text, v, err)
	}
}

func TestEstimateTokens(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abcd": 1, "abcde": 2, "稻飞虱": 3, "稻a": 2} {
		if got := EstimateTokens(in); got != want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLexicon(t *testing.T) {
	got := testLexicon(t).Constraints("旁边是蜂场！稻飞虱怎么治？\n这块地不能用吡虫啉；明天下雨")
	if len(got) != 2 || got[0] != "旁边是蜂场" || got[1] != "这块地不能用吡虫啉" {
		t.Fatalf("constraints = %q", got)
	}
	if _, err := NewLexicon(1, "a", " A "); !errors.Is(err, ErrBadLexicon) {
		t.Fatalf("duplicate keyword: err = %v", err)
	}
	if _, err := NewLexicon(0, "a"); !errors.Is(err, ErrBadLexicon) {
		t.Fatalf("version 0: err = %v", err)
	}
	if _, err := LoadLexicon("../../../../policy/user_constraint_lexicon.yaml"); err != nil {
		t.Fatalf("repo lexicon: %v", err)
	}
}

// 脱敏：源表自由文本（驳回原因、问题、回答、结论）带手机号时，会话块、compaction_state 与原文转存都只含掩码；
// 断言对重读的源同样脱敏后比对，照常通过。
func TestAssemble_MasksFreeText(t *testing.T) {
	const phone = "13812345678"
	dirty := testSource()
	dirty.Orders[0].RejectText = "旁边有鱼塘，电话" + phone
	dirty.Turns[0].Question = "我电话" + phone + "，旁边是蜂场"
	dirty.Turns[5].Answer = "有问题打" + phone
	dirty.Cards[2].Conclusion = "明早作业，联系" + phone
	raw := &fakeRaw{}
	res := newEngine(t, &fakeSource{srcs: []model.SessionSource{dirty}}, raw, tightConfig, nil).
		Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	if res.Failed != "" || res.State == nil || res.Applied == nil {
		t.Fatalf("res = %+v, want applied", res)
	}
	st, err := json.Marshal(res.State)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"block": res.Block, "state": st, "raw": raw.puts[0].Body} {
		if strings.Contains(string(b), phone) {
			t.Fatalf("%s leaks phone: %s", name, b)
		}
	}
	if got := res.State.Facts.Orders[0].RejectText; got != pii.Mask(dirty.Orders[0].RejectText) || got == dirty.Orders[0].RejectText {
		t.Fatalf("reject text = %q", got)
	}
	if dirty.Orders[0].RejectText != "旁边有鱼塘，电话"+phone {
		t.Fatal("sanitize mutated the source")
	}
}

// blockingSummarizer 阻塞到 ctx 结束（模拟轻量档模型无响应）；ignoreCtx 时超时后才返回成功。
type blockingSummarizer struct{ ignoreCtx bool }

func (b blockingSummarizer) Summarize(ctx context.Context, _ string, _ []Turn, _ int) (string, string, error) {
	if b.ignoreCtx {
		<-ctx.Done()
		return "迟到的摘要", "slow", nil
	}
	<-ctx.Done()
	return "", "", ctx.Err()
}

// 摘要超时：按 SummaryTimeout 取消，走 COMPACTION_FAILED 回退完整上下文；摘要器无视 ctx 迟到返回也放弃。
func TestAssemble_SummaryTimeoutFailsClosed(t *testing.T) {
	cfg := tightConfig
	cfg.SummaryTimeout = 20 * time.Millisecond
	for name, sum := range map[string]Summarizer{"honors ctx": blockingSummarizer{}, "ignores ctx": blockingSummarizer{ignoreCtx: true}} {
		t.Run(name, func(t *testing.T) {
			res := newEngine(t, &fakeSource{srcs: []model.SessionSource{testSource()}}, &fakeRaw{}, cfg, sum).
				Assemble(context.Background(), Request{Scope: scope, RunID: 107})
			if !strings.Contains(res.Failed, context.DeadlineExceeded.Error()) || res.State != nil || res.Applied != nil {
				t.Fatalf("res = %+v, want deadline fallback", res)
			}
			if b := decodeBlock(t, res.Block); b.Summary != nil || len(b.Turns) != 6 {
				t.Fatalf("fallback block = %+v, want full context", b)
			}
		})
	}
}

// 硬上限：facts 计入估算。超出时由早到晚丢原文轮次、再丢摘要，facts 一条不裁；Trimmed 记裁剪。
func TestAssemble_MaxBlockTokensTrims(t *testing.T) {
	lex, src := testLexicon(t), testSource()
	full := blockTokens(fullOf(src, lex))
	t.Run("full context trimmed oldest first", func(t *testing.T) {
		// 轮次少于 keep_recent_turns：不触发压缩，只受硬上限约束（比全量少 1 token，丢最早一轮即够）
		cfg := Config{TriggerTokens: 10, KeepRecentTurns: 100, SummaryMaxTokens: 5, MaxBlockTokens: full - 1,
			SummaryTimeout: time.Second}
		res := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, cfg, nil).
			Assemble(context.Background(), Request{Scope: scope, RunID: 107})
		b := decodeBlock(t, res.Block)
		if !strings.HasPrefix(res.Trimmed, "trimmed:") || res.Failed != "" || res.State != nil || len(b.Turns) != 5 ||
			b.Turns[0].StepID != "run-102" {
			t.Fatalf("trimmed=%q failed=%q turns=%+v, want oldest turn dropped", res.Trimmed, res.Failed, b.Turns)
		}
		if EstimateTokens(string(res.Block)) > cfg.MaxBlockTokens {
			t.Fatalf("block %d tokens > max %d", EstimateTokens(string(res.Block)), cfg.MaxBlockTokens)
		}
		if err := Assert(mustEncode(t, Block{CompactionVersion: b.CompactionVersion, Facts: b.Facts,
			Turns: turnsOf(src.Turns)}), src, lex); err != nil {
			t.Fatalf("trimmed block lost whitelist facts: %v", err)
		}
	})
	t.Run("facts never trimmed", func(t *testing.T) {
		cfg := tightConfig
		cfg.MaxBlockTokens = cfg.TriggerTokens + 1 // 远小于 facts 本身
		res := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, cfg, nil).
			Assemble(context.Background(), Request{Scope: scope, RunID: 107})
		b := decodeBlock(t, res.Block)
		if len(b.Turns) != 0 || b.Summary != nil || !strings.Contains(res.Trimmed, "facts alone") {
			t.Fatalf("trimmed=%q block=%+v, want facts only", res.Trimmed, b)
		}
		want := BuildFacts(src, lex)
		got, err := json.Marshal(b.Facts)
		if err != nil {
			t.Fatal(err)
		}
		if w, err := json.Marshal(want); err != nil || string(got) != string(w) {
			t.Fatalf("facts trimmed: got %s want %s err=%v", got, w, err)
		}
	})
	t.Run("compressed block trimmed before assertion", func(t *testing.T) {
		cfg := tightConfig
		cfg.KeepRecentTurns = 3
		probe := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, cfg, nil).
			Assemble(context.Background(), Request{Scope: scope, RunID: 107})
		if probe.Applied == nil {
			t.Fatalf("probe not compressed: %+v", probe)
		}
		cfg.MaxBlockTokens = EstimateTokens(string(probe.Block)) - 1
		res := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, cfg, nil).
			Assemble(context.Background(), Request{Scope: scope, RunID: 107})
		b := decodeBlock(t, res.Block)
		if !strings.HasPrefix(res.Trimmed, "trimmed:") || res.Failed != "" || res.Applied == nil || b.Summary == nil ||
			len(b.Turns) != 2 {
			t.Fatalf("trimmed=%q failed=%q applied=%v turns=%d, want summary + 2 turns", res.Trimmed, res.Failed, res.Applied, len(b.Turns))
		}
		if err := Assert(res.Block, src, lex); err != nil {
			t.Fatalf("trimmed compressed block: %v", err)
		}
	})
}

// 已有状态解码失败按无压缩处理（不报错、不 panic），用完整上下文。
func TestAssemble_CorruptStateIsUncompressed(t *testing.T) {
	src := testSource()
	src.State = json.RawMessage(`{"compaction_version":`)
	res := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, roomyConfig, nil).
		Assemble(context.Background(), Request{Scope: scope, RunID: 107})
	if b := decodeBlock(t, res.Block); res.State != nil || b.Summary != nil || len(b.Turns) != 6 {
		t.Fatalf("corrupt state: res=%+v", res)
	}
}

// 会话链截断：Truncated 记原因（压缩照常进行），facts 带更早 run 的约束 / 否决，其中自由文本同样脱敏。
func TestAssemble_TruncatedChainAggregates(t *testing.T) {
	const phone = "13812345678"
	src := truncatedSource()
	src.EarlierTurns[0].Question += "，有事打" + phone
	src.EarlierOrders[0].RejectText += "，联系" + phone
	for name, cfg := range map[string]Config{"compacted": tightConfig, "full context": roomyConfig} {
		t.Run(name, func(t *testing.T) {
			res := newEngine(t, &fakeSource{srcs: []model.SessionSource{src}}, &fakeRaw{}, cfg, nil).
				Assemble(context.Background(), Request{Scope: scope, RunID: 107})
			if res.Failed != "" || !strings.Contains(res.Truncated, "earlier 3 runs") {
				t.Fatalf("failed=%q truncated=%q", res.Failed, res.Truncated)
			}
			if (cfg == tightConfig) != (res.Applied != nil) {
				t.Fatalf("applied = %+v under %s", res.Applied, name)
			}
			if strings.Contains(string(res.Block), phone) {
				t.Fatalf("block leaks phone: %s", res.Block)
			}
			b := decodeBlock(t, res.Block)
			if len(b.Facts.VetoedProducts) != 2 || b.Facts.VetoedProducts[0].Product != "毒死蜱" ||
				b.Facts.UserConstraints[0].TurnStepID != "run-60" {
				t.Fatalf("facts = %+v, want earlier veto / constraint", b.Facts)
			}
		})
	}
	if got := newEngine(t, &fakeSource{srcs: []model.SessionSource{testSource()}}, &fakeRaw{}, tightConfig, nil).
		Assemble(context.Background(), Request{Scope: scope, RunID: 107}); got.Truncated != "" {
		t.Fatalf("untruncated chain: truncated = %q", got.Truncated)
	}
}
