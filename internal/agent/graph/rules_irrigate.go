package graph

import (
	"fmt"
	"regexp"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// 生育期名（phenology.stage）。
const (
	stageSeedling = "秧苗期"
	stageRegreen  = "返青期"
	stageTiller   = "分蘖期"
	stageBooting  = "拔节孕穗期"
	stageHeading  = "抽穗扬花期"
	stageFilling  = "灌浆结实期"
	stageMature   = "成熟期"
)

var rewaterWords = regexp.MustCompile(`复水`)

// depthParam 是各生育期的水层参数：生育期 → (条目, 参数 id)。
var depthParam = map[string][2]string{
	stageRegreen: {kbSeedlingTillering, "regreening_depth_cm"},
	stageTiller:  {kbSeedlingTillering, "tillering_depth_cm"},
	stageBooting: {kbHeading, "heading_depth_cm"},
	stageHeading: {kbHeading, "heading_depth_cm"},
}

// decideIrrigate：按意图分支（晒田 / 断水、水层、施药后保水、现在灌不灌、灌水量）。
func decideIrrigate(e *env, s *state) error {
	s.dec.WorkType = model.WorkIrrigate
	switch s.cls.Intent {
	case llm.IntentDrain:
		return e.decideDrain(s)
	case llm.IntentAfterSpray:
		return e.decideAfterSpray(s)
	case llm.IntentIrrigateNow:
		return e.decideIrrigateNow(s)
	case llm.IntentAmount:
		return e.decideWaterAmount(s)
	}
	return e.decideDepth(s)
}

// decideDrain：抽穗扬花期不断水（转水层管理）；灌浆后按收获前断水天数；其余按晒田条目判定，只给观察类。
func (e *env) decideDrain(s *state) error {
	s.dec.AdviceType = model.AdviceTypeObservation
	switch s.stage {
	case stageHeading:
		return e.decideDepth(s)
	case stageFilling, stageMature:
		en, err := e.entry(kbGrainFilling)
		if err != nil {
			return err
		}
		s.dec.cite(en.KBID)
		note(s, en, "灌浆后期干湿交替，收获前按知识库天数断水", factExpectedHarvest)
		s.dec.Text = "按收获前断水天数安排断水"
		return e.paramRange(s, en, "cutoff_days_before_harvest", "cutoff_days", "收获前断水天数", precisionDays)
	}
	en, err := e.entry(kbDrain)
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	ok := true
	for _, th := range en.Thresholds {
		if e.judge(s, en, th.ID) != met {
			ok = false
		}
	}
	s.dec.Text = "暂不晒田，继续观察苗数与天气"
	if ok {
		s.dec.Text = "达到晒田条件，可排水晒田并按知识库天数控制"
	}
	return nil
}

// decideDepth：按当前生育期给适宜水层区间（观察类）。
func (e *env) decideDepth(s *state) error {
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "按当前生育期保持适宜水层"
	p, ok := depthParam[s.stage]
	if !ok {
		return refuse(RefuseKBMiss, "no water depth param for stage %q", s.stage)
	}
	en, err := e.entry(p[0])
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	note(s, en, "当前生育期按知识库水层管理", factStage)
	return e.paramRange(s, en, p[1], "depth_cm", "适宜水层", precisionDepth)
}

// decideAfterSpray：施药后按知识库保水水层与天数管理（作业类）。
func (e *env) decideAfterSpray(s *state) error {
	en, err := e.entry(kbSprayWater)
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	note(s, en, "施药后保持水层，保水期内不排水、不串灌")
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "施药后保持田间水层，暂不排水"
	return e.paramRange(s, en, "keep_water_depth_cm", "depth_cm", "施药后保水水层", precisionDepth)
}

// decideIrrigateNow：问复水 → 晒田后复水水层；否则实测水层低于当前生育期下限（分蘖期为落干补水线）即灌水。
func (e *env) decideIrrigateNow(s *state) error {
	if rewaterWords.MatchString(s.q.Text) {
		en, err := e.entry(kbRewater)
		if err != nil {
			return err
		}
		s.dec.cite(en.KBID)
		if err := e.noteKB(s, kbDrain, "晒田达到知识库要求的天数与田面状态后复水", factLastDrain); err != nil {
			return err
		}
		note(s, en, "复水后干湿交替，按知识库复水水层灌浅水", factWaterDepth)
		s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "可以复水，按知识库复水水层灌浅水"
		return e.paramPoint(s, en, "rewater_depth_cm", "rewater_depth_cm", "复水水层", precisionDepth)
	}
	depth, ok := s.facts.num(factWaterDepth)
	if !ok {
		return refuse(RefuseDataMissing, "water_depth_cm is required to decide irrigation")
	}
	p, ok := depthParam[s.stage]
	if !ok {
		return refuse(RefuseKBMiss, "no water depth param for stage %q", s.stage)
	}
	en, err := e.entry(p[0])
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	pid, bound := p[1], "min"
	if s.stage == stageTiller {
		pid, bound = "irrigate_trigger_depth_cm", "value"
	}
	prm, err := e.param(en, pid)
	if err != nil {
		return err
	}
	low := prm.Value
	if bound == "min" {
		low = prm.Min
	}
	if low == nil {
		return refuse(RefuseKBMiss, "param %s in %s has no %s", pid, en.Ref(), bound)
	}
	need := depth <= *low
	if bound == "min" {
		need = depth < *low
	}
	if !need {
		note(s, en, "实测水层未低于知识库补水下限，暂不灌水", factWaterDepth)
		s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "田间水层够，暂不灌水"
		return nil
	}
	note(s, en, "实测水层已低于知识库补水下限，需及时灌浅水", factWaterDepth)
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "水层不足，建议今天灌浅水"
	return nil
}

// decideWaterAmount：灌水量 = 面积 × 水层差 × 损失系数（晒田后复水用复水系数）；面积、实测水层、目标水层缺一即数据缺失。
func (e *env) decideWaterAmount(s *state) error {
	area, ok := s.facts.num(factAreaMu)
	if !ok {
		return refuse(RefuseDataMissing, "area_mu is required for water volume")
	}
	cur, ok := s.facts.num(factWaterDepth)
	if !ok {
		return refuse(RefuseDataMissing, "water_depth_cm is required for water volume")
	}
	target := s.cls.Slots.TargetDepthCm
	if target == nil {
		return refuse(RefuseDataMissing, "target depth is required for water volume")
	}
	en, err := e.entry(calc.KBWaterVolume)
	if err != nil {
		return err
	}
	afterDrain := s.facts.has(factLastDrain) && cur == 0
	facts := []string{factAreaMu, factWaterDepth}
	if afterDrain {
		facts = append(facts, factLastDrain)
	}
	note(s, en, "按田块面积、实测水层与目标水层计算灌水量，计入渗漏损失", facts...)
	if afterDrain && s.stage == stageBooting {
		if err := e.noteKB(s, kbRewater, "晒田后复水按知识库复水水层灌浅水", factLastDrain); err != nil {
			return err
		}
	}
	if s.stage == stageHeading {
		if err := e.noteKB(s, kbHeading, "抽穗扬花期保持知识库水层", factStage); err != nil {
			return err
		}
	}
	r, err := e.calc.WaterVolume(area, *target, cur, afterDrain)
	if err != nil {
		return fmt.Errorf("water volume: %w", err)
	}
	s.addNumber("water_m3", "灌水量", calc.KBWaterVolume, r)
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "按计算的灌水量灌到目标水层"
	return nil
}
