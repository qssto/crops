package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// Source 读一次上下文组装的会话原料（repository.RunRepo 实现）。断言前会再调一次，取值从源表重新读取。
type Source interface {
	LoadSession(ctx context.Context, scope model.Scope, runID uint64) (model.SessionSource, error)
}

// Config 是压缩参数（取自 config.Compaction，app 装配时转换）。
//   - MaxBlockTokens 是整个会话块（含 facts）的估算 token 硬上限，须大于 TriggerTokens；超出时按
//     「最早的原文轮次 → 摘要」顺序裁剪非白名单内容，facts 不裁（见 capBlock）；
//   - SummaryTimeout 是单次摘要调用的超时，超时按摘要失败放弃压缩。
type Config struct {
	TriggerTokens    int
	KeepRecentTurns  int
	SummaryMaxTokens int
	MaxBlockTokens   int
	SummaryTimeout   time.Duration
}

// Deps 是 Engine 的依赖；Summarizer 为空取 Deterministic。
// Mask 是落库脱敏（生产注入 pii.Mask；本包不 import guardrail）：每次读出会话原料后先对自由文本脱敏，
// 会话块、compaction_state 与检查点因此都只含脱敏后的取值（rejection_reason.reason_text 落库时未脱敏）。
type Deps struct {
	Config     Config
	Lexicon    *Lexicon
	Source     Source
	Raw        agent.RawStore
	Summarizer Summarizer
	Clock      clock.Clock
	Mask       func(string) string
	Log        *slog.Logger // 为空取 slog.Default()
}

// Engine 是上下文组装的压缩入口（无状态，可并发使用）。
type Engine struct{ d Deps }

// NewEngine 校验依赖并构造 Engine。
func NewEngine(d Deps) (*Engine, error) {
	var missing []string
	for name, isNil := range map[string]bool{"Lexicon": d.Lexicon == nil, "Source": d.Source == nil,
		"Raw": d.Raw == nil, "Clock": d.Clock == nil, "Mask": d.Mask == nil} {
		if isNil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("compaction: missing deps %v", missing)
	}
	c := d.Config
	if c.TriggerTokens <= 0 || c.KeepRecentTurns <= 0 || c.SummaryMaxTokens <= 0 || c.SummaryMaxTokens >= c.TriggerTokens ||
		c.MaxBlockTokens <= c.TriggerTokens || c.SummaryTimeout <= 0 {
		return nil, fmt.Errorf("compaction: bad config %+v", c)
	}
	if d.Summarizer == nil {
		d.Summarizer = Deterministic{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Engine{d: d}, nil
}

// Request 是一次上下文组装。
type Request struct {
	Scope model.Scope
	RunID uint64
}

// Applied 是一次新压缩的前后估算 token 数（COMPACTION_APPLIED 事件）。
type Applied struct {
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
}

// Result 是组装结果：
//   - Block 为附加到模型输入的会话块字节；nil 表示无会话（首轮）或源表读取失败，模型输入与开关关闭时逐字节相同；
//   - State 非空表示本 run 使用了摘要，须随检查点 / 终态写入 agent_run.compaction_state；
//   - Applied 非空表示本次新做了一次压缩；Failed 非空表示放弃压缩（原因），已退回完整上下文；
//   - Trimmed 非空表示会话块超过 MaxBlockTokens，裁掉了最早的原文轮次 / 摘要（facts 不裁；State / Applied 仍可能非空）；
//   - Truncated 非空表示会话链超过回溯上限被截断，更早 run 的约束 / 否决已按地块汇总进 facts。
//
// Failed / Trimmed / Truncated 都经 COMPACTION_FAILED 发出（payload kind 区分），不让 run 失败。
// COMPACTION_FAILED payload 的 kind：区分放弃压缩、超限裁剪与会话链截断（共用一个事件类型，不新增事件）。
const (
	KindFallback  = "FALLBACK"          // 放弃压缩，已退回完整上下文
	KindTrimmed   = "TRIMMED"           // 会话块超过 max_block_tokens，裁掉了原文轮次 / 摘要
	KindTruncated = "SESSION_TRUNCATED" // 会话链超过回溯上限，更早 run 的约束 / 否决已按地块汇总
)

type Result struct {
	Block     []byte
	State     *model.CompactionState
	Applied   *Applied
	Failed    string
	Trimmed   string
	Truncated string
}

// Assemble 组装会话块（P2 §6.7 技术实现 1、3-5、7）。失败关闭：摘要 / 断言 / 原文转存任一失败都放弃压缩，
// 发 Failed 并用完整上下文（白名单 facts + 全部原文轮次）；不返回 error，不让 run 失败。
func (e *Engine) Assemble(ctx context.Context, req Request) Result {
	src, err := e.load(ctx, req)
	if err != nil {
		return Result{Failed: err.Error()}
	}
	prev := e.decodeState(ctx, req, src.State)
	if len(src.Turns) == 0 && prev == nil {
		return Result{}
	}
	res := e.assemble(ctx, req, src, prev)
	if src.Truncated {
		res.Truncated = fmt.Sprintf("session chain truncated: earlier %d runs of this field / user aggregated for constraints and vetoes",
			src.EarlierRuns)
	}
	return res
}

// assemble 是 Assemble 读出会话原料之后的部分。
func (e *Engine) assemble(ctx context.Context, req Request, src model.SessionSource, prev *model.CompactionState) Result {
	full := fullOf(src, e.d.Lexicon)
	facts, turns := full.Facts, full.Turns
	kept, ok := keptTurns(turns, prev)
	if !ok {
		prev = nil // 摘要边界不在本会话轮次内（状态与轮次不符）：按无压缩
		kept = turns
	}
	block := Block{CompactionVersion: model.CompactionVersion, Facts: facts, Turns: kept}
	if prev != nil {
		block.Summary = prev.Summary
	}
	before, err := encode(block)
	if err != nil {
		return e.fallback(full, "encode: "+err.Error())
	}
	p := plan{full: full, block: block, state: prev}
	if freeTokens(block) > e.d.Config.TriggerTokens && len(kept) > e.d.Config.KeepRecentTurns {
		cut := len(kept) - e.d.Config.KeepRecentTurns
		summary, err := e.summarize(ctx, block.Summary, kept[:cut])
		if err != nil {
			return e.fallback(full, err.Error())
		}
		p.block.Summary, p.block.Turns = summary, kept[cut:]
		p.state = &model.CompactionState{SummarizedUntilStepID: kept[cut-1].StepID, Summary: summary}
		if prev != nil {
			p.state.RawRefs, p.state.History = slices.Clone(prev.RawRefs), slices.Clone(prev.History)
		}
		p.compressed, p.tokensBefore = kept[:cut], EstimateTokens(string(before))
	}
	return e.finish(ctx, req, p)
}

// freeTokens 是会话块中可压缩部分（摘要 + 原文轮次）的估算 token 数，作触发依据。
// 白名单 facts 不可压缩、不计入：否则 facts 本身超过阈值后每轮都会重复触发（技术实现 1「压缩后估算值回落」）。
// history / COMPACTION_APPLIED 的前后 token 数按整个会话块计。
func freeTokens(b Block) int {
	n := 0
	if b.Summary != nil {
		n += EstimateTokens(b.Summary.Text)
	}
	for _, t := range b.Turns {
		n += EstimateTokens(t.StepID) + EstimateTokens(t.Question) + EstimateTokens(t.Answer)
	}
	return n
}

// plan 是待断言的组装结果：block 为拟发出的会话块，full 为失败时退回的完整上下文；
// compressed 非空表示本次新压缩了这些轮次（断言通过后才转存原文、记 history）。
type plan struct {
	full, block  Block
	state        *model.CompactionState
	compressed   []Turn
	tokensBefore int
}

// finish 对拟发出的会话块做白名单断言（源表重新读取），通过后转存被压缩原文并补齐状态。
// 不带摘要（未压缩过）的会话块就是完整上下文，无需断言。
func (e *Engine) finish(ctx context.Context, req Request, p plan) Result {
	if p.block.Summary == nil {
		return e.plain(p.full)
	}
	// 先裁到硬上限再断言：发出的就是断言过的字节（裁剪只去最早的原文轮次 / 摘要，仍须满足后缀 + 摘要承接）。
	block, trimmed := capBlock(p.block, e.d.Config.MaxBlockTokens)
	p.block = block
	out, err := encode(p.block)
	if err != nil {
		return e.fallback(p.full, "encode: "+err.Error())
	}
	reread, err := e.load(ctx, req)
	if err != nil {
		// 重读失败拿不到更新的源表，只能退回首读的完整上下文（与开关关时同口径：首读即本次可得的最新事实）。
		return e.fallback(p.full, "reread: "+err.Error())
	}
	// 重读成功后一切回退都用重读的源表重建完整上下文：首读之后工单被取消 / 票被驳回时，
	// 首读的事实已过时，不能不经断言发给模型。
	fresh := fullOf(reread, e.d.Lexicon)
	if err := Assert(out, reread, e.d.Lexicon); err != nil {
		return e.fallback(fresh, err.Error())
	}
	st := *p.state
	st.CompactionVersion, st.Facts = model.CompactionVersion, p.block.Facts
	res := Result{Block: out, State: &st, Trimmed: trimmed}
	if len(p.compressed) > 0 {
		id, err := e.putRaw(ctx, req, p.compressed)
		if err != nil {
			return e.fallback(fresh, err.Error())
		}
		after := EstimateTokens(string(out))
		st.RawRefs = append(st.RawRefs, id)
		st.History = append(st.History, model.CompactionHistory{At: e.d.Clock.Now(), TokensBefore: p.tokensBefore,
			TokensAfter: after})
		res.Applied = &Applied{TokensBefore: p.tokensBefore, TokensAfter: after}
	}
	return res
}

// load 读会话原料并对自由文本脱敏（首读与断言前重读同一口径，断言两侧可比）。
func (e *Engine) load(ctx context.Context, req Request) (model.SessionSource, error) {
	src, err := e.d.Source.LoadSession(ctx, req.Scope, req.RunID)
	if err != nil {
		return model.SessionSource{}, fmt.Errorf("load session: %w", err)
	}
	return sanitize(src, e.d.Mask), nil
}

// sanitize 返回自由文本经 mask 的会话原料副本（问题、回答、驳回原因、建议卡结论；不改入参的底层数组）。
// 结构化取值（id、状态、剂量、实测摘要——后者落库前已经 MaskJSON）原样保留。
func sanitize(src model.SessionSource, mask func(string) string) model.SessionSource {
	src.Turns = slices.Clone(src.Turns)
	for i := range src.Turns {
		src.Turns[i].Question, src.Turns[i].Answer = mask(src.Turns[i].Question), mask(src.Turns[i].Answer)
	}
	src.Cards = slices.Clone(src.Cards)
	for i := range src.Cards {
		src.Cards[i].Conclusion = mask(src.Cards[i].Conclusion)
	}
	src.Orders = slices.Clone(src.Orders)
	for i := range src.Orders {
		src.Orders[i].RejectText = mask(src.Orders[i].RejectText)
	}
	src.EarlierTurns = slices.Clone(src.EarlierTurns)
	for i := range src.EarlierTurns {
		src.EarlierTurns[i].Question, src.EarlierTurns[i].Answer = mask(src.EarlierTurns[i].Question), mask(src.EarlierTurns[i].Answer)
	}
	src.EarlierOrders = slices.Clone(src.EarlierOrders)
	for i := range src.EarlierOrders {
		src.EarlierOrders[i].RejectText = mask(src.EarlierOrders[i].RejectText)
	}
	return src
}

// fullOf 由会话原料构造完整上下文（白名单 facts + 全部原文轮次，不带摘要）。
func fullOf(src model.SessionSource, lex *Lexicon) Block {
	return Block{CompactionVersion: model.CompactionVersion, Facts: BuildFacts(src, lex), Turns: turnsOf(src.Turns)}
}

// plain 发完整上下文、不带状态（未压缩过的会话）；超过硬上限时裁剪原文轮次（Failed 记裁剪）。
func (e *Engine) plain(full Block) Result {
	block, trimmed := capBlock(full, e.d.Config.MaxBlockTokens)
	out, err := encode(block)
	if err != nil {
		return Result{Failed: "encode full context: " + err.Error()}
	}
	return Result{Block: out, Trimmed: trimmed}
}

// fallback 放弃压缩、退回完整上下文（失败关闭：不丢白名单，不写新状态）。
func (e *Engine) fallback(full Block, reason string) Result {
	r := e.plain(full)
	if r.Failed == "" { // 编码失败时保留编码错误
		r.Failed = reason
	}
	return r
}

// capBlock 把会话块裁到估算 token ≤ max（P2 L6：facts 计入估算，回退的全量原文也不得超出模型窗口）。
// 裁剪顺序：先由早到晚丢原文轮次，轮次丢完仍超出再丢摘要；白名单 facts 永不裁剪，
// facts 本身超出时照发 facts 并在说明里注明。未裁剪返回 ""。
func capBlock(b Block, max int) (Block, string) {
	if blockTokens(b) <= max {
		return b, ""
	}
	before, dropped := blockTokens(b), 0
	for len(b.Turns) > 0 && blockTokens(b) > max {
		b.Turns = b.Turns[1:]
		dropped++
	}
	note := fmt.Sprintf("trimmed: block %d tokens > max_block_tokens %d, dropped %d oldest turns", before, max, dropped)
	if blockTokens(b) > max && b.Summary != nil {
		b.Summary = nil
		note += " and summary"
	}
	if n := blockTokens(b); n > max {
		note += fmt.Sprintf("; facts alone %d tokens (whitelist kept)", n)
	}
	return b, note
}

// blockTokens 是会话块编码后的估算 token 数（编码失败按超出处理，交由后续编码报错）。
func blockTokens(b Block) int {
	out, err := encode(b)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return EstimateTokens(string(out))
}

// summarize 调摘要器并校验上限。
func (e *Engine) summarize(ctx context.Context, prev *model.CompactionSummary, turns []Turn) (*model.CompactionSummary, error) {
	prevText := ""
	if prev != nil {
		prevText = prev.Text
	}
	sctx, cancel := context.WithTimeout(ctx, e.d.Config.SummaryTimeout)
	defer cancel()
	text, version, err := e.d.Summarizer.Summarize(sctx, prevText, turns, e.d.Config.SummaryMaxTokens)
	if err != nil {
		return nil, fmt.Errorf("summarize: %w", err)
	}
	if err := sctx.Err(); err != nil { // 摘要器未理会 ctx、超时后才返回：同样放弃
		return nil, fmt.Errorf("summarize: %w", err)
	}
	tokens := EstimateTokens(text)
	switch {
	case text == "":
		return nil, errors.New("summarize: empty summary")
	case version == "":
		return nil, errors.New("summarize: empty prompt_version")
	case tokens > e.d.Config.SummaryMaxTokens:
		return nil, fmt.Errorf("summarize: %d tokens > max %d", tokens, e.d.Config.SummaryMaxTokens)
	}
	return &model.CompactionSummary{Text: text, Tokens: tokens, PromptVersion: version}, nil
}

// RawRecord 是被压缩原文在 MinIO 中的内容（raw_object.kind=COMPACTED_MSGS），按 run_id 列出后可还原。
type RawRecord struct {
	RunID uint64 `json:"run_id"`
	Turns []Turn `json:"turns"`
}

// DecodeRaw 解码 RawRecord。
func DecodeRaw(body []byte) (RawRecord, error) {
	var r RawRecord
	if err := json.Unmarshal(body, &r); err != nil {
		return RawRecord{}, fmt.Errorf("compaction: decode raw record: %w", err)
	}
	return r, nil
}

// putRaw 转存被压缩的原文轮次；对象名由本 run 与压缩边界确定（重放同一压缩幂等）。
func (e *Engine) putRaw(ctx context.Context, req Request, turns []Turn) (uint64, error) {
	body, err := json.Marshal(RawRecord{RunID: req.RunID, Turns: turns})
	if err != nil {
		return 0, fmt.Errorf("encode raw record: %w", err)
	}
	obj, err := e.d.Raw.Put(ctx, model.RawPut{TenantID: req.Scope.TenantID, FarmID: req.Scope.FarmID, RunID: req.RunID,
		Kind: model.RawCompactedMsgs, Name: fmt.Sprintf("compacted-%d-until-%s", req.RunID, turns[len(turns)-1].StepID),
		At: e.d.Clock.Now(), Body: body, ContentType: "application/json"})
	if err != nil {
		return 0, fmt.Errorf("put compacted msgs: %w", err)
	}
	return obj.ID, nil
}

// decodeState 解码已有压缩状态；为空、解码失败或 compaction_version 不识别时按无压缩（技术实现 7），
// 解码失败记 Warn（状态损坏不该静默）。
func (e *Engine) decodeState(ctx context.Context, req Request, raw json.RawMessage) *model.CompactionState {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var st model.CompactionState
	if err := json.Unmarshal(raw, &st); err != nil {
		e.d.Log.WarnContext(ctx, "compaction: decode compaction_state failed, treated as uncompressed",
			"run_id", req.RunID, "err", err)
		return nil
	}
	if st.CompactionVersion != model.CompactionVersion {
		return nil
	}
	return &st
}

// keptTurns 返回摘要边界之后的轮次；无摘要返回全部。边界不在轮次中时 ok=false。
func keptTurns(turns []Turn, prev *model.CompactionState) ([]Turn, bool) {
	if prev == nil || prev.Summary == nil || prev.SummarizedUntilStepID == "" {
		return turns, prev == nil || prev.Summary == nil
	}
	for i, t := range turns {
		if t.StepID == prev.SummarizedUntilStepID {
			return turns[i+1:], true
		}
	}
	return turns, false
}
