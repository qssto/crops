package compare

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// aaQuantile 是 A/A 阈值取的分位（两两差值分布的 95% 分位，P2 §6.1 功能任务 6）。
const aaQuantile = 0.95

var (
	// ErrAANotSameSnapshot 表示 A/A 的几次运行快照不一致（因子、n、suites 或 samples_hash 不同）。
	ErrAANotSameSnapshot = errors.New("compare: A/A runs must share one snapshot")
	// ErrAADuplicate 表示 A/A 同一实验出现多次（自己和自己配对差值恒为 0，会压低阈值）。
	ErrAADuplicate = errors.New("compare: A/A experiments must be distinct")
	// ErrAATooFew 表示 A/A 不足两次运行。
	ErrAATooFew = errors.New("compare: A/A needs >= 2 runs")
	// ErrAAErrors 表示 A/A 运行里有 infra_error / grader_error trial（红线按失败计会放大噪声、抬高阈值）。
	ErrAAErrors = errors.New("compare: A/A run has error trials")
)

// MinAARuns 是两两配对所需的最少实验数（cmd/eval aa 的参数校验共用）。
const MinAARuns = 2

// aaSame 返回 other 与 first 不一致的因子与运行参数名（A/A 要求全部一致，含 n / suites / samples_hash）。
func aaSame(first, other evalkit.Snapshot) []string {
	var diff []string
	for _, f := range Factors {
		if f.Value(first) != f.Value(other) {
			diff = append(diff, f.Name)
		}
	}
	for _, p := range RunParams {
		if p.Value(first) != p.Value(other) {
			diff = append(diff, p.Name)
		}
	}
	return diff
}

// checkAARuns 校验 A/A 输入（失败关闭）：≥ 2 次、id 不重复、都 DONE 且有 trial、快照完整、无出错 trial、
// 因子与运行参数（n / suites / samples_hash）全部一致。
func checkAARuns(runs []Side) error {
	if len(runs) < MinAARuns {
		return fmt.Errorf("%w, got %d", ErrAATooFew, len(runs))
	}
	seen := map[uint64]bool{}
	for _, r := range runs {
		if seen[r.ID] {
			return fmt.Errorf("%w: exp %d", ErrAADuplicate, r.ID)
		}
		seen[r.ID] = true
		side := "aa exp " + strconv.FormatUint(r.ID, 10)
		if err := errors.Join(RequireUsable(side, r), RequireComplete(side, r.Snapshot)); err != nil {
			return err
		}
		if n := len(errorSamples(r.Trials)); n > 0 {
			return fmt.Errorf("%w: exp %d, %d samples", ErrAAErrors, r.ID, n)
		}
		if d := aaSame(runs[0].Snapshot, r.Snapshot); len(d) > 0 {
			return fmt.Errorf("%w: exp %d vs %d differ in %s", ErrAANotSameSnapshot, runs[0].ID, r.ID, strings.Join(d, ","))
		}
	}
	return nil
}

// PairDiff 是 A/A 中一对实验在某指标上的样本级配对均值差（后者 − 前者）。
type PairDiff struct {
	Pair string  `yaml:"pair"`
	Mean float64 `yaml:"mean"`
}

// MetricThreshold 是 thresholds.yaml 中一个指标的阈值下限；judge 指标 Threshold 为 nil（yaml null）。
type MetricThreshold struct {
	Name          string     `yaml:"name"`
	Title         string     `yaml:"title"`
	LowerIsBetter bool       `yaml:"lower_is_better"`
	Threshold     *float64   `yaml:"threshold"`
	PairwiseDiffs []PairDiff `yaml:"pairwise_diffs,omitempty"`
	Note          string     `yaml:"note,omitempty"`
}

// Thresholds 是 eval/gates/thresholds.yaml（草稿：signed_by 留空，Owner 以 PR 签字合入）。
type Thresholds struct {
	GeneratedAt time.Time `yaml:"generated_at"`
	Experiments []string  `yaml:"experiments"`
	Suites      []string  `yaml:"suites"`
	// Tags 是样本筛选条件（运行参数，A/A 各次须一致；空表即不按 tag 筛选）。
	Tags        []string          `yaml:"tags"`
	N           int               `yaml:"n"`
	SamplesHash string            `yaml:"samples_hash"`
	Quantile    float64           `yaml:"quantile"`
	Seed        uint64            `yaml:"bootstrap_seed"`
	Resamples   int               `yaml:"bootstrap_resamples"`
	SignedBy    string            `yaml:"signed_by"`
	Notes       []string          `yaml:"notes"`
	Metrics     []MetricThreshold `yaml:"metrics"`
}

// AA 由同一快照的多次运行求每个质量指标的噪声阈值：每对实验在共同适用样本上做样本级配对 bootstrap，
// 汇总全部重抽均值差的绝对值取 95% 分位（nearest-rank）。
func AA(runs []Side, seed uint64, resamples int, now time.Time) (*Thresholds, error) {
	if err := checkAARuns(runs); err != nil {
		return nil, err
	}
	if resamples < 1 {
		resamples = DefaultResamples
	}
	first := runs[0].Snapshot
	t := &Thresholds{GeneratedAt: now.UTC(), Suites: first.Suites, Tags: first.Tags, N: first.N, SamplesHash: first.SamplesHash,
		Quantile: aaQuantile, Seed: seed, Resamples: resamples}
	for _, r := range runs {
		t.Experiments = append(t.Experiments, strconv.FormatUint(r.ID, 10))
	}
	for i, m := range reduce.QualityMetrics {
		t.Metrics = append(t.Metrics, aaMetric(m, runs, seed+uint64(i), resamples))
	}
	t.Notes = aaNotes(first.Model, t.Metrics)
	return t, nil
}

func aaMetric(m reduce.QualityMetric, runs []Side, seed uint64, resamples int) MetricThreshold {
	mt := MetricThreshold{Name: m.Name(), Title: m.Title(), LowerIsBetter: m.LowerIsBetter()}
	if p := m.Pending(); p != "" {
		mt.Note = p + "：依赖 LLM-judge，待 L2 接入后重测"
		return mt
	}
	all := map[string]bool{}
	idx := make([]map[string]reduce.SampleValue, len(runs))
	for i, r := range runs {
		idx[i] = map[string]reduce.SampleValue{}
		for _, v := range reduce.SampleValues(m, r.Trials) {
			idx[i][v.Key] = v
			all[v.Key] = true
		}
	}
	var pooled []float64
	for i := range runs {
		for j := i + 1; j < len(runs); j++ {
			keys := commonKeys(idx[i], idx[j])
			if len(keys) == 0 {
				continue
			}
			diffs := subtract(values(idx[j], keys), values(idx[i], keys))
			mt.PairwiseDiffs = append(mt.PairwiseDiffs, PairDiff{Pair: fmt.Sprintf("%d-%d", runs[i].ID, runs[j].ID), Mean: mean(diffs)})
			for _, d := range bootstrapMeans(diffs, resamples, seed+uint64(i*len(runs)+j)) {
				pooled = append(pooled, math.Abs(d))
			}
		}
	}
	if len(pooled) == 0 {
		mt.Note = "无适用样本"
		return mt
	}
	slices.Sort(pooled)
	v := Percentile(pooled, aaQuantile)
	mt.Threshold = &v
	return mt
}

func aaNotes(model string, ms []MetricThreshold) []string {
	notes := []string{"草稿：阈值是退化判定的下限（A/A 两两差值 95% 分位），signed_by 留空待 Owner 以 PR 签字合入（待签字）。"}
	if model == evalkit.ModelStub {
		notes = append(notes, "模型为桩（stub）：输出确定，A/A 差值为 0，阈值 0 只说明工具链可用；真实模型接入后须重测。")
	}
	var pending []string
	for _, m := range ms {
		if m.Threshold == nil && m.Note != "" {
			pending = append(pending, m.Name)
		}
	}
	if len(pending) > 0 {
		notes = append(notes, "threshold 为 null 的指标："+strings.Join(pending, "、")+"。")
	}
	return notes
}
