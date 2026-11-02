package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail"
	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/model"
)

// 卡片置信度：规则层完整判定、降级、拒答。
const (
	confidenceFull     = 0.8
	confidenceDegraded = 0.5
)

// claim_path 取值（与 citation_valid 打分器一致）。
const claimConclusion = "conclusion"

// refusedConclusion 是非故障类拒答的卡片结论（不含数字）；DATA_MISSING / KB_MISS / LLM_UNAVAILABLE
// 是故障矩阵的 refuse_reason，文案取 configs/texts/zh-CN.yaml（refusedText）。
var refusedConclusion = map[string]string{
	RefuseOutOfScope: "该问题超出本农场水稻业务范围，无法给出建议",
	RefuseValidation: "建议未通过校验，已拦截，请农技专家人工处理",
}

// refusedFallback 是未登记拒答原因的结论。
const refusedFallback = "暂无法给出建议"

// assemble：把规则层结论、模型结论与数值组装为建议卡（含逐字段引用）；已拒答则组装拒答卡。
func (a *Agent) assemble(ctx context.Context, s *state) (*state, error) {
	if s.stopped() {
		s.card = a.refusedCard(s)
		if err := a.emitRefusalFault(ctx, s); err != nil {
			return s, fmt.Errorf("assemble: %w", err)
		}
		return s, nil
	}
	c := a.baseCard(s)
	c.Outcome, c.AdviceType, c.Degraded = model.OutcomeAdvice, s.dec.AdviceType, s.degraded || s.staleData
	c.Conclusion = fill(s.conclusion, s.numbers)
	c.ProductCandidates = append([]string(nil), s.candidates...)
	if s.selected != "" {
		c.SelectedProduct = s.selected
	}
	c.Confidence, c.NextAction = confidenceFull, model.NextObserve
	if s.degraded {
		c.Confidence = confidenceDegraded
	}
	if s.operation() {
		c.NextAction = model.NextCreateOrder
		c.SuggestedWindow = &model.TimeWindow{From: s.now.UTC(), To: s.now.Add(a.cfg.OrderWindow).UTC()}
	}
	c.Citations = append(c.Citations, a.kbCites(s, claimConclusion, s.dec.Cites)...)
	for i, r := range s.dec.Reasons {
		path := fmt.Sprintf("reasons[%d]", i)
		c.Reasons = append(c.Reasons, r.Text)
		c.Citations = append(c.Citations, a.kbCites(s, path, r.KB)...)
		c.Citations = append(c.Citations, a.measuredCites(s, path, r.Facts)...)
	}
	for i, n := range s.numbers {
		c.Numbers = append(c.Numbers, model.Number{Field: n.Field, Value: n.Result.Rounded(), Unit: n.Result.Unit})
		c.Citations = append(c.Citations, a.kbCites(s, fmt.Sprintf("numbers[%d]", i), []string{n.KB})...)
	}
	s.card = c
	return s, nil
}

// baseCard 是建议卡与拒答卡共有的字段；分类不在四个一级类时 category 记 protect（契约枚举要求）。
func (a *Agent) baseCard(s *state) *model.AdviceCard {
	cat := s.category()
	if _, ok := rules[cat]; !ok {
		cat = model.CategoryProtect
	}
	return &model.AdviceCard{RunID: s.runID, TenantID: s.q.Scope.TenantID, FarmID: s.q.Scope.FarmID,
		FieldID: s.q.FieldID, Crop: a.cfg.Crop, PhenologyStage: s.stage, Category: cat,
		Reasons: []string{}, Citations: []model.Citation{}}
}

// refusedCard 是拒答卡：不带建议类型、数值、药剂与引用。
func (a *Agent) refusedCard(s *state) *model.AdviceCard {
	c := a.baseCard(s)
	c.Outcome, c.RefuseReason, c.NextAction = model.OutcomeRefused, s.refuse, model.NextNone
	c.Conclusion = a.refusedText(s.refuse)
	return c
}

// fill 把 {{nK}} 占位符替换为舍入后的数值与单位；越界占位符原样保留（由 validate 拒绝）。
func fill(text string, ns []number) string {
	for i := len(ns) - 1; i >= 0; i-- { // 倒序，避免 {{n1}} 先替换掉 {{n10}} 的前缀
		r := ns[i].Result
		text = strings.ReplaceAll(text, placeholder(i), strings.TrimSpace(fmtNum(r.Rounded())+" "+r.Unit))
	}
	return text
}

// kbCites 为 claim_path 生成知识库引用（kb_version 取当前快照）。
func (a *Agent) kbCites(s *state, path string, ids []string) []model.Citation {
	out := make([]model.Citation, 0, len(ids))
	for _, id := range ids {
		c := model.Citation{TenantID: s.q.Scope.TenantID, FarmID: s.q.Scope.FarmID, Type: model.CitationKB,
			ClaimPath: path, KBID: id}
		if en, ok := s.env.snap.Entry(id); ok {
			c.KBVersion = en.Version
		}
		out = append(out, c)
	}
	return out
}

// measuredCites 为 claim_path 生成实测引用；来源工具没有 tool_call_id 时不引用（契约要求）。
func (a *Agent) measuredCites(s *state, path string, keys []string) []model.Citation {
	var out []model.Citation
	for _, k := range keys {
		m, ok := a.measured(s, k)
		if !ok {
			continue
		}
		out = append(out, model.Citation{TenantID: s.q.Scope.TenantID, FarmID: s.q.Scope.FarmID,
			Type: model.CitationMeasured, ClaimPath: path, DataSource: m.Source, CollectedAt: &m.CollectedAt,
			Value: m.Value, ToolCallID: &m.ToolCallID})
	}
	return out
}

// measuredFact 是一条可引用的实测事实（带 tool_call_id 的工具结果中的一个字段）。
type measuredFact struct {
	Value       json.RawMessage
	Source      string
	CollectedAt time.Time
	ToolCallID  uint64
}

// measured 取事实 k 的实测来源；事实不存在、来源工具无 tool_call_id 或值不可编码时返回 false。
func (a *Agent) measured(s *state, k string) (measuredFact, bool) {
	f, ok := s.facts[k]
	if !ok {
		return measuredFact{}, false
	}
	o := s.tools[f.Tool]
	if o == nil || o.Result.ToolCallID == 0 {
		return measuredFact{}, false
	}
	v, err := json.Marshal(f.Value)
	if err != nil {
		a.log.Warn("measured value not encodable", "run_id", s.runID, "fact", k, "err", err)
		return measuredFact{}, false
	}
	at := s.now.UTC()
	if t, ok := parseTime(o.Data[keyCollectedAt], a.loc); ok {
		at = t.UTC()
	}
	src := o.Result.Source
	if src == "" {
		src = o.Tool
	}
	return measuredFact{Value: v, Source: src, CollectedAt: at, ToolCallID: o.Result.ToolCallID}, true
}

// outputValidator 是输出护栏（契约 schema 编译一次，构造后只读）。
var outputValidator = sync.OnceValues(guardrail.New)

// validationDetailMax 是 VALIDATION_FAILED 事件 detail 的字节上限（run_event.payload ≤ 8KB）。
const validationDetailMax = 1024

// validate：经 guardrail.OutputValidator 按 schema → 引用 → 数值 → 药剂 校验建议卡（拒答卡只校验 schema）；
// 不通过即改为 VALIDATION_FAILED 拒答卡并发 VALIDATION_FAILED 事件，不修不补、不重试。
func (a *Agent) validate(ctx context.Context, s *state) (*state, error) {
	if s.stopped() {
		return s, nil
	}
	v, err := outputValidator()
	if err != nil {
		return s, fmt.Errorf("validate: %w", err)
	}
	p := v.Validate(a.guardInput(s))
	if p == nil {
		return s, nil
	}
	a.log.Warn("advice rejected by validate", "run_id", s.runID, "check", p.Check, "problem", p.Detail)
	s.refuseWith(RefuseValidation, p.Error())
	s.card = a.refusedCard(s)
	if err := a.emitValidationFailed(ctx, s, p); err != nil {
		return s, fmt.Errorf("validate: %w", err)
	}
	return s, nil
}

// guardInput 组装护栏输入：药剂候选取规则层 s.candidates，模型原文为结论模板与模型理由。
func (a *Agent) guardInput(s *state) guardrail.Input {
	nums := make([]guardrail.Number, len(s.numbers))
	for i, n := range s.numbers {
		nums[i] = guardrail.Number{Field: n.Field, Result: n.Result}
	}
	in := guardrail.Input{
		Card:           s.card,
		Numbers:        nums,
		Candidates:     s.candidates,
		ModelText:      append([]string{s.conclusion}, s.modelWhy...),
		RequireProduct: s.category() == model.CategoryProtect && s.operation() && !s.degraded,
	}
	if s.env.snap != nil { // 避免 typed-nil 接口：未载入知识时由护栏按「无快照」拒绝
		in.KB = s.env.snap
	}
	return in
}

// emitValidationFailed 发 VALIDATION_FAILED 事件（payload：run_id、失败校验项、脱敏截断后的 detail）；
// Events 为空时只记日志。发送失败返回错误（run 失败），不吞。
func (a *Agent) emitValidationFailed(ctx context.Context, s *state, p *guardrail.Violation) error {
	if a.cfg.Events == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"run_id": s.runID, "check": p.Check,
		"detail": truncateBytes(pii.Mask(p.Detail), validationDetailMax)})
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", model.EventValidationFailed, err)
	}
	_, err = a.cfg.Events.Emit(ctx, model.RunEvent{TenantID: s.q.Scope.TenantID, FarmID: s.q.Scope.FarmID,
		RunID: s.runID, StepID: NodeValidate, Type: model.EventValidationFailed, Actor: model.ActorAgent,
		Payload: payload, CreatedAt: a.cfg.Clock.Now()})
	if err != nil {
		return fmt.Errorf("emit %s: %w", model.EventValidationFailed, err)
	}
	return nil
}

// truncateBytes 截断到最多 n 字节（不切断 UTF-8 字符）。
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// persist：落建议卡（拒答卡同样落库）；执行权丢失后不写。SaveAdvice 按 run 幂等。
func (a *Agent) persist(ctx context.Context, s *state) (*state, error) {
	if err := runner.Alive(ctx); err != nil {
		return s, fmt.Errorf("persist: %w", err)
	}
	id, err := a.cfg.Advice.SaveAdvice(ctx, s.q.Scope, *s.card)
	if err != nil {
		return s, fmt.Errorf("persist: save advice: %w", err)
	}
	s.card.AdviceID = id
	a.emitAdviceReady(ctx, s)
	return s, nil
}

// 工单设备类型（work_order.device_type，≤16 字符）。
const (
	deviceManual = "manual"
	deviceValve  = "valve"
)

// toOrder：作业类建议转作业单草稿（DRAFT + DRY_RUN）；观察类、降级与拒答不转。CreateDraft 按建议卡幂等。
func (a *Agent) toOrder(ctx context.Context, s *state) (*state, error) {
	c := s.card
	if c.Outcome != model.OutcomeAdvice || c.NextAction != model.NextCreateOrder {
		return s, nil
	}
	if err := runner.Alive(ctx); err != nil {
		return s, fmt.Errorf("to_order: %w", err)
	}
	payload, err := json.Marshal(a.payload(s))
	if err != nil {
		return s, fmt.Errorf("to_order: encode payload: %w", err)
	}
	device := deviceOf(s.dec)
	o := model.WorkOrder{AdviceID: c.AdviceID, RunID: s.runID, TenantID: c.TenantID, FarmID: c.FarmID,
		FieldID: c.FieldID, WorkType: s.dec.WorkType, DeviceType: device, Payload: payload,
		TimeWindow: *c.SuggestedWindow, Status: model.OrderDraft, DispatchMode: a.dispatchMode(ctx, s.q.Scope, device)}
	o.ConstraintChecks = a.constraintChecks(ctx, s, model.PhasePreApproval, o, s.selectedProduct())
	id, err := a.cfg.Drafts.CreateDraft(ctx, s.q.Scope, o)
	if err != nil {
		return s, fmt.Errorf("to_order: create draft: %w", err)
	}
	o.OrderID = id
	s.order = &o
	err = a.openTicket(ctx, s)
	var blocked *agent.ConstraintBlockedError
	switch {
	case errors.As(err, &blocked):
		// P1 L3：开票前约束拦截是业务结论而非运行错误：单留 DRAFT、无票据，run 正常收尾；
		// CONSTRAINT_BLOCKED 已由闸门在拦截事务内写入（与校验行同事务）
		s.blocked = slices.Clone(blocked.RuleIDs)
		a.log.InfoContext(ctx, "work order blocked by constraints", "run_id", s.runID, "order_id", id, "rule_ids", s.blocked)
	case err != nil:
		return s, fmt.Errorf("to_order: %w", err)
	}
	return s, nil
}

// selectedProduct 是工单所用登记药剂 id（施药且已在候选内选药）；非药剂作业返回空。
func (s *state) selectedProduct() string {
	if s.dec.WorkType == model.WorkSpray && s.selected != "" && slices.Contains(s.candidates, s.selected) {
		return s.selected
	}
	return ""
}

// dispatchMode 在 to_order 读一次开关写定 dispatch_mode（P1-L2：按设备类型，开关开且类型可开放才 REAL）；
// 未装配或读取失败按 DRY_RUN（失败关闭）。
func (a *Agent) dispatchMode(ctx context.Context, scope model.Scope, deviceType string) model.DispatchMode {
	if a.cfg.DispatchMode == nil {
		return model.DispatchDryRun
	}
	m, err := a.cfg.DispatchMode.DispatchModeFor(ctx, scope, deviceType)
	if err != nil {
		a.log.ErrorContext(ctx, "to_order: read dispatch mode, fall back to DRY_RUN", "farm_id", scope.FarmID,
			"device_type", deviceType, "err", err)
		return model.DispatchDryRun
	}
	return m
}

// deviceOf：施药与叶面喷施按施药方式，灌溉为阀门，其余人工。
func deviceOf(d decision) string {
	switch {
	case d.WorkType == model.WorkSpray || d.Product == nameKH2PO4:
		return string(d.Sprayer)
	case d.WorkType == model.WorkIrrigate:
		return deviceValve
	}
	return deviceManual
}

// quantity 是工单 payload 里的一个数量。
type quantity struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// orderPayload 是作业单 payload：物料、亩用量、整田用量、地块、面积。
type orderPayload struct {
	Product   string    `json:"product,omitempty"`
	DosePerMu *quantity `json:"dose_per_mu,omitempty"`
	Total     *quantity `json:"total,omitempty"`
	FieldID   uint64    `json:"field_id"`
	AreaMu    *float64  `json:"area_mu,omitempty"`
}

// payload 取第一个亩用量（不含兑水量）与第一个整田用量 / 灌水量。
func (a *Agent) payload(s *state) orderPayload {
	p := orderPayload{Product: s.productName(), FieldID: s.q.FieldID}
	for _, n := range s.card.Numbers {
		q := &quantity{Value: n.Value, Unit: n.Unit}
		switch {
		case p.DosePerMu == nil && strings.Contains(n.Field, "per_mu") && !strings.HasPrefix(n.Field, "water"):
			p.DosePerMu = q
		case p.Total == nil && (strings.Contains(n.Field, "total") || n.Field == "water_m3"):
			p.Total = q
		}
	}
	if area, ok := s.facts.num(factAreaMu); ok {
		p.AreaMu = &area
	}
	return p
}
