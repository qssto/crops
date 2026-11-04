package review

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/store"
)

// 校准抽样参数（路线 §6.3 功能任务 5-6）。
const (
	DefaultCalibrationTotal = 100
	DefaultPerStratum       = 20
	DefaultSecondRate       = 0.25
	strataUnknown           = "UNKNOWN"
)

// Candidate 是某维度可进校准 / 复核的 trial：judge 已打分（含 UNKNOWN），不是 holdout。
type Candidate struct {
	SampleID   string
	TrialIndex int
	Suite      string
	JudgeScore *int // nil 为 UNKNOWN
}

func (c Candidate) stratum() string {
	if c.JudgeScore == nil {
		return strataUnknown
	}
	return strconv.Itoa(*c.JudgeScore)
}

// Candidates 取 trials 中 dim 维度有 judge 结果（非 grader_error）的 trial；holdout trial 不进专家任务（内容仅负责人可读）。
func Candidates(trials []evalkit.Trial, dim string) []Candidate {
	var out []Candidate
	for _, t := range publicTrials(trials) {
		if s, ok := judgeScore(t, dim); ok {
			out = append(out, Candidate{SampleID: t.SampleID, TrialIndex: t.TrialIndex, Suite: t.Suite, JudgeScore: s})
		}
	}
	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(a.SampleID, b.SampleID), cmp.Compare(a.TrialIndex, b.TrialIndex))
	})
	return out
}

func publicTrials(trials []evalkit.Trial) []evalkit.Trial {
	out := make([]evalkit.Trial, 0, len(trials))
	for _, t := range trials {
		if !t.IsHoldout() {
			out = append(out, t)
		}
	}
	return out
}

// SampleOptions 是校准抽样参数；零值取默认。
type SampleOptions struct {
	Total      int
	PerStratum int
	SecondRate float64
	Seed       uint64
}

func (o SampleOptions) withDefaults() SampleOptions {
	if o.Total == 0 {
		o.Total = DefaultCalibrationTotal
	}
	if o.PerStratum == 0 {
		o.PerStratum = DefaultPerStratum
	}
	if o.SecondRate == 0 {
		o.SecondRate = DefaultSecondRate
	}
	if o.Seed == 0 {
		o.Seed = DefaultSeed
	}
	return o
}

// Plan 是一次抽样结果：任务（主标 + 双标）、各层抽中数与不足说明。
type Plan struct {
	Tasks   []store.ReviewTask
	Strata  map[string]int
	Primary int
	Second  int
	Seed    uint64
	Short   []string
}

// PlanCalibration 生成 dim 维度的 CALIBRATION 任务。专家打分前没有专家分数，抽样按 judge 分数（1/2/3/UNKNOWN）分层作代理：
// 先每个 1/2/3 层抽 PerStratum 个，再轮流从各层补到 Total；层内按套件轮转（capability / 陷阱组与 regression 交替，
// 不只抽通过率高的 regression）。专家打完后报告再按专家分数核对每层 ≥ PerStratum，不足在报告中列出待补抽。
// 随机 SecondRate（向上取整）的主标任务另生成 rater_no=2 的双标任务。
func PlanCalibration(expID uint64, dim, version string, cands []Candidate, opt SampleOptions) Plan {
	opt = opt.withDefaults()
	r := newRand(opt.Seed)
	strata := map[string][]Candidate{}
	for _, c := range cands {
		strata[c.stratum()] = append(strata[c.stratum()], c)
	}
	order := []string{"1", "2", "3", strataUnknown}
	queues := map[string][]Candidate{}
	for _, k := range order {
		queues[k] = interleaveSuites(strata[k], r)
	}
	p := Plan{Strata: map[string]int{}, Seed: opt.Seed}
	var picked []Candidate
	take := func(k string) bool {
		if len(queues[k]) == 0 {
			return false
		}
		picked = append(picked, queues[k][0])
		queues[k] = queues[k][1:]
		p.Strata[k]++
		return true
	}
	for _, k := range order[:3] {
		for p.Strata[k] < opt.PerStratum {
			if !take(k) {
				break
			}
		}
		if p.Strata[k] < opt.PerStratum {
			p.Short = append(p.Short, fmt.Sprintf("judge=%s: %d < %d", k, p.Strata[k], opt.PerStratum))
		}
	}
	for progress := true; len(picked) < opt.Total && progress; {
		progress = false
		for _, k := range order {
			if len(picked) < opt.Total && take(k) {
				progress = true
			}
		}
	}
	if len(picked) < opt.Total {
		p.Short = append(p.Short, fmt.Sprintf("total: %d < %d", len(picked), opt.Total))
	}
	for _, c := range picked {
		p.Tasks = append(p.Tasks, trialTask(store.TaskCalibration, expID, dim, version, c, store.RaterPrimary))
	}
	p.Primary = len(picked)
	n2 := int(math.Ceil(float64(len(picked)) * opt.SecondRate))
	for _, i := range r.Perm(len(picked))[:n2] {
		p.Tasks = append(p.Tasks, trialTask(store.TaskCalibration, expID, dim, version, picked[i], store.RaterSecond))
	}
	p.Second = n2
	return p
}

// interleaveSuites 按套件分组、组内打乱，再按套件名顺序轮转取出。
func interleaveSuites(cs []Candidate, r interface{ Perm(int) []int }) []Candidate {
	bySuite := map[string][]Candidate{}
	for _, c := range cs {
		bySuite[c.Suite] = append(bySuite[c.Suite], c)
	}
	suites := make([]string, 0, len(bySuite))
	for s := range bySuite {
		suites = append(suites, s)
	}
	slices.Sort(suites)
	for _, s := range suites {
		g := bySuite[s]
		shuffled := make([]Candidate, len(g))
		for i, j := range r.Perm(len(g)) {
			shuffled[i] = g[j]
		}
		bySuite[s] = shuffled
	}
	out := make([]Candidate, 0, len(cs))
	for len(out) < len(cs) {
		for _, s := range suites {
			if g := bySuite[s]; len(g) > 0 {
				out = append(out, g[0])
				bySuite[s] = g[1:]
			}
		}
	}
	return out
}

func trialTask(taskType string, expID uint64, dim, version string, c Candidate, rater int) store.ReviewTask {
	idx := c.TrialIndex
	return store.ReviewTask{TaskType: taskType, ExperimentID: &expID, SampleID: c.SampleID, TrialIndex: &idx,
		Dimension: dim, JudgeVersion: version, JudgeScore: c.JudgeScore, RaterNo: rater}
}
