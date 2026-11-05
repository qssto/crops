package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// P2 L6 长对话压缩接入主链路（P2 §6.7 技术实现 1、5、6、7）：
//   - 上下文组装在本 run 第一次调用模型时做一次（classify / choose / explain 共用同一会话块），
//     会话块以 "session" 键追加在任务入参之后；首轮（无上一轮）不追加；
//   - enable_compaction 关、未配置 Compaction、下发前复检时不进 compaction 包任何分支，入参与 P1 逐字节相同；
//   - 本 run 用到的压缩状态放进 ctx 载体（agent.CompactionCarrier），中断事务与终态 UPDATE 据此写 agent_run，
//     并随检查点快照落盘，resume 时从快照恢复到载体。

// sessionKey 是会话块在任务入参里的键。
const sessionKey = "session"

// errSessionInput 表示任务入参无法追加会话块（不是 JSON 对象或已有 session 键）。
var errSessionInput = errors.New("graph: task input cannot carry session")

// compactionRun 是一次 run 的上下文组装结果缓存（只组装一次）。
type compactionRun struct {
	once  sync.Once
	block json.RawMessage // nil：不追加会话块
}

type compactionRunKey struct{}

// withCompaction 在 runner.Run / Resume 之前给 ctx 挂压缩载体与组装缓存；未配置 Compaction 时原样返回。
func (a *Agent) withCompaction(ctx context.Context) context.Context {
	if a.cfg.Compaction == nil {
		return ctx
	}
	ctx, _ = agent.WithCompactionCarrier(ctx)
	return context.WithValue(ctx, compactionRunKey{}, &compactionRun{})
}

// llmInput 返回本次模型调用的入参：有会话块时为「原入参 + session」，否则原样返回 in。
func (a *Agent) llmInput(ctx context.Context, s *state, task llm.Task, in any) any {
	cr, _ := ctx.Value(compactionRunKey{}).(*compactionRun)
	if cr == nil || rechecking(ctx) {
		return in
	}
	cr.once.Do(func() { cr.block = a.assembleSession(ctx, s) })
	if cr.block == nil {
		return in
	}
	body, err := withSession(in, cr.block)
	if err != nil {
		a.log.WarnContext(ctx, "compaction: session block skipped", "run_id", s.runID, "task", task, "err", err)
		return in
	}
	return body
}

// assembleSession 读开关并组装会话块；开关关 / 读失败（失败关闭）返回 nil，不调用 compaction 包。
func (a *Agent) assembleSession(ctx context.Context, s *state) json.RawMessage {
	on, err := agent.FlagOn(ctx, a.cfg.Flags, model.FlagEnableCompaction, s.q.Scope)
	if err != nil && !agent.IsNoFlagReader(err) {
		a.log.WarnContext(ctx, "compaction: flag read failed, treated as off", "run_id", s.runID, "err", err)
	}
	if !on {
		return nil
	}
	res := a.cfg.Compaction.Assemble(ctx, compaction.Request{Scope: s.q.Scope, RunID: s.runID})
	for _, f := range []struct{ kind, reason string }{
		{compaction.KindFallback, res.Failed}, {compaction.KindTrimmed, res.Trimmed}, {compaction.KindTruncated, res.Truncated},
	} {
		if f.reason == "" {
			continue
		}
		// 原因可能带源表取值（如断言字段、驱动错误里的参数），落 run_event 前同样脱敏。
		reason := pii.Mask(f.reason)
		a.log.WarnContext(ctx, "compaction degraded", "run_id", s.runID, "kind", f.kind, "reason", reason)
		a.emitAs(ctx, s, model.RunEvent{Type: model.EventCompactionFailed, Actor: model.ActorAgent},
			map[string]any{"kind": f.kind, "reason": reason})
	}
	count := 0
	if res.Applied != nil {
		count = 1
		a.emitAs(ctx, s, model.RunEvent{Type: model.EventCompactionApplied, Actor: model.ActorAgent}, res.Applied)
	}
	if c := agent.CompactionCarrierFrom(ctx); c != nil {
		c.Set(res.State, count)
	}
	return res.Block
}

// withSession 把会话块追加为入参对象的最后一个键（入参其余字节不变）。
func withSession(in any, block json.RawMessage) (json.RawMessage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSessionInput, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil || keys == nil {
		return nil, fmt.Errorf("%w: not a JSON object: %v", errSessionInput, err)
	}
	if _, dup := keys[sessionKey]; dup {
		return nil, fmt.Errorf("%w: input already has %q", errSessionInput, sessionKey)
	}
	body = body[:len(body)-1] // json.Marshal 的对象以 '}' 结尾、无尾随空白
	sep := ","
	if len(keys) == 0 {
		sep = ""
	}
	out := make([]byte, 0, len(body)+len(block)+len(sessionKey)+5)
	out = append(out, body...)
	out = append(out, sep+`"`+sessionKey+`":`...)
	out = append(out, block...)
	return append(out, '}'), nil
}

// addTokens 累计本次调用的 token（与 LLM_CALL_END 同一组值，只做成本统计）；stub 下为 0 不写，失败只记日志。
func (a *Agent) addTokens(ctx context.Context, s *state, u llm.Usage) {
	if a.cfg.Usage == nil || (u.PromptTokens == 0 && u.CompletionTokens == 0) {
		return
	}
	if err := a.cfg.Usage.AddTokens(ctx, s.q.Scope, s.runID, u.PromptTokens, u.CompletionTokens); err != nil {
		a.log.WarnContext(ctx, "add run tokens failed", "run_id", s.runID, "err", err)
	}
}

// compactionSnapshot 把载体里的压缩状态写进检查点快照（无状态时快照字节与 P1 相同）。
func compactionSnapshot(ctx context.Context, snap resumeSnapshot) resumeSnapshot {
	if st, n, ok := agent.CompactionCarrierFrom(ctx).Get(); ok {
		snap.CompactionState, snap.CompactionCount = st, n
	}
	return snap
}

// restoreCompaction 以检查点快照里的压缩状态为准恢复载体；compaction_version 不识别视为无压缩。
func restoreCompaction(ctx context.Context, snap resumeSnapshot) {
	c := agent.CompactionCarrierFrom(ctx)
	if c == nil || snap.CompactionState == nil || snap.CompactionState.CompactionVersion != model.CompactionVersion {
		return
	}
	c.Set(snap.CompactionState, snap.CompactionCount)
}
