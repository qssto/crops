package graph

import (
	"slices"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/guardrail"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// ---- P2 L4 规则与模型职责再切分：下沉项的规则结论（不读开关，拒收常开）----

// paramMaxProductsInTank 是 common/safety/mixing 的单桶产品数上限参数。
const paramMaxProductsInTank = "max_products_in_tank"

// ruleOutputs 是本轮下沉项的规则结论，交给 output_validator 拒收冲突输出（off / shadow / on 三态都传）。
// 只对出卡的建议计算；无规则结论的项为 nil（不校验）。
func (a *Agent) ruleOutputs(s *state) *guardrail.RuleOutputs {
	if s.stopped() || s.env == nil || s.env.snap == nil {
		return nil
	}
	return &guardrail.RuleOutputs{Window: a.windowRule(s), TankMix: s.tankMixRule(), Priority: s.priorityRule()}
}

// windowEntries 是作业窗口硬约束所在的知识条目（施药、施肥窗口）。
var windowEntries = []string{kbSpray, kbFertWindow}

// windowRule：规则层判定过的作业窗口硬约束（不含数据时效类）汇成窗口结论，时段取作业单建议时段；
// 本轮没有判定窗口约束（非作业类问题、无预报）时为 nil。
func (a *Agent) windowRule(s *state) *guardrail.WindowRule {
	var checks []calc.WindowCheck
	for _, j := range s.dec.Judged {
		if slices.Contains(windowEntries, j.KB) && !slices.Contains(staleMetrics, j.Th.Metric) {
			checks = append(checks, calc.WindowCheck{ID: j.Th.ID, Met: j.Met})
		}
	}
	if len(checks) == 0 {
		return nil
	}
	return &guardrail.WindowRule{Window: calc.OperationWindow(checks, s.now, s.now.Add(a.cfg.OrderWindow)), Loc: a.loc}
}

// tankMixRule：任何建议卡的模型原文都不得建议禁忌混用（不限一级类与建议类型）。所选药剂取规则层候选内的选择，
// 单桶上限取知识库参数（条目或参数缺失时只查禁忌组合）。
func (s *state) tankMixRule() *guardrail.TankMixRule {
	r := &guardrail.TankMixRule{Lexicon: s.env.lexicon()}
	if s.selected != "" && slices.Contains(s.candidates, s.selected) {
		r.Selected = s.env.commonName(s.selected)
	}
	if en, ok := s.env.snap.Entry(kbMixing); ok && !s.env.snap.Deprecated(kbMixing) {
		if p, ok := en.Param(paramMaxProductsInTank, nil); ok && p.Value != nil {
			r.Max = int(*p.Value)
		}
	}
	return r
}

// minPriorityTargets 是优先级排序适用的最少已判定防治对象数（只有一个对象时无排序可言）。
const minPriorityTargets = 2

// priorityRule：植保（非安全类）问题、虫情调查可用时，按虫情调查给出的观测值判定各防治对象条目的达标阈值
// （>= / > 类、非连续天数、非时效类、生育期适用），至少两个对象有判定时给出规则排序；否则为 nil。
func (s *state) priorityRule() *guardrail.PriorityRule {
	if s.category() != model.CategoryProtect || s.cls.Intent == llm.IntentSafety || !s.ok(toolPestSurvey) {
		return nil
	}
	var loads []calc.PestLoad
	names := map[string][]string{}
	for _, en := range s.env.snap.Entries() {
		if en.Target == "" || en.Crop != s.env.crop || s.env.snap.Deprecated(en.KBID) {
			continue
		}
		names[en.Target] = calc.TargetNames(s.env.snap.Registry().Targets[en.Target])
		if l, ok := s.pestLoad(en); ok {
			loads = append(loads, l)
		}
	}
	if distinctTargets(loads) < minPriorityTargets {
		return nil
	}
	return &guardrail.PriorityRule{Order: calc.PestPriority(loads), Names: names}
}

// pestLoad 判定一个防治对象条目：取虫情调查实测值，Exceed 取各适用达标阈值的实测值 / 阈值最大者，
// TooLate 为过晚阈值满足。没有可判定的达标阈值时返回 false。
func (s *state) pestLoad(en *knowledge.Entry) (calc.PestLoad, bool) {
	l := calc.PestLoad{Target: en.Target}
	judged := false
	for _, th := range en.Thresholds {
		if th.DurationDays > 0 || slices.Contains(staleMetrics, th.Metric) ||
			(len(th.Stage) > 0 && !slices.Contains(th.Stage, s.stage)) || s.facts[th.Metric].Tool != toolPestSurvey {
			continue
		}
		v, ok := s.facts.num(th.Metric)
		if !ok {
			continue
		}
		hit := compare(th.Op, v, th.Value)
		switch {
		case th.ID == thTooLate:
			l.TooLate = l.TooLate || hit
		case (th.Op == ">=" || th.Op == ">") && th.Value > 0:
			judged, l.Met, l.Exceed = true, l.Met || hit, max(l.Exceed, v/th.Value)
		}
	}
	return l, judged
}

// distinctTargets 是 loads 里不同防治对象的个数。
func distinctTargets(loads []calc.PestLoad) int {
	seen := map[string]bool{}
	for _, l := range loads {
		seen[l.Target] = true
	}
	return len(seen)
}
