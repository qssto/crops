// Package stat 是可信度指标的比例区间与显著性检验（P2 L3，口径表 §统计）：离线 evalkit/reduce 与线上 rollup / 日报共用一份，
// 生产代码不 import evalkit。标准库手写，不引第三方依赖。
package stat

import (
	"math"
	"sort"
)

// WilsonZ 是 95% 区间的 z 值。
const WilsonZ = 1.96

// Wilson 是 k / n 的 95% Wilson 区间；n = 0 返回 [0, 0]。
func Wilson(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	p, nf, z2 := float64(k)/float64(n), float64(n), WilsonZ*WilsonZ
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	margin := WilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	return max(0, center-margin), min(1, center+margin)
}

// Holm 是 Holm-Bonferroni 逐步校正：按 p 升序，第 i 个（从 0 起）与 alpha / (m - i) 比，首个不显著即停；
// 返回与 ps 同序的是否显著。
func Holm(ps []float64, alpha float64) []bool {
	idx := make([]int, len(ps))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return ps[idx[a]] < ps[idx[b]] })
	out := make([]bool, len(ps))
	for rank, i := range idx {
		if ps[i] > alpha/float64(len(ps)-rank) {
			break
		}
		out[i] = true
	}
	return out
}

// TwoProportionP 是两比例差（k1/n1 vs k2/n2）合并方差 z 检验的双侧 p 值；任一 n 为 0 或方差为 0 返回 1。
func TwoProportionP(k1, n1, k2, n2 int) float64 {
	if n1 <= 0 || n2 <= 0 {
		return 1
	}
	p1, p2 := float64(k1)/float64(n1), float64(k2)/float64(n2)
	pool := float64(k1+k2) / float64(n1+n2)
	se := math.Sqrt(pool * (1 - pool) * (1/float64(n1) + 1/float64(n2)))
	return zP(p1-p2, se)
}

// IntervalDiffP 是两个独立估计（点估计 + 95% 区间）之差的双侧 p 值：标准误按区间半宽 / 1.96 近似（bootstrap 指标用）。
func IntervalDiffP(p1, lo1, hi1, p2, lo2, hi2 float64) float64 {
	se1, se2 := (hi1-lo1)/(2*WilsonZ), (hi2-lo2)/(2*WilsonZ)
	return zP(p1-p2, math.Sqrt(se1*se1+se2*se2))
}

// zP 是正态近似双侧 p 值；差为 0 或标准误为 0 返回 1（无可检验的差异）。
func zP(diff, se float64) float64 {
	if diff == 0 || se == 0 {
		return 1
	}
	return math.Erfc(math.Abs(diff/se) / math.Sqrt2)
}
