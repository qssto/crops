// Package review 是人工队列与 judge 校准统计（P2 L2，路线 §6.3 功能任务 5-8、10）：
// 四类入队规则（注册表）、校准分层抽样与双标、二次加权 Kappa + bootstrap 区间、混淆矩阵、月度复核抽样。
// 只产出 store.ReviewTask 与统计结果，落库经 store.Reviews。
package review

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/store"
)

// DefaultSeed 是 RANDOM_10 抽检与校准抽样的默认 seed（进报告）。
const DefaultSeed = 20261101

// randomRate 是通过 trial 的随机抽检比例。
const randomRate = 0.10

// judgePassScore 是 judge 结论按「通过」看的最低分（2、3 分为通过，1 分为不通过），CONFLICT 用。
const judgePassScore = 2

// QueueInput 是一次实验的入队输入。
type QueueInput struct {
	ExperimentID uint64
	Trials       []evalkit.Trial
	// JudgeVersions 是实验快照的 judge_versions（维度 → 版本），写 review_task.judge_version。
	JudgeVersions map[string]string
	Seed          uint64
}

// Rule 是人工队列的一类入口。Match 返回该规则命中的任务（task_type = HUMAN_QUEUE）。
type Rule interface {
	Reason() string
	Match(in QueueInput) []store.ReviewTask
}

// Rules 是入口注册表，按优先级排列：同一 (trial, 维度) 被多条规则命中时只派一单（ref_key 不含 queue_reason），
// 取排在前面的规则。CONFLICT 在最前，保证与代码打分器冲突的 trial 100% 以 CONFLICT 入队（验收 3）。
var Rules = []Rule{ConflictRule{Pairs: DefaultConflictPairs}, UnknownRule{}, FlakyRule{}, Random10Rule{}}

// Queue 跑全部规则并按 ref_key 去重（保留优先级高的）；返回任务与各入口计数。holdout trial 不进人工队列
// （专家任务会展示 trial 内容，holdout 内容仅负责人可读）。
func Queue(in QueueInput, rules []Rule) ([]store.ReviewTask, map[string]int) {
	in.Trials = publicTrials(in.Trials)
	seen := map[string]bool{}
	var out []store.ReviewTask
	counts := map[string]int{}
	for _, r := range rules {
		for _, t := range r.Match(in) {
			k := t.Key()
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, t)
			counts[r.Reason()]++
		}
	}
	return out, counts
}

func humanTask(in QueueInput, t evalkit.Trial, reason, dim string, judgeScore *int) store.ReviewTask {
	exp, idx := in.ExperimentID, t.TrialIndex
	return store.ReviewTask{TaskType: store.TaskHumanQueue, QueueReason: reason, ExperimentID: &exp, SampleID: t.SampleID,
		TrialIndex: &idx, Dimension: dim, JudgeVersion: in.JudgeVersions[dim], JudgeScore: judgeScore, RaterNo: store.RaterPrimary}
}

// judgeScore 取 trial 在 judge 维度的有效分数：没有该维度或 grader_error 返回 ok=false；UNKNOWN 与门禁未开时的
// judge 出错（AdvisoryError，按 UNKNOWN 转人工，L2 评审裁决）返回 (nil, true)。
func judgeScore(t evalkit.Trial, dim string) (*int, bool) {
	sc, ok := t.ScoreOf(dim)
	if !ok || sc.Error != "" {
		return nil, false
	}
	if sc.Unknown || sc.AdvisoryError != "" {
		return nil, true
	}
	v := int(math.Round(sc.Value))
	return &v, true
}

// UnknownRule：judge 返回 UNKNOWN 的 (trial, 维度) 入队。
type UnknownRule struct{}

func (UnknownRule) Reason() string { return store.ReasonUnknown }

func (UnknownRule) Match(in QueueInput) []store.ReviewTask {
	var out []store.ReviewTask
	for _, t := range in.Trials {
		for _, d := range judge.Dimensions {
			if s, ok := judgeScore(t, d); ok && s == nil {
				out = append(out, humanTask(in, t, store.ReasonUnknown, d, nil))
			}
		}
	}
	return out
}

// ConflictPair 是一对可比的 judge 维度与代码打分器。
type ConflictPair struct {
	Dimension string
	Grader    string
}

// DefaultConflictPairs：fact_consistency ↔ citation_valid（验收 3 的最低要求）、conclusion_match ↔ outcome_match。
var DefaultConflictPairs = []ConflictPair{
	{Dimension: judge.DimFactConsistency, Grader: "citation_valid"},
	{Dimension: judge.DimConclusionMatch, Grader: "outcome_match"},
}

// ConflictRule：judge 结论（≥ 2 分为通过）与代码打分器 Pass 不一致的 (trial, 维度) 入队。
// 代码打分器出错、未适用（NotApplicable）或 judge UNKNOWN / 出错时不比。
type ConflictRule struct{ Pairs []ConflictPair }

func (ConflictRule) Reason() string { return store.ReasonConflict }

func (r ConflictRule) Match(in QueueInput) []store.ReviewTask {
	var out []store.ReviewTask
	for _, t := range in.Trials {
		for _, p := range r.Pairs {
			js, ok := judgeScore(t, p.Dimension)
			code, found := t.ScoreOf(p.Grader)
			if !ok || js == nil || !found || code.Error != "" || code.NotApplicable {
				continue
			}
			if (*js >= judgePassScore) != code.Pass {
				out = append(out, humanTask(in, t, store.ReasonConflict, p.Dimension, js))
			}
		}
	}
	return out
}

// FlakyRule：红线结果不一致的样本（reduce.Flaky），其全部 trial 整体入队（维度为空）。
type FlakyRule struct{}

func (FlakyRule) Reason() string { return store.ReasonFlaky }

func (FlakyRule) Match(in QueueInput) []store.ReviewTask {
	flaky := map[string]bool{}
	for _, f := range reduce.Reduce(in.Trials).Flaky {
		flaky[f.Sample.ID] = true
	}
	var out []store.ReviewTask
	for _, t := range in.Trials {
		if flaky[t.SampleID] {
			out = append(out, humanTask(in, t, store.ReasonFlaky, "", nil))
		}
	}
	return out
}

// Random10Rule：通过的 trial 按 seed 随机抽 10%（向上取整），整体入队（维度为空）。
// 先按 (sample_id, trial_index) 排序再抽，同 seed 结果可复现。
type Random10Rule struct{}

func (Random10Rule) Reason() string { return store.ReasonRandom10 }

func (Random10Rule) Match(in QueueInput) []store.ReviewTask {
	var pass []evalkit.Trial
	for _, t := range in.Trials {
		if t.Status() == evalkit.StatusPass {
			pass = append(pass, t)
		}
	}
	sortTrials(pass)
	n := int(math.Ceil(float64(len(pass)) * randomRate))
	var out []store.ReviewTask
	for _, i := range newRand(in.Seed).Perm(len(pass))[:n] {
		out = append(out, humanTask(in, pass[i], store.ReasonRandom10, "", nil))
	}
	return out
}

func sortTrials(ts []evalkit.Trial) {
	slices.SortFunc(ts, func(a, b evalkit.Trial) int {
		return cmp.Or(cmp.Compare(a.SampleID, b.SampleID), cmp.Compare(a.TrialIndex, b.TrialIndex))
	})
}

// newRand 是固定 seed 的 PCG（与 compare 同构造，同 seed 可复现）。
func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }
