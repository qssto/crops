package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

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

// refusedConclusion 是拒答卡结论（不含数字）。
var refusedConclusion = map[string]string{
	RefuseOutOfScope:     "该问题超出本农场水稻业务范围，无法给出建议",
	RefuseDataMissing:    "所需田块数据缺失或已过期，暂无法给出建议，请补充数据后再问",
	RefuseKBMiss:         "知识库未覆盖该问题，不凭经验作答，请咨询农技专家",
	RefuseLLMUnavailable: "模型暂不可用且规则层无明确结论，请稍后再问",
	RefuseValidation:     "建议未通过校验，已拦截，请农技专家人工处理",
}

// assemble：把规则层结论、模型结论与数值组装为建议卡（含逐字段引用）；已拒答则组装拒答卡。
func (a *Agent) assemble(_ context.Context, s *state) (*state, error) {
	if s.stopped() {
		s.card = a.refusedCard(s)
		return s, nil
	}
	c := a.baseCard(s)
	c.Outcome, c.AdviceType, c.Degraded = model.OutcomeAdvice, s.dec.AdviceType, s.degraded
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
	c.Conclusion = refusedConclusion[s.refuse]
	if c.Conclusion == "" {
		c.Conclusion = "暂无法给出建议"
	}
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
		f, ok := s.facts[k]
		if !ok {
			continue
		}
		o := s.tools[f.Tool]
		if o == nil || o.Result.ToolCallID == 0 {
			continue
		}
		v, err := json.Marshal(f.Value)
		if err != nil {
			a.log.Warn("measured value not encodable", "run_id", s.runID, "fact", k, "err", err)
			continue
		}
		at := s.now.UTC()
		if t, ok := parseTime(o.Data[keyCollectedAt], a.loc); ok {
			at = t.UTC()
		}
		src := o.Result.Source
		if src == "" {
			src = o.Tool
		}
		id := o.Result.ToolCallID
		out = append(out, model.Citation{TenantID: s.q.Scope.TenantID, FarmID: s.q.Scope.FarmID,
			Type: model.CitationMeasured, ClaimPath: path, DataSource: src, CollectedAt: &at, Value: v, ToolCallID: &id})
	}
	return out
}

// placeholderRe 匹配 {{nK}} 占位符。
var placeholderRe = regexp.MustCompile(`\{\{n(\d+)\}\}`)

// conclusionMaxRunes 是 advice_card.conclusion 的长度上限（契约 maxLength）。
const conclusionMaxRunes = 512

// validate：拦截模型裸写数字、越界占位符、集合外药剂、无 calc trace 的数值、不存在的知识引用、
// 无 tool_call_id 的实测引用；不通过即改为 VALIDATION_FAILED 拒答卡。
func (a *Agent) validate(_ context.Context, s *state) (*state, error) {
	if s.stopped() {
		return s, nil
	}
	if problem := a.check(s); problem != "" {
		a.log.Warn("advice rejected by validate", "run_id", s.runID, "problem", problem)
		s.refuseWith(RefuseValidation, problem)
		s.card = a.refusedCard(s)
	}
	return s, nil
}

// check 返回第一处不合规（空串表示通过）。
func (a *Agent) check(s *state) string {
	texts := append([]string{s.conclusion}, s.modelWhy...)
	for _, t := range texts {
		for _, m := range placeholderRe.FindAllStringSubmatch(t, -1) {
			if i, err := strconv.Atoi(m[1]); err != nil || i >= len(s.numbers) {
				return "placeholder " + m[0] + " has no number"
			}
		}
		if bare := digitRun.FindString(placeholderRe.ReplaceAllString(t, "")); bare != "" {
			return "model text has bare number " + bare
		}
	}
	c := s.card
	switch {
	case utf8.RuneCountInString(c.Conclusion) > conclusionMaxRunes:
		return "conclusion too long"
	case c.SelectedProduct != "" && !slices.Contains(c.ProductCandidates, c.SelectedProduct):
		return "selected product " + c.SelectedProduct + " not in candidates"
	case s.category() == model.CategoryProtect && s.operation() && !s.degraded && c.SelectedProduct == "":
		return "spray operation without selected product"
	case len(c.Citations) == 0:
		return "advice has no citation"
	}
	for _, n := range s.numbers {
		if n.Result.Trace.Formula == "" || !finite(n.Result.Value) {
			return "number " + n.Field + " has no calc trace"
		}
	}
	for i, ct := range c.Citations {
		switch ct.Type {
		case model.CitationKB:
			if ct.KBVersion == "" || !s.env.snap.Has(ct.KBID, ct.KBVersion) || s.env.snap.Deprecated(ct.KBID) {
				return fmt.Sprintf("citations[%d] %s not in knowledge %s", i, ct.KBID, s.env.snap.Ref())
			}
		case model.CitationMeasured:
			if ct.ToolCallID == nil || *ct.ToolCallID == 0 {
				return fmt.Sprintf("citations[%d] measured without tool_call_id", i)
			}
		}
	}
	return ""
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

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
	o := model.WorkOrder{AdviceID: c.AdviceID, RunID: s.runID, TenantID: c.TenantID, FarmID: c.FarmID,
		FieldID: c.FieldID, WorkType: s.dec.WorkType, DeviceType: deviceOf(s.dec), Payload: payload,
		TimeWindow: *c.SuggestedWindow, ConstraintChecks: []model.ConstraintCheck{},
		Status: model.OrderDraft, DispatchMode: model.DispatchDryRun}
	id, err := a.cfg.Drafts.CreateDraft(ctx, s.q.Scope, o)
	if err != nil {
		return s, fmt.Errorf("to_order: create draft: %w", err)
	}
	o.OrderID = id
	s.order = &o
	return s, nil
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
