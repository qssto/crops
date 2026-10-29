package graph

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

// 规则层直接引用的知识条目 kb_id（按 id 精确取，缺失即 KB_MISS）。
const (
	kbSpray             = "common/operation_window/spray"
	kbFertWindow        = "common/operation_window/fertilize"
	kbMixing            = "common/safety/mixing"
	kbResistance        = "common/safety/resistance"
	kbPPE               = "common/safety/ppe"
	kbRegistration      = "common/safety/registration"
	kbBasal             = "rice/fertilization/basal"
	kbPanicle           = "rice/fertilization/panicle"
	kbFoliar            = "rice/fertilization/foliar"
	kbDeficiencyPrefix  = "rice/fertilization/deficiency-" // + n / k / zn
	kbSeedlingTillering = "rice/irrigation/seedling-tillering"
	kbDrain             = "rice/irrigation/drain"
	kbRewater           = "rice/irrigation/rewater"
	kbHeading           = "rice/irrigation/heading"
	kbGrainFilling      = "rice/irrigation/grain-filling"
	kbSprayWater        = "rice/irrigation/spray-water"
	kbDisasterPrefix    = "rice/disaster/" // + heat / cold / typhoon / flood / lodging
)

// 数值精度（知识库参数直出时使用；calc 计算结果自带精度）。
const (
	precisionDepth = 0.5
	precisionDays  = calc.PrecisionDays
	precisionCount = 1.0
	precisionGram  = 1.0
	precisionKg    = 0.1
	precisionWater = 0.1
	gramsPerKg     = 1000
)

// rule 是一个一级类的规则：tools 决定 enrich 调哪些工具，decide 在知识加载后给出结论；
// compute（可空）在选药之后补算依赖所选药剂的数值（药量、安全间隔期等）。
type rule struct {
	tools   func(s *state) []string
	decide  func(e *env, s *state) error
	compute func(e *env, s *state) error
}

// rules 是一级类 → 规则（新增一级类只加一项，不改节点）。
var rules = map[model.Category]rule{
	model.CategoryProtect:   {tools: protectTools, decide: decideProtect, compute: computeProtect},
	model.CategoryFertilize: {tools: siteTools, decide: decideFertilize},
	model.CategoryIrrigate:  {tools: siteTools, decide: decideIrrigate},
	model.CategoryDisaster:  {tools: disasterTools, decide: decideDisaster},
}

// siteTools 是施肥 / 灌溉的工具集：生育期、田块档案、农事记录、天气预报。
func siteTools(*state) []string {
	return []string{toolPhenology, toolProfile, toolHistory, toolForecast}
}

func disasterTools(*state) []string {
	return []string{toolPhenology, toolForecast, toolAlert, toolHistory, toolProfile}
}

// env 是规则层的只读依赖。
type env struct {
	snap   *knowledge.Snapshot
	tx     *knowledge.Taxonomy
	calc   *calc.Calculator
	loc    *time.Location
	crop   string
	region string
}

// use 是所选药剂在本农场作物 × 区域下的适用范围。
func (e *env) use(s *state, productID string) calc.Use {
	return calc.Use{ProductID: productID, Crop: e.crop, Target: s.dec.Target, Region: e.region}
}

// refusal 是业务拒答（不是运行错误）：节点据此置 refuse 并直通到 assemble。
type refusal struct {
	reason string
	detail string
}

func (r *refusal) Error() string { return r.reason + ": " + r.detail }

func refuse(reason, format string, args ...any) error {
	return &refusal{reason: reason, detail: fmt.Sprintf(format, args...)}
}

// asRefusal 把规则 / calc 错误归为拒答原因；不是业务错误时返回 false。
func asRefusal(err error) (string, bool) {
	var r *refusal
	switch {
	case errors.As(err, &r):
		return r.reason, true
	case errors.Is(err, knowledge.ErrKBMiss), errors.Is(err, calc.ErrMissingParam),
		errors.Is(err, calc.ErrNoCandidates), errors.Is(err, calc.ErrNotRegistered):
		return RefuseKBMiss, true
	case errors.Is(err, calc.ErrOutOfRange), errors.Is(err, calc.ErrUnitMismatch), errors.Is(err, calc.ErrPHINotApplicable):
		return RefuseDataMissing, true
	}
	return "", false
}

func (e *env) entry(id string) (*knowledge.Entry, error) {
	en, ok := e.snap.Entry(id)
	if !ok || e.snap.Deprecated(id) {
		return nil, refuse(RefuseKBMiss, "entry %s not in knowledge %s", id, e.snap.Ref())
	}
	return en, nil
}

// targetEntry 取防治对象的专属条目（entry.target == target）。
func (e *env) targetEntry(target string) (*knowledge.Entry, error) {
	if target != "" {
		for _, en := range e.snap.Entries() {
			if en.Target == target && !e.snap.Deprecated(en.KBID) {
				return en, nil
			}
		}
	}
	return nil, refuse(RefuseKBMiss, "no entry for target %q", target)
}

// verdict 是一条阈值的判定结果。
type verdict int

const (
	notApplicable verdict = iota // 生育期不符或无观测值
	met
	unmet
)

// judge 判定条目 en 的阈值 id；适用且有观测值时记一条理由（知识库 + 实测引用）。
func (e *env) judge(s *state, en *knowledge.Entry, id string) verdict {
	i := slices.IndexFunc(en.Thresholds, func(t knowledge.Threshold) bool { return t.ID == id })
	if i < 0 {
		return notApplicable
	}
	th := en.Thresholds[i]
	if len(th.Stage) > 0 && !slices.Contains(th.Stage, s.stage) {
		return notApplicable
	}
	var hit bool
	var observed, key string
	if th.DurationDays > 0 {
		vals, k, ok := s.facts.series(th.Metric)
		if !ok {
			return notApplicable
		}
		run := longestRun(vals, th)
		hit, key = run >= th.DurationDays, k
		observed = fmt.Sprintf("连续 %d 天满足，序列 %s", run, joinNums(vals, th.Unit))
	} else {
		v, ok := s.facts.num(th.Metric)
		if !ok {
			return notApplicable
		}
		hit, key = compare(th.Op, v, th.Value), th.Metric
		observed = fmtFact(v) + " " + th.Unit
	}
	s.dec.Evaluated++
	s.dec.Judged = append(s.dec.Judged, judged{KB: en.KBID, Th: th, Fact: key, Met: hit})
	cond := fmt.Sprintf("%s %s %s", th.Op, fmtNum(th.Value), th.Unit)
	if th.DurationDays > 0 {
		cond = fmt.Sprintf("%s 且连续 ≥ %d 天", cond, th.DurationDays)
	}
	status, brief := "未满足", th.Label+"未满足"
	if hit {
		status, brief = "满足", th.Label+"满足"
	}
	text := fmt.Sprintf("%s：实测 %s，条件 %s，%s", th.Label, observed, cond, status)
	if hit && th.Action != "" {
		text += "；" + th.Action
	}
	s.dec.because(reason{Text: text, Brief: brief, KB: []string{en.KBID}, Facts: []string{key}})
	if hit {
		return met
	}
	return unmet
}

// note 记一条只有知识依据（可附实测事实）的理由。
func note(s *state, en *knowledge.Entry, text string, facts ...string) {
	s.dec.because(reason{Text: text, Brief: en.Title, KB: []string{en.KBID}, Facts: facts})
}

func compare(op string, v, th float64) bool {
	switch op {
	case ">=":
		return v >= th
	case ">":
		return v > th
	case "<=":
		return v <= th
	case "<":
		return v < th
	case "==":
		return v == th
	}
	return false
}

// longestRun 是序列中连续满足阈值的最长天数。
func longestRun(vals []float64, th knowledge.Threshold) int {
	best, cur := 0, 0
	for _, v := range vals {
		if compare(th.Op, v, th.Value) {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}
