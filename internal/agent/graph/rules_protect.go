package graph

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// 植保规则用到的阈值 id 与时效类 metric。
const (
	thWindKnapsack = "wind-knapsack"
	thWindUAV      = "wind-uav"
	thTooLate      = "too-late"
	thClosed       = "closed-treatment"
	methodSoil     = "药土法"
)

// staleMetrics 是数据时效类 metric：不满足即数据缺失（DATA_MISSING），不参与作业判定。
var staleMetrics = []string{factSurveyAge, factWeatherAge}

// 植保问题关键词。
var (
	harvestWords = regexp.MustCompile(`收割|收获`)
	closedWords  = regexp.MustCompile(`封闭`)
	mixWords     = regexp.MustCompile(`混`)
	ppeWords     = regexp.MustCompile(`防护|几个小时`)
)

// protectTools：安全类知识问答不需要田块数据；其余取生育期、档案、农事、虫情、预报、近期天气。
func protectTools(s *state) []string {
	if s.cls.Intent == llm.IntentSafety {
		return nil
	}
	return []string{toolPhenology, toolProfile, toolHistory, toolPestSurvey, toolForecast, toolWeatherRecent}
}

func sprayerOf(sl llm.Slots) calc.Sprayer {
	if sl.Sprayer == llm.SprayerUAV {
		return calc.SprayerUAV
	}
	return calc.SprayerKnapsack
}

// decideProtect：防治对象条目阈值 → 作业窗口 → 候选约束（抗性轮换、施药方式、安全间隔期）。
func decideProtect(e *env, s *state) error {
	s.dec.WorkType, s.dec.Sprayer = model.WorkSpray, sprayerOf(s.cls.Slots)
	if s.cls.Intent == llm.IntentSafety {
		return e.decideSafety(s)
	}
	en, err := e.targetEntry(s.cls.Target)
	if err != nil {
		return err
	}
	s.dec.Target = s.cls.Target
	s.dec.cite(en.KBID)
	if err := e.requireFresh(s, en); err != nil {
		return err
	}
	vs, err := e.judgeTarget(s, en)
	if err != nil {
		return err
	}
	op := s.cls.Intent != llm.IntentIdentify && (len(vs) == 0 || anyMet(vs)) && vs[thTooLate] != met
	if op && s.ok(toolForecast) {
		if op, err = e.sprayWindow(s); err != nil {
			return err
		}
	}
	if s.dec.Sprayer == calc.SprayerUAV {
		if err := e.noteKB(s, calc.KBUAV, "无人机飞防按知识库的亩施药液量、作业高度与速度执行"); err != nil {
			return err
		}
	}
	if s.dec.Target == "stem_borer" || s.dec.Target == "weeds" {
		if err := e.noteKB(s, kbSprayWater, "施药前后按知识库保持田间水层"); err != nil {
			return err
		}
	}
	if s.dec.Target == "weeds" && (closedWords.MatchString(s.q.Text) || vs[thClosed] == met) {
		s.dec.Method = methodSoil
	}
	if !op {
		s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "暂不建议施药，继续观察并按时调查"
		return nil
	}
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "达到防治条件，建议在作业窗口内施药"
	return e.constrainCandidates(s)
}

// decideSafety：混配 / 防护等安全类知识问答，只给观察类。
func (e *env) decideSafety(s *state) error {
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "按知识库的安全规范执行"
	q := s.q.Text
	if mixWords.MatchString(q) {
		if err := e.noteKB(s, kbMixing, "桶混须先查标签与混配禁忌，单桶产品数不超过知识库上限，先小样试配"); err != nil {
			return err
		}
		if en, err := e.targetEntry(s.cls.Target); err == nil {
			note(s, en, "防治对象的用药时期与方法见知识库条目")
		}
	}
	if ppeWords.MatchString(q) {
		en, err := e.entry(kbPPE)
		if err != nil {
			return err
		}
		note(s, en, "施药须穿戴防护服、口罩、手套，单日作业时长不超过知识库上限")
		if err := e.paramPoint(s, en, "max_work_hours_per_day", "max_hours_per_day", "每日作业时长上限", precisionCount); err != nil {
			return err
		}
	}
	if len(s.dec.Reasons) == 0 {
		return e.noteKB(s, kbRegistration, "只使用在本作物、本防治对象上登记的药剂，按标签用量施用")
	}
	return nil
}

// noteKB 记一条只引用知识条目 id 的理由。
func (e *env) noteKB(s *state, id, text string, facts ...string) error {
	en, err := e.entry(id)
	if err != nil {
		return err
	}
	note(s, en, text, facts...)
	return nil
}

// requireFresh：条目里的数据时效阈值不满足即数据缺失。
func (e *env) requireFresh(s *state, en *knowledge.Entry) error {
	for _, th := range en.Thresholds {
		if slices.Contains(staleMetrics, th.Metric) && e.judge(s, en, th.ID) == unmet {
			return refuse(RefuseDataMissing, "%s: %s stale", en.KBID, th.Metric)
		}
	}
	return nil
}

// judgeTarget 判定防治对象条目的业务阈值（不含时效类），返回已判定的阈值 → 结论。
// 条目有阈值却一个都判不了（缺观测数据）时，除识别类外按数据缺失拒答。
func (e *env) judgeTarget(s *state, en *knowledge.Entry) (map[string]verdict, error) {
	vs := map[string]verdict{}
	total := 0
	for _, th := range en.Thresholds {
		if slices.Contains(staleMetrics, th.Metric) {
			continue
		}
		total++
		if v := e.judge(s, en, th.ID); v != notApplicable {
			vs[th.ID] = v
		}
	}
	if total > 0 && len(vs) == 0 && s.cls.Intent != llm.IntentIdentify {
		return nil, refuse(RefuseDataMissing, "%s: no observation for any threshold", en.KBID)
	}
	return vs, nil
}

// anyMet 表示有业务阈值满足（"预防过晚"是否决条件，不算）。
func anyMet(vs map[string]verdict) bool {
	for id, v := range vs {
		if v == met && id != thTooLate {
			return true
		}
	}
	return false
}

// sprayWindow 判定施药作业窗口（风速只取当前施药方式的上限）；预报过期按数据缺失拒答。
func (e *env) sprayWindow(s *state) (bool, error) {
	en, err := e.entry(kbSpray)
	if err != nil {
		return false, err
	}
	s.dec.cite(en.KBID)
	if err := e.requireFresh(s, en); err != nil {
		return false, err
	}
	skip := thWindUAV
	if s.dec.Sprayer == calc.SprayerUAV {
		skip = thWindKnapsack
	}
	ok := true
	for _, th := range en.Thresholds {
		if th.ID == skip || slices.Contains(staleMetrics, th.Metric) {
			continue
		}
		if e.judge(s, en, th.ID) == unmet {
			ok = false
		}
	}
	return ok, nil
}

// constrainCandidates 记录候选约束：抗性轮换排除近期用过的药、点名未登记药剂、临近收获的安全间隔期。
func (e *env) constrainCandidates(s *state) error {
	reg := e.snap.Registry()
	used, usedKey := usedProducts(s)
	for _, name := range used {
		for _, p := range reg.Products {
			if p.CommonName == name && !slices.Contains(s.dec.Exclude, p.ID) {
				s.dec.Exclude = append(s.dec.Exclude, p.ID)
			}
		}
	}
	if len(s.dec.Exclude) > 0 {
		if err := e.noteKB(s, kbResistance, "近期已用过同类药剂，按抗性治理要求轮换作用机理不同的药剂", usedKey); err != nil {
			return err
		}
	}
	all, err := e.calc.Candidates(e.crop, s.dec.Target, e.region)
	if err != nil {
		return err
	}
	for _, p := range reg.Products {
		if p.CommonName != "" && strings.Contains(s.q.Text, p.CommonName) && !slices.Contains(all, p.ID) {
			s.dec.MentionedOut = true
		}
	}
	if s.dec.MentionedOut {
		if err := e.noteKB(s, kbRegistration, "问题点名的药剂未在本作物、本防治对象上登记，不得使用，只在登记药剂中选择"); err != nil {
			return err
		}
	}
	if harvestWords.MatchString(s.q.Text) {
		if h, ok := s.facts.str(factExpectedHarvest); ok {
			if d, ok := civilDays(h, s.now, e.loc); ok {
				left := -d
				s.dec.MaxPHIDays = &left
				if err := e.noteKB(s, calc.KBPHI, "临近收获，只选安全间隔期不超过距收获天数的药剂", factExpectedHarvest); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// usedProducts 是农事记录里近期用过的药剂通用名（last_spray.product 与 sprays[].product）及其事实名。
func usedProducts(s *state) ([]string, string) {
	o := s.tools[toolHistory]
	if o == nil || o.Data == nil {
		return nil, ""
	}
	var out []string
	key := ""
	if ls, ok := o.Data[factLastSpray].(map[string]any); ok {
		if p, ok := ls["product"].(string); ok {
			out, key = append(out, p), factLastSpray
		}
	}
	if arr, ok := o.Data[factSprays].([]any); ok {
		for _, x := range arr {
			if m, ok := x.(map[string]any); ok {
				if p, ok := m["product"].(string); ok {
					out = append(out, p)
					if key == "" {
						key = factSprays
					}
				}
			}
		}
	}
	return out, key
}

// admits 判断登记候选是否满足规则层约束：不在抗性排除集、施药方式一致、安全间隔期不超过距收获天数。
func (e *env) admits(s *state, productID string) (bool, error) {
	if slices.Contains(s.dec.Exclude, productID) {
		return false, nil
	}
	g, err := e.calc.Registration(e.use(s, productID))
	if err != nil {
		return false, fmt.Errorf("registration %s: %w", productID, err)
	}
	if s.dec.Method != "" && g.Method != s.dec.Method {
		return false, nil
	}
	if m := s.dec.MaxPHIDays; m != nil && (g.PHIDays == nil || *g.PHIDays > *m) {
		return false, nil
	}
	return true, nil
}

// computeProtect 在选药之后补算：安全间隔期、亩用量区间、亩施药液量、整田用量或按倍数配药量。
func computeProtect(e *env, s *state) error {
	if s.selected == "" {
		return nil
	}
	u := e.use(s, s.selected)
	g, err := e.calc.Registration(u)
	if err != nil {
		return err
	}
	if s.dec.MaxPHIDays != nil && g.PHIDays != nil {
		s.addNumber("phi_days", "安全间隔期", calc.KBPHI, calc.Result{Value: float64(*g.PHIDays), Unit: "天",
			Precision: precisionDays, Trace: calc.Trace{Inputs: map[string]any{"product_id": s.selected,
				"target": s.dec.Target}, Formula: e.calc.RegistryRef()}})
	}
	if s.cls.Intent != llm.IntentDosage {
		return nil
	}
	if g.Dose.Unit == knowledge.DoseUnitRatio {
		sl := s.cls.Slots
		if sl.DilutionRatio == nil || sl.WaterL == nil {
			return refuse(RefuseDataMissing, "dilution ratio and water volume are required for %s", s.selected)
		}
		r, err := e.calc.DoseFromDilution(u, *sl.DilutionRatio, *sl.WaterL)
		if err != nil {
			return err
		}
		s.addNumber("product_ml", "配药量", calc.KBDosageDilution, r)
		return nil
	}
	return e.perMuNumbers(s, u, g)
}

// perMuNumbers 输出按亩登记药剂的亩用量区间、亩施药液量，有面积时再给整田用量区间。
func (e *env) perMuNumbers(s *state, u calc.Use, g knowledge.Registration) error {
	ref := e.calc.RegistryRef()
	for _, b := range []struct {
		field, label string
		v            float64
	}{{"dose_per_mu_min", "亩用量下限", g.Dose.Min}, {"dose_per_mu_max", "亩用量上限", g.Dose.Max}} {
		s.addNumber(b.field, b.label, calc.KBDosageDilution, calc.Result{Value: b.v, Unit: g.Dose.Unit,
			Precision: calc.PrecisionDosePerMu, Trace: calc.Trace{Inputs: map[string]any{"product_id": u.ProductID,
				"target": u.Target, "bound": b.field}, Formula: ref}})
	}
	vol, err := e.calc.SprayVolume(s.dec.Sprayer)
	if err != nil {
		return err
	}
	volKB := calc.KBDosageDilution
	if s.dec.Sprayer == calc.SprayerUAV {
		volKB = calc.KBUAV
	}
	s.addNumber("water_per_mu", "亩施药液量", volKB, vol)
	area, ok := s.facts.num(factAreaMu)
	if !ok {
		return nil
	}
	for _, b := range []struct {
		field, label string
		v            float64
	}{{"product_total_min", "整田用药量下限", g.Dose.Min}, {"product_total_max", "整田用药量上限", g.Dose.Max}} {
		r, err := e.calc.Dosage(u, b.v, area)
		if err != nil {
			return fmt.Errorf("%s: %w", b.field, err)
		}
		s.addNumber(b.field, b.label, calc.KBDosageDilution, r)
	}
	return nil
}
