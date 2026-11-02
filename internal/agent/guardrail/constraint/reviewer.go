package constraint

import (
	"encoding/json"
	"slices"

	"agri-steward/internal/model"
)

// Reviewer 是开票前的拦截判定（ApprovalService.Open 调用，纯函数）：以注册表必检集合 ∪ 法规对照表适用条目为
// 必须项，复核 graph 写入作业单的 constraint_checks。
type Reviewer struct {
	reg  *Registry
	regs *RegulationMap
}

// NewReviewer 绑定注册表与法规对照表（二者均必填：对照表须由同一注册表解析）。
func NewReviewer(reg *Registry, regs *RegulationMap) *Reviewer {
	return &Reviewer{reg: reg, regs: regs}
}

// Registry 是 Reviewer 使用的注册表（app 装配时交给 graph，保证开票前复核与 graph 生成校验行用同一实例）。
func (r *Reviewer) Registry() *Registry { return r.reg }

// 检查器在「不适用」类通过（observed.value 为空）时写的固定说明，Reviewer 复核时按原文比对。
const (
	noteNoPrevious  = "no previous " // + 作业类别（opKind）
	noteNoZone      = "no zone nearby"
	noteNoSensitive = "no sensitive adjacent field"
	noteNoDownwind  = "no sensitive field downwind"
)

// nilValueOK 是通过时允许 observed.value 为空的规则及其固定的「不适用」说明集合（无同类作业记录 / 附近无禁飞区 /
// 无敏感相邻地块）；其余规则通过时必须带数值观测值。复核第 23 条：空值通过行还须带 threshold.op（来自知识的阈值），
// 且 note 属于该规则的集合，缺一即改判失败。phi_interval 通过时总带天数裕量（复核第 19、20 条），不在此列。
var nilValueOK = map[string][]string{
	RuleRepeat: {noteNoPrevious + opKind[model.WorkSpray], noteNoPrevious + opKind[model.WorkFertilize],
		noteNoPrevious + opKind[model.WorkIrrigate]},
	RuleNoFly: {noteNoZone},
	RuleDrift: {noteNoSensitive, noteNoDownwind},
}

// Review 返回应落库的 PRE_APPROVAL 校验行与未通过的 rule_id（顺序：法规前置条件 → 未映射法规条目 → 约束）。
// 规则：未注册的行忽略、同一 rule_id 只取第一行；通过行缺 tool_call_id / source / threshold.kb 改判失败
// （前置条件不要求 tool_call_id），且按行内 threshold.op / value 与 observed.value 复算，不一致改判失败；
// 必须项缺行、适用的 unmapped 条目、注册表必检集合为空（不论是否有适用法规条目）都输出失败行。blocked 为空即放行。
func (r *Reviewer) Review(o model.WorkOrder) ([]model.ConstraintCheck, []string) {
	phase := model.PhasePreApproval
	required := r.reg.Required(phase, o.WorkType, o.DeviceType)
	noPlan := len(required) == 0
	var unmapped []model.ConstraintCheck
	for _, e := range r.regs.Applicable(o.WorkType, o.DeviceType, o.DispatchMode) {
		if e.Kind == KindUnmapped {
			unmapped = append(unmapped, Missing(phase, e.CheckRuleID(), "unmapped regulation: "+e.Title+"（待法务确认）"))
			continue
		}
		required = append(required, e.RuleID)
	}
	required = r.reg.sorted(required)
	given := make(map[string]model.ConstraintCheck, len(o.ConstraintChecks))
	for _, c := range o.ConstraintChecks {
		if _, dup := given[c.RuleID]; dup || !r.reg.Has(c.RuleID) {
			continue
		}
		given[c.RuleID] = r.vouch(c)
	}
	var pre, checks []model.ConstraintCheck
	if noPlan {
		checks = append(checks, Missing(phase, RuleNoPlan, "no constraint plan for "+string(o.WorkType)+"/"+o.DeviceType))
	}
	for _, id := range required {
		c, ok := given[id]
		if !ok {
			c = Missing(phase, id, "required check missing")
		}
		c.Phase, c.Attempt = phase, 0
		if r.reg.IsPrecondition(id) {
			pre = append(pre, c)
		} else {
			checks = append(checks, c)
		}
	}
	rows := slices.Concat(pre, unmapped, checks)
	var blocked []string
	for _, c := range rows {
		if !c.Passed {
			blocked = append(blocked, c.RuleID)
		}
	}
	return rows, blocked
}

// vouch 复核通过行的证据：缺 tool_call_id（前置条件除外）/ source / threshold.kb、无法解析，或按 threshold 复算
// observed.value 不通过，即改判失败。
func (r *Reviewer) vouch(c model.ConstraintCheck) model.ConstraintCheck {
	if !c.Passed {
		return c
	}
	var obs Observed
	var th Threshold
	errObs, errTh := json.Unmarshal(c.Observed, &obs), json.Unmarshal(c.Threshold, &th)
	note := ""
	switch {
	case errObs != nil || errTh != nil:
		note = "evidence not decodable"
	case th.KB == "":
		note = "evidence without threshold kb"
	case obs.Source == "":
		note = "evidence without source"
	case obs.ToolCallID == 0 && !r.reg.IsPrecondition(c.RuleID):
		note = "evidence without tool_call_id"
	default:
		note = r.recompute(c.RuleID, obs, th)
		if note == "" {
			return c
		}
	}
	obs.Note = note
	c.Passed, c.Observed = false, mustJSON(obs)
	if errTh != nil {
		c.Threshold = mustJSON(Threshold{Signoff: SignoffPending})
	}
	return c
}

// recompute 按行内阈值复算通过行：数值（布尔按 1 / 0）须满足 compare(op, value, threshold.value)；
// 空值只允许前置条件，以及带 threshold.op 且 note 属于固定「不适用」集合的 nilValueOK 规则；其他类型一律不通过。
// 返回失败说明（空串为一致）。
func (r *Reviewer) recompute(id string, obs Observed, th Threshold) string {
	var x float64
	switch t := obs.Value.(type) {
	case nil:
		notes, ok := nilValueOK[id]
		switch {
		case r.reg.IsPrecondition(id):
			return ""
		case !ok:
			return "passed without observed value"
		case th.Op == "":
			return "passed without observed value or threshold"
		case !slices.Contains(notes, obs.Note):
			return "passed without observed value, note is not a fixed not-applicable reason"
		}
		return ""
	case float64:
		x = t
	case bool:
		if t {
			x = 1
		}
	default:
		return "observed value is not numeric"
	}
	if !compare(th.Op, x, th.Value) {
		return "passed does not match threshold"
	}
	return ""
}
