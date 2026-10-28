package graph

import (
	"regexp"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// 灾害规则用到的阈值 id。
const (
	thBootingCold = "booting-cold"
)

var (
	sprayWords  = regexp.MustCompile(`打药|喷药|施药|飞防`)
	rescueWords = regexp.MustCompile(`补肥|补多少|追肥`)
)

// decideDisaster：取灾种条目判定阈值；叶面喷施（灾后促灌浆）走叶面肥作业窗口；
// 孕穗期低温灌深水保温、淹水超耐受抢排或灾后补肥为作业类，其余为观察类。
func decideDisaster(e *env, s *state) error {
	hz := s.cls.Slots.Hazard
	if hz == "" {
		return refuse(RefuseKBMiss, "hazard not recognized")
	}
	en, err := e.entry(kbDisasterPrefix + hz)
	if err != nil {
		return err
	}
	s.dec.cite(en.KBID)
	if s.cls.Slots.Foliar {
		s.dec.WorkType, s.dec.Sprayer = model.WorkFertilize, sprayerOf(s.cls.Slots)
		note(s, en, "灾后按知识库要点恢复管理")
		return e.decideFoliar(s)
	}
	s.dec.WorkType = model.WorkIrrigate
	vs := map[string]verdict{}
	for _, th := range en.Thresholds {
		if v := e.judge(s, en, th.ID); v != notApplicable {
			vs[th.ID] = v
		}
	}
	if len(vs) == 0 && hz != llm.HazardLodging {
		return refuse(RefuseDataMissing, "%s: no observation for any threshold", en.KBID)
	}
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeObservation, "按知识库的防御要点加强田间管理，密切关注天气"
	switch hz {
	case llm.HazardCold:
		if vs[thBootingCold] == met {
			s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "孕穗期低温，建议灌深水保温"
			return e.paramRange(s, en, "warm_water_cm", "warm_water_cm", "保温深水层", precisionDepth)
		}
	case llm.HazardFlood:
		return e.decideFlood(s, vs)
	case llm.HazardTyphoon:
		if sprayWords.MatchString(s.q.Text) {
			s.dec.Text = "大风暴雨前后不宜施药，待天气转好再按作业窗口施药"
			return e.noteKB(s, kbSpray, "施药须满足知识库的风速、降雨作业窗口")
		}
	case llm.HazardLodging:
		s.dec.Text = "旺长有倒伏风险，控氮并适度重晒田"
		if err := e.noteKB(s, kbPanicle, "叶色过深或旺长时穗肥减量或不施"); err != nil {
			return err
		}
		return e.noteKB(s, kbDrain, "够苗后及时排水晒田，控制无效分蘖、壮秆防倒")
	}
	return nil
}

// decideFlood：淹水超过当期耐受时长须抢排；问补肥时给退水后补施尿素区间。
func (e *env) decideFlood(s *state, vs map[string]verdict) error {
	en, err := e.entry(kbDisasterPrefix + llm.HazardFlood)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if v == met {
			s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "淹水超过耐受时长，建议立即抢排"
		}
	}
	if !rescueWords.MatchString(s.q.Text) {
		return nil
	}
	s.dec.WorkType, s.dec.Product = model.WorkFertilize, fertProductNames[calc.ProductUrea]
	s.dec.AdviceType, s.dec.Text = model.AdviceTypeOperation, "退水后及时补施尿素恢复长势"
	return e.paramRange(s, en, "rescue_urea_per_mu", "urea_per_mu", "退水后补施尿素亩用量", precisionKg)
}
