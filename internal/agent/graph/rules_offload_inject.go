package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/guardrail"
	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// ---- P2 L4 下沉项注入与影子比对：explain 前读开关（每轮一次）——on 注入规则结论，shadow 不注入、模型输出后比对并在
// 落卡同一事务写 rule_shadow_diff，off 只拒收。三态下 output_validator 的拒收都照常（rules_offload.go，不读开关）。
//
// choose 不注入：choose 只在规则层候选内选药，三个下沉项都不改候选集合（混用禁忌只约束原文里的混配建议，
// 窗口与优先级只约束原文里的时间与对象），注入 choose 不改变可选范围。

// offloadRun 是本轮下沉项的开关与影子比对结果（persist 落库）。
type offloadRun struct {
	modes map[model.RuleOffloadItem]model.FlagMode
	diffs []model.RuleShadowDiff
}

// offloadSpec 是一个下沉项的注入与影子记录方式。
type offloadSpec struct {
	ver string // rule_shadow_diff.rule_ver
	// constrain 把规则结论写进注入约束；本轮不适用返回 false。
	constrain func(r *guardrail.RuleOutputs, c *llm.RuleConstraints) bool
	// output 是 rule_shadow_diff.rule_output 的内容（只在该项适用时调用）。
	output func(r *guardrail.RuleOutputs) any
}

// offloadSpecs 是已接入注入的下沉项（未登记的项三态都只拒收）。
var offloadSpecs = map[model.RuleOffloadItem]offloadSpec{
	model.RuleItemOperationWindow: {ver: calc.WindowRuleVer, constrain: constrainWindow, output: windowOutput},
	model.RuleItemTankMix:         {ver: calc.TankMixRuleVer, constrain: constrainTankMix, output: tankMixOutput},
	model.RuleItemPestPriority:    {ver: calc.PriorityRuleVer, constrain: constrainPriority, output: priorityOutput},
}

// offloadModes 读三个下沉项开关；读失败按 off（未装配属预期不告警，其余记日志）。
func (a *Agent) offloadModes(ctx context.Context, s *state) map[model.RuleOffloadItem]model.FlagMode {
	modes := make(map[model.RuleOffloadItem]model.FlagMode, len(model.RuleOffloadFlags()))
	for _, k := range model.RuleOffloadFlags() {
		item, ok := model.RuleOffloadItemOf(k)
		if !ok {
			continue
		}
		m, err := agent.FlagModeOf(ctx, a.cfg.Flags, k, s.q.Scope)
		if err != nil && !agent.IsNoFlagReader(err) {
			a.log.WarnContext(ctx, "read rule offload flag, fall back to off", "run_id", s.runID, "flag", k, "err", err)
		}
		modes[item] = m
	}
	return modes
}

// mode 是下沉项本轮的开关档位（未读或未登记为 off）。
func (o *offloadRun) mode(item model.RuleOffloadItem) model.FlagMode {
	if o == nil {
		return model.FlagModeOff
	}
	if m, ok := o.modes[item]; ok {
		return m
	}
	return model.FlagModeOff
}

// offloadConstraints 是 on 项的注入约束；没有 on 且适用的项时为 nil（explain 入参不带 constraints 字段）。
func (a *Agent) offloadConstraints(s *state) *llm.RuleConstraints {
	r := a.ruleOutputs(s)
	if r == nil {
		return nil
	}
	c := &llm.RuleConstraints{}
	found := false
	for _, item := range model.RuleOffloadItems() {
		spec, ok := offloadSpecs[item]
		if ok && s.offload.mode(item) == model.FlagModeOn && spec.constrain(r, c) {
			found = true
		}
	}
	if !found {
		return nil
	}
	return c
}

// shadowModel 是 rule_shadow_diff.model_output：冲突分句（pii.Mask）与冲突说明；一致时为空对象。
type shadowModel struct {
	Snippet string `json:"snippet,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// shadowCompare：shadow 项在模型输出后按与拒收同一实现（guardrail.CheckRules）比对，记一行待 persist 同事务落库。
// 只在模型给出了结论时比对（降级的规则层模板不算模型输出）。
func (a *Agent) shadowCompare(s *state) error {
	if s.offload == nil {
		return nil
	}
	r := a.ruleOutputs(s)
	for _, res := range guardrail.CheckRules(r, append([]string{s.conclusion}, s.modelWhy...), s.history) {
		spec, ok := offloadSpecs[res.Item]
		if !ok || s.offload.mode(res.Item) != model.FlagModeShadow {
			continue
		}
		rule, err := json.Marshal(spec.output(r))
		if err != nil {
			return fmt.Errorf("encode %s rule output: %w", res.Item, err)
		}
		var m shadowModel
		d := model.RuleShadowDiff{TenantID: s.q.Scope.TenantID, FarmID: s.q.Scope.FarmID, RunID: s.runID,
			Item: res.Item, Agreed: res.Conflict == nil, RuleOutput: rule, RuleVer: spec.ver}
		if c := res.Conflict; c != nil {
			d.DiffKind, m = c.Kind, shadowModel{Snippet: pii.Mask(c.Snippet), Detail: c.Detail}
		}
		if d.ModelOutput, err = json.Marshal(m); err != nil {
			return fmt.Errorf("encode %s model output: %w", res.Item, err)
		}
		s.offload.diffs = append(s.offload.diffs, d)
	}
	return nil
}

// ShadowAdviceStore 是可在落卡同一事务写 rule_shadow_diff 的 AdviceStore（repository.AdviceRepo 实现）。
// Config.Advice 实现它时 persist 走本方法；diffs 里的 AdviceID 由实现按卡片结局填（出卡取 advice_id，拒答为 0）。
type ShadowAdviceStore interface {
	SaveAdviceWithShadow(ctx context.Context, scope model.Scope, card model.AdviceCard, diffs []model.RuleShadowDiff) (uint64, error)
}

// saveAdvice 落卡：没有影子比对行时与 P1 相同（SaveAdvice）；有影子行而 AdviceStore 不支持同事务写时只落卡并告警。
func (a *Agent) saveAdvice(ctx context.Context, s *state) (uint64, error) {
	if s.offload == nil || len(s.offload.diffs) == 0 {
		return a.cfg.Advice.SaveAdvice(ctx, s.q.Scope, *s.card)
	}
	st, ok := a.cfg.Advice.(ShadowAdviceStore)
	if !ok {
		a.log.WarnContext(ctx, "advice store cannot write rule_shadow_diff, diffs dropped", "run_id", s.runID,
			"diffs", len(s.offload.diffs))
		return a.cfg.Advice.SaveAdvice(ctx, s.q.Scope, *s.card)
	}
	now := a.cfg.Clock.Now()
	for i := range s.offload.diffs {
		s.offload.diffs[i].CreatedAt = now
	}
	return st.SaveAdviceWithShadow(ctx, s.q.Scope, *s.card, s.offload.diffs)
}

// emitShadowDiffs 落库后逐行发 RULE_SHADOW_DIFF（观测类，发送失败只告警）。
func (a *Agent) emitShadowDiffs(ctx context.Context, s *state) {
	if s.offload == nil {
		return
	}
	for _, d := range s.offload.diffs {
		a.emitAs(ctx, s, model.RunEvent{Type: model.EventRuleShadowDiff, Actor: model.ActorAgent, StepID: NodePersist},
			model.RuleShadowDiffPayload{RunID: s.runID, Item: d.Item, Agreed: d.Agreed, DiffKind: d.DiffKind, RuleVer: d.RuleVer})
	}
}

// ---- operation_window ----

func constrainWindow(r *guardrail.RuleOutputs, c *llm.RuleConstraints) bool {
	if r.Window == nil || !r.Window.Window.Evaluated {
		return false
	}
	c.OperationWindow = &llm.WindowConstraint{Open: r.Window.Window.Open, Days: r.Window.AdmittedDays()}
	return true
}

// windowRuleOutput 是作业窗口的 rule_output：窗口结论与可安排作业的日期词。
type windowRuleOutput struct {
	calc.Window
	Days []string `json:"days,omitempty"`
}

func windowOutput(r *guardrail.RuleOutputs) any {
	return windowRuleOutput{Window: r.Window.Window, Days: r.Window.AdmittedDays()}
}

// ---- tank_mix ----

// constrainTankMix：注入所选药剂与分组表中不得与之同桶的药剂、是否有单桶上限；既无所选药剂也无上限时不注入。
func constrainTankMix(r *guardrail.RuleOutputs, c *llm.RuleConstraints) bool {
	t := r.TankMix
	if t == nil || (t.Selected == "" && t.Max <= 0) {
		return false
	}
	c.TankMix = &llm.TankMixConstraint{Selected: t.Selected, Forbidden: calc.ForbiddenPartners(t.Selected), TankLimit: t.Max > 0}
	return true
}

// tankMixRuleOutput 是混用禁忌的 rule_output：所选药剂、禁忌搭档与单桶上限。
type tankMixRuleOutput struct {
	Selected  string   `json:"selected,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
	Max       int      `json:"max,omitempty"`
}

func tankMixOutput(r *guardrail.RuleOutputs) any {
	t := r.TankMix
	return tankMixRuleOutput{Selected: t.Selected, Forbidden: calc.ForbiddenPartners(t.Selected), Max: t.Max}
}

// ---- pest_priority ----

// constrainPriority：注入规则排序（防治对象主名，从高到低）。
func constrainPriority(r *guardrail.RuleOutputs, c *llm.RuleConstraints) bool {
	if r.Priority == nil {
		return false
	}
	c.PestPriority = &llm.PriorityConstraint{Order: priorityNames(*r.Priority)}
	return true
}

// priorityNames 把规则排序的防治对象 id 换成主名（无名称时用 id）。
func priorityNames(p guardrail.PriorityRule) []string {
	out := make([]string, 0, len(p.Order))
	for _, id := range p.Order {
		if ns := p.Names[id]; len(ns) > 0 {
			out = append(out, ns[0])
		} else {
			out = append(out, id)
		}
	}
	return out
}

// priorityRuleOutput 是优先级排序的 rule_output：规则排序（防治对象 id）。
type priorityRuleOutput struct {
	Order []string `json:"order"`
}

func priorityOutput(r *guardrail.RuleOutputs) any {
	return priorityRuleOutput{Order: append([]string{}, r.Priority.Order...)}
}
