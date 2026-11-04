package review

import (
	"slices"

	"agri-steward/internal/evalkit/compare"
)

// 量表（1-3）。
const (
	minScore   = 1
	maxScore   = 3
	categories = maxScore - minScore + 1
	// DefaultResamples 是 Kappa bootstrap 次数。
	DefaultResamples = compare.DefaultResamples
	ciLow, ciHigh    = 0.025, 0.975
	// MaxUndefinedRatio 是 bootstrap 无定义重抽的最高占比（L2 评审 P1-3）：超过即区间不可用（只剩有定义的重抽会高估一致性）。
	MaxUndefinedRatio = 0.01
)

// Pair 是一对评分（judge-专家或主标-双标），取值 1-3。
type Pair struct{ A, B int }

// Matrix 是 3×3 混淆矩阵：M[a-1][b-1] 为 (A=a, B=b) 的次数；行为 A（judge / 主标），列为 B（专家 / 双标）。
type Matrix [categories][categories]int

// NewMatrix 由评分对建矩阵（越界的对忽略）。
func NewMatrix(pairs []Pair) Matrix {
	var m Matrix
	for _, p := range pairs {
		if inRange(p.A) && inRange(p.B) {
			m[p.A-minScore][p.B-minScore]++
		}
	}
	return m
}

func inRange(s int) bool { return s >= minScore && s <= maxScore }

// Total 是矩阵总次数。
func (m Matrix) Total() int {
	n := 0
	for _, row := range m {
		for _, c := range row {
			n += c
		}
	}
	return n
}

// Pairs 把矩阵展开为评分对（bootstrap 用）。
func (m Matrix) Pairs() []Pair {
	var out []Pair
	for i, row := range m {
		for j, c := range row {
			for range c {
				out = append(out, Pair{A: i + minScore, B: j + minScore})
			}
		}
	}
	return out
}

// QuadraticKappa 是二次加权 Kappa：κ = 1 − Σ w·O / Σ w·E，w_ij = (i−j)² / (k−1)²，E_ij = 行和_i · 列和_j / N。
// 没有数据或期望不一致为 0（两边都只用同一个分数，κ 无定义）时 ok = false。
func (m Matrix) QuadraticKappa() (kappa float64, ok bool) {
	n := float64(m.Total())
	if n == 0 {
		return 0, false
	}
	var rows, cols [categories]float64
	for i, row := range m {
		for j, c := range row {
			rows[i] += float64(c)
			cols[j] += float64(c)
		}
	}
	var obs, exp float64
	for i := range categories {
		for j := range categories {
			w := float64((i-j)*(i-j)) / float64((categories-1)*(categories-1))
			obs += w * float64(m[i][j])
			exp += w * rows[i] * cols[j] / n
		}
	}
	if exp == 0 {
		return 0, false
	}
	return 1 - obs/exp, true
}

// Interval 是 Kappa 点估计与 bootstrap 95% 区间：OK 为点估计有定义；N 是评分对数；Undefined 是 κ 无定义的重抽次数，
// 占比超过 MaxUndefinedRatio（或没有重抽）时 Usable = false，区间不得用于门禁。
type Interval struct {
	Kappa, Low, High float64
	OK               bool
	N                int
	Resamples        int
	Undefined        int
	Usable           bool
	Seed             uint64
}

// BootstrapKappa 对评分对有放回重抽 resamples 次求 κ 的 nearest-rank 2.5% / 97.5% 分位；无定义的重抽不进分位但计数，
// 占比超过 MaxUndefinedRatio 时区间不可用。
func BootstrapKappa(pairs []Pair, resamples int, seed uint64) Interval {
	iv := Interval{N: len(pairs), Resamples: resamples, Seed: seed}
	iv.Kappa, iv.OK = NewMatrix(pairs).QuadraticKappa()
	if !iv.OK || resamples < 1 {
		return iv
	}
	r := newRand(seed)
	ks := make([]float64, 0, resamples)
	buf := make([]Pair, len(pairs))
	for range resamples {
		for i := range buf {
			buf[i] = pairs[r.IntN(len(pairs))]
		}
		k, ok := NewMatrix(buf).QuadraticKappa()
		if !ok {
			iv.Undefined++
			continue
		}
		ks = append(ks, k)
	}
	slices.Sort(ks)
	iv.Low, iv.High = compare.Percentile(ks, ciLow), compare.Percentile(ks, ciHigh)
	iv.Usable = len(ks) > 0 && float64(iv.Undefined) <= MaxUndefinedRatio*float64(resamples)
	return iv
}
