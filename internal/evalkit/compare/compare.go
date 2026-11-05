package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// Side 是对比的一边：experiment.id、快照与读回的 trial。Done 表示实验 status = DONE（零值 false 即被拒绝，失败关闭）。
type Side struct {
	ID       uint64
	Done     bool
	Snapshot evalkit.Snapshot
	Trials   []evalkit.Trial
}

var (
	// ErrNotDone 表示一边实验没跑完（status 不是 DONE）。
	ErrNotDone = errors.New("compare: experiment is not DONE")
	// ErrNoTrials 表示一边实验没有任何 trial。
	ErrNoTrials = errors.New("compare: experiment has no trials")
	// ErrNoPairs 表示两边 (sample_id, sample_version) 交集为空，没有可配对的样本。
	ErrNoPairs = errors.New("compare: no paired samples")
	// ErrNoMetric 表示没有任何质量指标在配对样本上适用（Samples > 0 的指标数为 0）。
	ErrNoMetric = errors.New("compare: no quality metric applies to the paired samples")
	// ErrReferencePhaseOnly 表示 --reference 只用于阶段级对比。
	ErrReferencePhaseOnly = errors.New("compare: reference is only valid in phase mode")
)

// RequireUsable 要求一边实验 DONE 且至少有一条 trial（compare / aa 共用）。
func RequireUsable(side string, s Side) error {
	switch {
	case !s.Done:
		return fmt.Errorf("%w: %s exp %d", ErrNotDone, side, s.ID)
	case len(s.Trials) == 0:
		return fmt.Errorf("%w: %s exp %d", ErrNoTrials, side, s.ID)
	}
	return nil
}

// Input 是一次对比的输入。Paired 是两边 (sample_id, sample_version) 交集（store.PairedSamples，sample_id@version）。
type Input struct {
	Mode      Mode
	Baseline  Side
	Candidate Side
	// Reference 非 nil 时给出第三列（阶段级：「baseline 配置 + 候选知识快照」），把差异拆成知识侧与 agent 侧。
	Reference *Side
	Paired    []string
	// KnowledgeChanges 是两边 git_sha 之间 knowledge/ 下的变更文件（阶段级报告首行）。
	KnowledgeChanges []string
	Seed             uint64
	Resamples        int
}

// Verdict 是单个指标或整体的对比结论。
type Verdict string

// 结论取值。
const (
	VerdictImproved   Verdict = "改善"
	VerdictNoSig      Verdict = "无显著差异"
	VerdictRegressed  Verdict = "退化"
	VerdictPending    Verdict = "未接入"
	VerdictNoApplying Verdict = "无适用样本"
)

// Conclusion 是单因子模式的门禁结论（L7-a 写 gate_verdict 时复用）；阶段级模式为空。
type Conclusion string

// 单因子结论。
const (
	ConclusionNotAttributable Conclusion = "NOT_ATTRIBUTABLE"
	// ConclusionIncomplete 表示数据不完整，不可放行：任一边有 infra_error / grader_error 的 trial，
	// 或 baseline 有样本未被 candidate 配对（candidate 少跑了样本、或样本版本变了）。
	ConclusionIncomplete Conclusion = "INCOMPLETE"
	ConclusionImproved   Conclusion = "IMPROVED"
	ConclusionNoSig      Conclusion = "NO_SIGNIFICANT_DIFF"
	ConclusionRegressed  Conclusion = "REGRESSED"
)

// MetricResult 是一个质量指标在配对样本上的结果。
type MetricResult struct {
	Name, Title   string
	LowerIsBetter bool
	Pending       string
	// NotGating 为 true 表示只出报告、不进整体结论与放行判定（LLM-judge 指标在 L2 校准达标前，口径表「统计」一节）。
	NotGating bool
	// Samples 是两边都适用的配对样本数。
	Samples                     int
	BaselineMean, CandidateMean float64
	Diff                        Interval
	Verdict                     Verdict
	// Flaky 是任一边 flaky 的配对样本（照样本级值参与，另列清单）。
	Flaky []string
	// Knowledge / Agent 是 --reference 时的拆分：reference − baseline、candidate − reference（三边都适用的样本）。
	Knowledge, Agent *Interval
	ReferenceMean    float64
}

// Excluded 是不在配对交集内的样本（只在一边出现，或两边 version 不同）。
type Excluded struct {
	Key  string
	Side string
}

// Result 是一次对比的结果。
type Result struct {
	Mode                    Mode
	BaselineID, CandidateID uint64
	ReferenceID             uint64
	Baseline, Candidate     evalkit.Snapshot
	Diffs                   []FactorDiff
	Paired                  int
	Excluded                []Excluded
	Metrics                 []MetricResult
	Overall                 Verdict
	Conclusion              Conclusion
	Seed                    uint64
	Resamples               int
	KnowledgeChanges        []string
	// Errors 是各边 infra_error / grader_error 的 trial 数（按 Side 名）。
	Errors map[string][2]int
	// ErrorSamples 是各边有 infra_error / grader_error trial 的样本（按 Side 名，排序）；baseline / candidate 非空即 INCOMPLETE。
	ErrorSamples map[string][]string
}

// Attributable 报告单因子模式下差异因子是否 ≤ 1。
func (r *Result) Attributable() bool { return r.Mode == ModeFactor && len(r.Diffs) <= 1 }

// releasableConclusions 是可放行的单因子结论（白名单：新增结论默认不可放行）。
var releasableConclusions = map[Conclusion]bool{ConclusionImproved: true, ConclusionNoSig: true}

// Releasable 报告结论能否用于门禁放行：只有单因子模式且结论在白名单内（可归因、未退化、数据完整）；阶段级模式恒为 false。
func (r *Result) Releasable() bool {
	return r.Mode == ModeFactor && r.Attributable() && releasableConclusions[r.Conclusion]
}

// Incomplete 报告 baseline 或 candidate 是否有出错 trial。
func (r *Result) Incomplete() bool {
	return len(r.ErrorSamples[sideBaseline]) > 0 || len(r.ErrorSamples[sideCandidate]) > 0
}

// Uncovered 返回 baseline 有而 candidate 未配对的样本键（排序；含 candidate 换了版本的样本）。非空即 INCOMPLETE：
// 单因子模式只在交集上配对，candidate 少跑的样本若不拦，结论会建立在 baseline 的子集上（P2-L0 第二轮审查修复）。
func (r *Result) Uncovered() []string {
	var out []string
	for _, e := range r.Excluded {
		if e.Side == sideBaseline {
			out = append(out, e.Key)
		}
	}
	return out
}

func (r *Result) uncovered() bool { return len(r.Uncovered()) > 0 }

// Record 返回写入候选实验 factor_diff 的 JSON（[{name, baseline, candidate}]）；阶段级模式 ok 为 false（不写任何门禁字段）。
func (r *Result) Record() (factorDiff json.RawMessage, ok bool, err error) {
	if r.Mode != ModeFactor {
		return nil, false, nil
	}
	diffs := r.Diffs
	if diffs == nil {
		diffs = []FactorDiff{}
	}
	b, err := json.Marshal(diffs)
	if err != nil {
		return nil, false, fmt.Errorf("compare: marshal factor_diff: %w", err)
	}
	return b, true, nil
}

// 报告里的边名。
const (
	sideBaseline  = "baseline"
	sideCandidate = "candidate"
	sideReference = "reference"
)

// Run 校验对比前提并计算全部质量指标（失败关闭）：两边须 DONE 且有 trial、快照完整且可比、配对交集非空、
// 至少一个指标真正参与；--reference 一边同样须 DONE 且与 baseline 满足阶段级前提。不满足返回哨兵错误。
func Run(in Input) (*Result, error) {
	if err := checkInput(in); err != nil {
		return nil, err
	}
	diffs, err := Check(Factors, in.Mode, in.Baseline.Snapshot, in.Candidate.Snapshot)
	if err != nil {
		return nil, err
	}
	if in.Resamples < 1 {
		in.Resamples = DefaultResamples
	}
	if in.Mode == ModeFactor {
		// 单因子模式：git_sha 以外的差异全部列出（含 Pinned 已校验一致）。
		diffs = Diff(Factors, in.Baseline.Snapshot, in.Candidate.Snapshot)
	}
	r := &Result{Mode: in.Mode, BaselineID: in.Baseline.ID, CandidateID: in.Candidate.ID, Baseline: in.Baseline.Snapshot,
		Candidate: in.Candidate.Snapshot, Diffs: diffs, Paired: len(in.Paired), Seed: in.Seed, Resamples: in.Resamples,
		KnowledgeChanges: in.KnowledgeChanges, Errors: map[string][2]int{}, ErrorSamples: map[string][]string{}}
	if in.Reference != nil {
		r.ReferenceID = in.Reference.ID
	}
	paired := make(map[string]bool, len(in.Paired))
	for _, k := range in.Paired {
		paired[k] = true
	}
	r.Excluded = append(excluded(sideBaseline, in.Baseline.Trials, paired), excluded(sideCandidate, in.Candidate.Trials, paired)...)
	sort.SliceStable(r.Excluded, func(i, j int) bool { return r.Excluded[i].Key < r.Excluded[j].Key })
	sides := []struct {
		name string
		s    *Side
	}{{sideBaseline, &in.Baseline}, {sideCandidate, &in.Candidate}, {sideReference, in.Reference}}
	for _, sd := range sides {
		if sd.s != nil {
			r.Errors[sd.name] = errorCounts(sd.s.Trials)
			r.ErrorSamples[sd.name] = errorSamples(sd.s.Trials)
		}
	}
	applied := 0
	for i, m := range reduce.QualityMetrics {
		mr := metricResult(m, in, paired, in.Seed+uint64(i))
		if mr.Samples > 0 && !mr.NotGating {
			applied++
		}
		r.Metrics = append(r.Metrics, mr)
	}
	if applied == 0 {
		return nil, fmt.Errorf("%w: %d paired samples", ErrNoMetric, r.Paired)
	}
	r.Overall = overall(r.Metrics)
	if in.Mode == ModeFactor {
		r.Conclusion = conclusion(r)
	}
	return r, nil
}

// checkInput 校验各边可用、reference 只在阶段级且与 baseline 满足阶段级前提、配对交集非空。
func checkInput(in Input) error {
	if err := errors.Join(RequireUsable(sideBaseline, in.Baseline), RequireUsable(sideCandidate, in.Candidate)); err != nil {
		return err
	}
	if in.Reference != nil {
		if in.Mode != ModePhase {
			return ErrReferencePhaseOnly
		}
		if err := RequireUsable(sideReference, *in.Reference); err != nil {
			return err
		}
		if _, err := Check(Factors, ModePhase, in.Baseline.Snapshot, in.Reference.Snapshot); err != nil {
			return fmt.Errorf("reference exp %d: %w", in.Reference.ID, err)
		}
	}
	if len(in.Paired) == 0 {
		return fmt.Errorf("%w: baseline %d / candidate %d", ErrNoPairs, in.Baseline.ID, in.Candidate.ID)
	}
	return nil
}

// errorSamples 列出有 infra_error / grader_error trial 的样本键（排序去重；含交集外样本：只要有出错 trial 即不可放行）。
func errorSamples(trials []evalkit.Trial) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range trials {
		st := t.Status()
		if k := reduce.SampleKey(t); !seen[k] && (st == evalkit.StatusInfraError || st == evalkit.StatusGraderError) {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// excluded 列出 side 中不在交集内的样本键。
func excluded(side string, trials []evalkit.Trial, paired map[string]bool) []Excluded {
	seen := map[string]bool{}
	var out []Excluded
	for _, t := range trials {
		k := reduce.SampleKey(t)
		if !paired[k] && !seen[k] {
			seen[k] = true
			out = append(out, Excluded{Key: k, Side: side})
		}
	}
	return out
}

func errorCounts(trials []evalkit.Trial) [2]int {
	var c [2]int
	for _, t := range trials {
		switch t.Status() {
		case evalkit.StatusInfraError:
			c[0]++
		case evalkit.StatusGraderError:
			c[1]++
		}
	}
	return c
}

// blocker 是一条不可放行规则：命中即给出该结论（按注册顺序取第一条命中）。
type blocker struct {
	conclusion Conclusion
	hit        func(*Result) bool
}

// blockers 是单因子结论的阻断规则注册表（顺序即优先级）：多因子 > 退化 > 数据不完整（出错 trial、baseline 样本未覆盖）。
var blockers = []blocker{
	{ConclusionNotAttributable, func(r *Result) bool { return !r.Attributable() }},
	{ConclusionRegressed, func(r *Result) bool { return r.Overall == VerdictRegressed }},
	{ConclusionIncomplete, (*Result).Incomplete},
	{ConclusionIncomplete, (*Result).uncovered},
}

// overallConclusion 是未被阻断时整体结论到单因子结论的映射。
var overallConclusion = map[Verdict]Conclusion{VerdictImproved: ConclusionImproved, VerdictNoSig: ConclusionNoSig}

func conclusion(r *Result) Conclusion {
	for _, b := range blockers {
		if b.hit(r) {
			return b.conclusion
		}
	}
	if c, ok := overallConclusion[r.Overall]; ok {
		return c
	}
	return ConclusionIncomplete
}

// overall：只看进门禁的指标（NotGating 只出报告）。任一退化即退化；否则任一改善即改善；否则无显著差异。
func overall(ms []MetricResult) Verdict {
	v := VerdictNoSig
	for _, m := range ms {
		if m.NotGating {
			continue
		}
		switch m.Verdict {
		case VerdictRegressed:
			return VerdictRegressed
		case VerdictImproved:
			v = VerdictImproved
		}
	}
	return v
}
