package graph

import (
	"fmt"
	"regexp"
	"slices"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// 施肥规则用到的阈值 id 与常量。
const (
	thPromoteLeafAge = "promote-leaf-age"
	thProtectLeafAge = "protect-leaf-age"
	thLeafColorHigh  = "leaf-color-high"
	topicDeficiency  = "fertilize.deficiency"
	nutrientN        = "n"
	nameKH2PO4       = "磷酸二氢钾"
)

// fertVetoes 是"满足即不作业"的阈值（叶色过深推迟或减量）。
var fertVetoes = []string{thLeafColorHigh}

var potassiumWords = regexp.MustCompile(`钾`)

// fertProductNames 是肥料实物的中文名（进工单 payload 与数值标签）。
var fertProductNames = map[calc.FertProduct]string{calc.ProductUrea: "尿素", calc.ProductCompound: "复合肥"}

// rescueParams 是缺素条目的补施参数：元素 → (参数 id, 数值字段前缀, 标签)。
var rescueParams = map[string][3]string{
	"n": {"rescue_urea_per_mu", "urea_per_mu", "补施尿素亩用量"},
	"k": {"rescue_kcl_per_mu", "kcl_per_mu", "补施氯化钾亩用量"},
}

// decideFertilize：缺素诊断 → 观察类；叶面肥 → 作业窗口；其余按施肥时期条目判定时机，再按氮肥运筹算用量。
func decideFertilize(e *env, s *state) error {
	s.dec.WorkType, s.dec.Sprayer = model.WorkFertilize, sprayerOf(s.cls.Slots)
	switch {
	case s.cls.Intent == llm.IntentIdentify || s.cls.Topic == topicDeficiency:
		return e.decideDeficiency(s)
	case s.cls.Slots.Foliar:
		return e.decideFoliar(s)
	}
	return e.decideFertStage(s)
}

// decideDeficiency：缺素诊断只给观察类，有补施参数时给出补施用量区间。
func (e *env) decideDeficiency(s *state) error {
	nu := s.cls.Slots.Nutrient
	if nu == "" {
		nu = nutrientN
	}
	en, err := e.entry(kbDeficiencyPrefix + nu)
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	for _, th := range en.Thresholds {
		e.judge(s, en, th.ID)
	}
	note(s, en, "按知识库的症状特征现场核实，确诊后再补施")
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "症状疑似缺素，先现场核实再补施"
	if p, ok := rescueParams[nu]; ok {
		return e.paramRange(s, en, p[0], p[1], p[2], precisionKg)
	}
	return nil
}

// decideFoliar：叶面喷施磷酸二氢钾，按施药作业窗口判定。
func (e *env) decideFoliar(s *state) error {
	en, err := e.entry(kbFoliar)
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	s.dec.Product = nameKH2PO4
	note(s, en, "叶面喷施磷酸二氢钾按知识库亩用量与兑水量配制")
	if err := e.foliarNumbers(s, en); err != nil {
		return err
	}
	op := true
	if s.ok(toolForecast) {
		if op, err = e.sprayWindow(s); err != nil {
			return err
		}
	}
	if s.dec.Sprayer == calc.SprayerUAV {
		if err := e.noteKB(s, calc.KBUAV, "无人机飞防按知识库的亩施药液量、作业高度与速度执行"); err != nil {
			return err
		}
	}
	if !op {
		s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "天气不满足作业窗口，暂缓叶面喷施"
		return nil
	}
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "可以叶面喷施，建议在作业窗口内作业"
	return nil
}

// foliarNumbers：亩用量区间、亩兑水量，有面积时给整田用量区间（克换算为公斤）。
func (e *env) foliarNumbers(s *state, en *knowledge.Entry) error {
	if err := e.paramRange(s, en, "kh2po4_per_mu", "kh2po4_per_mu", "磷酸二氢钾亩用量", precisionGram); err != nil {
		return err
	}
	w, err := e.param(en, "foliar_water_per_mu")
	if err != nil {
		return err
	}
	if w.Value == nil {
		return refuse(RefuseKBMiss, "param foliar_water_per_mu in %s has no value", en.Ref())
	}
	s.addNumber("water_per_mu", "亩兑水量", en.KBID, paramResult(en, w, "value", *w.Value, precisionCount))
	area, ok := s.facts.num(factAreaMu)
	if !ok {
		return nil
	}
	for _, n := range slices.Clone(s.numbers) {
		if n.Field == "kh2po4_per_mu_min" || n.Field == "kh2po4_per_mu_max" {
			field := "kh2po4_total" + n.Field[len("kh2po4_per_mu"):]
			r := scaled(n.Result, area/gramsPerKg, "kg", fmt.Sprintf("× area_mu(%s) ÷ %d", fmtNum(area), gramsPerKg), precisionKg)
			s.addNumber(field, "磷酸二氢钾整田用量"+n.Label[len("磷酸二氢钾亩用量"):], en.KBID, r)
		}
	}
	return nil
}

// fertStageOf 取施肥时期：优先用分类槽位，缺省时按当前生育期推断。
func fertStageOf(s *state) calc.FertStage {
	if st := s.cls.Slots.FertStage; st != "" {
		return calc.FertStage(st)
	}
	switch s.stage {
	case "秧苗期":
		return calc.FertBasal
	case "返青期", "分蘖期":
		return calc.FertTillering
	case "拔节孕穗期":
		return calc.FertPaniclePromote
	}
	return ""
}

// fertStageEntry 是施肥时期对应的知识条目。
func fertStageEntry(st calc.FertStage) (string, bool) {
	switch st {
	case calc.FertBasal:
		return kbBasal, true
	case calc.FertTillering:
		return calc.KBTillering, true
	case calc.FertPaniclePromote, calc.FertPanicleProtect:
		return kbPanicle, true
	}
	return "", false
}

// decideFertStage：时期条目阈值 → 近期是否追过肥 → 施肥作业窗口；作业类或问用量时按氮肥运筹算量。
func (e *env) decideFertStage(s *state) error {
	st := fertStageOf(s)
	id, ok := fertStageEntry(st)
	if !ok {
		return refuse(RefuseKBMiss, "no fertilization entry for stage %q (%s)", st, s.stage)
	}
	en, err := e.entry(id)
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	op := true
	for _, th := range en.Thresholds {
		v := e.judge(s, en, th.ID)
		if (v == unmet && !slices.Contains(fertVetoes, th.ID)) || (v == met && slices.Contains(fertVetoes, th.ID)) {
			op = false
		}
	}
	if op {
		if op, err = e.recentFertilize(s); err != nil {
			return err
		}
	}
	if op && s.ok(toolForecast) {
		if op, err = e.fertWindow(s); err != nil {
			return err
		}
	}
	if p := s.cls.Slots.FertProduct; p != "" {
		s.dec.Product = fertProductNames[calc.FertProduct(p)]
	}
	if !op {
		s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "暂不建议追肥，按知识库的施肥时机再定"
	} else {
		s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "可以施肥，建议在施肥窗口内作业"
	}
	if s.cls.Intent == llm.IntentRate || (op && s.cls.Slots.FertProduct != "") {
		if err := e.fertNumbers(s, st); err != nil {
			return err
		}
	}
	if st == calc.FertBasal && potassiumWords.MatchString(s.q.Text) {
		return e.paramRange(s, en, "basal_kcl_per_mu", "kcl_per_mu", "基肥氯化钾亩用量", precisionKg)
	}
	return nil
}

// recentFertilize：两次追肥间隔不足 7 天（施肥作业窗口要点）则不建议再追。
func (e *env) recentFertilize(s *state) (bool, error) {
	o := s.tools[toolHistory]
	if o == nil || o.Data == nil {
		return true, nil
	}
	lf, ok := o.Data[factLastFertilize].(map[string]any)
	if !ok {
		return true, nil
	}
	date, ok := lf["date"].(string)
	if !ok {
		return true, nil
	}
	d, ok := civilDays(date, s.now, e.loc)
	if !ok || d >= recentFertilizeDays {
		return true, nil
	}
	if err := e.noteKB(s, kbFertWindow, "距上次追肥不足知识库要求的间隔天数，不建议重复追施", factLastFertilize); err != nil {
		return false, err
	}
	return false, nil
}

// fertWindow 判定施肥作业窗口（降雨、水层）。
func (e *env) fertWindow(s *state) (bool, error) {
	en, err := e.entry(kbFertWindow)
	if err != nil {
		return false, err
	}
	s.dec.cite(en.KBID)
	if err := e.requireFresh(s, en); err != nil {
		return false, err
	}
	ok := true
	for _, th := range en.Thresholds {
		if !slices.Contains(staleMetrics, th.Metric) && e.judge(s, en, th.ID) == unmet {
			ok = false
		}
	}
	return ok, nil
}

// fertNumbers 按氮肥运筹算亩用量与整田用量；分两次时给两次的亩用量，只问一次时给该次用量。
func (e *env) fertNumbers(s *state, st calc.FertStage) error {
	sl := s.cls.Slots
	area, ok := s.facts.num(factAreaMu)
	if !ok {
		return refuse(RefuseDataMissing, "area_mu is required for fertilizer amount")
	}
	soil, ok := s.facts.str(factSoilN)
	if !ok {
		return refuse(RefuseDataMissing, "soil_n_level is required for fertilizer amount")
	}
	p := calc.FertProduct(sl.FertProduct)
	in := calc.FertInput{SoilNLevel: soil, Stage: st, Product: p, Split: calc.FertSplit(sl.SplitPart), AreaMu: area}
	r, err := e.calc.Fertilizer(in)
	if err != nil {
		return fmt.Errorf("fertilizer: %w", err)
	}
	name, prefix := fertProductNames[p], string(p)
	s.addNumber(prefix+"_per_mu", name+"亩用量", calc.KBNBudget, r.ProductPerMu)
	if sl.Split && in.Split == calc.SplitNone && st == calc.FertTillering {
		for _, part := range []calc.FertSplit{calc.SplitFirst, calc.SplitSecond} {
			in.Split = part
			pr, err := e.calc.Fertilizer(in)
			if err != nil {
				return fmt.Errorf("fertilizer %s: %w", part, err)
			}
			s.addNumber(prefix+"_"+string(part)+"_per_mu", name+"分次亩用量", calc.KBNBudget, pr.ProductPerMu)
		}
	}
	s.addNumber(prefix+"_total", name+"整田用量", calc.KBNBudget, r.ProductTotal)
	if sl.BagKg != nil && *sl.BagKg > 0 {
		bag := *sl.BagKg
		s.addNumber("bags_"+fmtNum(bag)+"kg", "袋数", calc.KBNBudget,
			scaled(r.ProductTotal, 1/bag, "袋", fmt.Sprintf("÷ bag_kg(%s)", fmtNum(bag)), precisionKg))
	}
	return nil
}
