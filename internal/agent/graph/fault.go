package graph

// P1 L6 故障降级（§6.7 技术实现第 2、3、5 条）：LLM 不可用的规则层降级、MCP 缓存回落的标记与 DEGRADED 事件。
// 行为与文案取 Config.Faults（policy/fault_matrix.yaml + configs/texts/zh-CN.yaml）；事件经 Config.Events 落库，
// 由 EventRepo.AppendTx 同事务投影审计（harness 只记不判）。

import (
	"context"
	"fmt"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// SourceCache 是 MCP 网关超时回落到缓存时 ToolResult.Source / tool_call.source 的取值（与上游来源名不冲突）。
const SourceCache = model.ToolSourceCache

// degradeBlocker 返回不能走规则层降级的原因，空串表示可以降级：
// 知识已命中、规则层有阈值结论与引用（含规则建议），植保作业类另要求候选药剂非空。
func (s *state) degradeBlocker() string {
	switch {
	case len(s.hits) == 0:
		return "no knowledge hit"
	case s.dec.Text == "" || s.dec.Evaluated == 0 || len(s.dec.Cites) == 0:
		return "no rule decision"
	case s.category() == model.CategoryProtect && s.operation() && len(s.candidates) == 0:
		return "no candidate product"
	}
	return ""
}

// degrade：模型重试后仍不可用（task 节点），满足规则层条件则降级为保守的观察类建议（不选药、不转工单，
// 规则层数值仍由 calc 算、引用照常；compute 已按所选药剂补算的安全间隔期 / 用量 / 施药液量及其引用一并剥离），
// 写 DEGRADED(RULE_FALLBACK)；否则拒答 LLM_UNAVAILABLE（DEGRADED 在 assemble 写）。DEGRADED 写失败返回错误（run 失败）。
func (a *Agent) degrade(ctx context.Context, s *state, node string, task llm.Task) error {
	if why := s.degradeBlocker(); why != "" {
		s.refuseWith(RefuseLLMUnavailable, string(task)+": model unavailable, "+why)
		return nil
	}
	if s.degraded {
		return nil
	}
	s.degraded = true
	s.dec.AdviceType = model.AdviceTypeObservation
	s.candidates, s.selected = nil, ""
	s.dropProductNumbers()
	f := a.cfg.Faults.MustFault(model.FaultLLMUnavailable)
	return a.emitDegraded(ctx, s, node, model.DegradedPayload{Fault: f.Code, Action: model.DegradeRuleFallback,
		TextKey: f.DegradedTextKey})
}

// degradedConclusion 是规则层降级模板（文案表 degraded_text_key，填入不含数字的决策句）。
func (a *Agent) degradedConclusion(s *state) string {
	return a.cfg.Faults.MustFault(model.FaultLLMUnavailable).Degraded(s.dec.Text)
}

// noteToolFault 记录 enrich 中归入故障矩阵的工具失败（TIMEOUT / UPSTREAM → MCP_TIMEOUT），供拒答时写 DEGRADED(REFUSE)。
func (a *Agent) noteToolFault(s *state, o *toolOut) {
	if s.toolFault != "" || o.Result.Status == statusOK {
		return
	}
	if _, ok := a.cfg.Faults.ForToolError(o.Result.ErrorCode); ok {
		s.toolFault = o.Tool
	}
}

// noteCacheFallback：网关在 max_staleness 内回落到缓存的结果（source=cache）照常使用，卡片 degraded=true，
// 写 DEGRADED(MCP_TIMEOUT, CACHE_FALLBACK, staleness_sec)；staleness 取网关按原 collected_at 算的新鲜度。
func (a *Agent) noteCacheFallback(ctx context.Context, s *state, o *toolOut) error {
	if o.Result.Status != statusOK || o.Result.Source != SourceCache {
		return nil
	}
	s.staleData = true
	stale := o.Result.DataFreshnessSec
	f := a.cfg.Faults.MustFault(model.FaultMCPTimeout)
	return a.emitDegraded(ctx, s, NodeEnrich, model.DegradedPayload{Fault: f.Code, Action: model.DegradeCacheFallback,
		StalenessSec: &stale, TextKey: f.TextKey, Tool: o.Tool})
}

// refusedText 是拒答卡结论：故障类拒答原因（矩阵 refuse_reason）取文案表，其余取 refusedConclusion。
func (a *Agent) refusedText(reason string) string {
	if f, ok := a.cfg.Faults.ForRefuseReason(reason); ok {
		return f.Text
	}
	if t := refusedConclusion[reason]; t != "" {
		return t
	}
	return refusedFallback
}

// emitRefusalFault 在拒答收尾时写 DEGRADED(REFUSE)：LLM_UNAVAILABLE 与 KB_MISS 拒答一律写；
// DATA_MISSING 只在工具失败归入 MCP_TIMEOUT（无缓存 / 缓存超限 / 不回落的调用）时写，空数据 / 非法返回不算故障降级。
func (a *Agent) emitRefusalFault(ctx context.Context, s *state) error {
	f, ok := a.cfg.Faults.ForRefuseReason(s.refuse)
	if !ok || (f.Code == model.FaultMCPTimeout && s.toolFault == "") {
		return nil
	}
	p := model.DegradedPayload{Fault: f.Code, Action: model.DegradeRefuse, TextKey: f.TextKey}
	if f.Code == model.FaultMCPTimeout {
		p.Tool = s.toolFault
	}
	return a.emitDegraded(ctx, s, NodeAssemble, p)
}

// emitDegraded 发 DEGRADED（actor=system，投影为审计 DEGRADED）。与观测类事件不同，写失败返回错误、由节点让 run 失败
// （审查修复 L6-8）：故障降级必须留痕，降级卡 / 故障拒答卡不能在缺 DEGRADED 审计的情况下落库；
// 与 VALIDATION_FAILED 写失败的处理一致（runner 记 RUN_FAILED，调用方收到错误）。
func (a *Agent) emitDegraded(ctx context.Context, s *state, node string, p model.DegradedPayload) error {
	a.log.WarnContext(ctx, "run degraded", "run_id", s.runID, "fault", p.Fault, "action", p.Action, "tool", p.Tool,
		"alert", a.cfg.Faults.MustFault(p.Fault).Alert.Name)
	if err := a.emitStrict(ctx, s, model.RunEvent{Type: model.EventDegraded, Actor: model.ActorSystem, StepID: node}, p); err != nil {
		return fmt.Errorf("%s: %s/%s: %w", node, p.Fault, p.Action, err)
	}
	return nil
}
