package compare

import (
	"math"
	"math/rand/v2"
	"slices"
)

// 配对 bootstrap 的默认参数（P2 §6.1 功能任务 4：1000 次，seed 进报告）。
const (
	DefaultResamples = 1000
	DefaultSeed      = 20260101
	// ciLow / ciHigh 是 95% 区间的两端分位。
	ciLow  = 0.025
	ciHigh = 0.975
)

// Interval 是样本级配对差值（candidate − baseline）的均值与 bootstrap 95% 区间。
type Interval struct {
	Mean, Low, High float64
}

// Significant 报告区间是否不含 0。
func (iv Interval) Significant() bool { return iv.Low > 0 || iv.High < 0 }

// newRand 是固定 seed 的 PCG（同 seed 结果可复现）。
func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }

// bootstrapMeans 对配对差值有放回重抽 resamples 次，返回每次的均值（未排序）。diffs 为空返回 nil。
func bootstrapMeans(diffs []float64, resamples int, seed uint64) []float64 {
	if len(diffs) == 0 || resamples < 1 {
		return nil
	}
	r := newRand(seed)
	means := make([]float64, resamples)
	for b := range means {
		var sum float64
		for range diffs {
			sum += diffs[r.IntN(len(diffs))]
		}
		means[b] = sum / float64(len(diffs))
	}
	return means
}

// PairedBootstrap 求配对差值的均值与 bootstrap 95% 区间（nearest-rank 2.5% / 97.5% 分位）。
func PairedBootstrap(diffs []float64, resamples int, seed uint64) Interval {
	means := bootstrapMeans(diffs, resamples, seed)
	if means == nil {
		return Interval{}
	}
	slices.Sort(means)
	return Interval{Mean: mean(diffs), Low: Percentile(means, ciLow), High: Percentile(means, ciHigh)}
}

// Percentile 是已排序切片的 nearest-rank 分位；空切片为 0。
func Percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
