package reduce

import (
	"math"
	"math/rand/v2"
	"slices"

	"agri-steward/internal/service/quality/stat"
)

// 统计口径（P2 L3，口径表 §统计）：比例类按样本 Wilson，均值类按样本 bootstrap（1000 次、seed 进报告）；
// 切片差值标红前 Holm 校正。Wilson / Holm / 两比例检验在 service/quality/stat（线上 rollup 与日报共用），这里转调。
const (
	// WilsonZ 是 95% 区间的 z 值。
	WilsonZ = stat.WilsonZ
	// DefaultResamples / DefaultSeed 与 compare 一致（P2 §6.1 功能任务 4）。
	DefaultResamples = 1000
	DefaultSeed      = 20260101
	ciLow, ciHigh    = 0.025, 0.975
)

// Wilson 是 k / n 的 95% Wilson 区间；n = 0 返回 [0, 0]。
func Wilson(k, n int) (lo, hi float64) { return stat.Wilson(k, n) }

// BootstrapMean 对 xs 有放回重抽 resamples 次，返回均值的 nearest-rank 2.5% / 97.5% 分位；xs 为空返回 [0, 0]。
// 随机源是固定 seed 的 PCG（同 seed 可复现）。
func BootstrapMean(xs []float64, resamples int, seed uint64) (lo, hi float64) {
	if len(xs) == 0 || resamples < 1 {
		return 0, 0
	}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	means := make([]float64, resamples)
	for b := range means {
		var sum float64
		for range xs {
			sum += xs[r.IntN(len(xs))]
		}
		means[b] = sum / float64(len(xs))
	}
	slices.Sort(means)
	return nearestRank(means, ciLow), nearestRank(means, ciHigh)
}

func nearestRank(sorted []float64, q float64) float64 {
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// Holm 是 Holm-Bonferroni 逐步校正，返回与 ps 同序的是否显著。
func Holm(ps []float64, alpha float64) []bool { return stat.Holm(ps, alpha) }

// TwoProportionP 是两比例差合并方差 z 检验的双侧 p 值。
func TwoProportionP(k1, n1, k2, n2 int) float64 { return stat.TwoProportionP(k1, n1, k2, n2) }

// IntervalDiffP 是两个独立估计（点估计 + 95% 区间）之差的双侧 p 值。
func IntervalDiffP(p1, lo1, hi1, p2, lo2, hi2 float64) float64 {
	return stat.IntervalDiffP(p1, lo1, hi1, p2, lo2, hi2)
}
