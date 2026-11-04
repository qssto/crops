package review

import (
	"math"
	"testing"
)

// 手算小例：M = [[2,1,0],[0,2,1],[0,0,1]]，N=7，行和 3/3/1、列和 2/3/2，w = (i−j)²/4。
// Σ w·O = 1/4 + 1/4 = 0.5；Σ w·E = (9 + 24 + 6 + 6 + 8 + 3)/28 = 2；κ = 1 − 0.5/2 = 0.75。
func TestQuadraticKappa_KnownAnswers(t *testing.T) {
	tests := []struct {
		name   string
		m      Matrix
		want   float64
		wantOK bool
	}{
		{name: "hand computed", m: Matrix{{2, 1, 0}, {0, 2, 1}, {0, 0, 1}}, want: 0.75, wantOK: true},
		{name: "perfect", m: Matrix{{5, 0, 0}, {0, 3, 0}, {0, 0, 2}}, want: 1, wantOK: true},
		// 两对完全相反（1↔3）：Σ w·O = 2，Σ w·E = 1，κ = −1。
		{name: "opposite", m: Matrix{{0, 0, 1}, {0, 0, 0}, {1, 0, 0}}, want: -1, wantOK: true},
		{name: "single category", m: Matrix{{0, 0, 0}, {0, 4, 0}, {0, 0, 0}}, wantOK: false},
		{name: "empty", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, ok := tt.m.QuadraticKappa()
			if ok != tt.wantOK || math.Abs(k-tt.want) > 1e-12 {
				t.Fatalf("kappa = %v ok %v, want %v ok %v", k, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestBootstrapKappa(t *testing.T) {
	m := Matrix{{2, 1, 0}, {0, 2, 1}, {0, 0, 1}}
	iv := BootstrapKappa(m.Pairs(), DefaultResamples, 7)
	if !iv.OK || iv.Kappa != 0.75 || iv.Low > iv.Kappa || iv.High < iv.Kappa || iv.Low >= iv.High {
		t.Fatalf("interval = %+v", iv)
	}
	if iv.Undefined >= DefaultResamples/10 {
		t.Fatalf("undefined resamples %d", iv.Undefined)
	}
	if again := BootstrapKappa(m.Pairs(), DefaultResamples, 7); again != iv {
		t.Fatalf("same seed differs: %+v vs %+v", again, iv)
	}
	perfect := BootstrapKappa(Matrix{{30, 0, 0}, {0, 30, 0}, {0, 0, 30}}.Pairs(), DefaultResamples, 1)
	if perfect.Low != 1 || perfect.High != 1 {
		t.Fatalf("perfect interval = %+v", perfect)
	}
	if empty := BootstrapKappa(nil, DefaultResamples, 1); empty.OK {
		t.Fatalf("empty = %+v", empty)
	}
}

// L2 评审 P1-3：无定义重抽计数且占比超 MaxUndefinedRatio 时区间不可用（不静默丢弃）；N 是评分对数。
func TestBootstrapKappa_UndefinedRatio(t *testing.T) {
	// 118 对全是 1-1，另两类各 1 对：点估计有定义（κ=1），但约 13% 的重抽只剩一个分数、κ 无定义。
	skewed := Matrix{{118, 0, 0}, {0, 1, 0}, {0, 0, 1}}.Pairs()
	iv := BootstrapKappa(skewed, DefaultResamples, 3)
	if !iv.OK || iv.N != 120 || iv.Undefined == 0 {
		t.Fatalf("interval = %+v", iv)
	}
	if float64(iv.Undefined)/float64(iv.Resamples) <= MaxUndefinedRatio || iv.Usable {
		t.Fatalf("undefined %d/%d should make the interval unusable: %+v", iv.Undefined, iv.Resamples, iv)
	}
	good := BootstrapKappa(Matrix{{30, 0, 0}, {0, 30, 0}, {0, 0, 30}}.Pairs(), DefaultResamples, 1)
	if !good.Usable || good.Undefined != 0 || good.N != 90 {
		t.Fatalf("balanced interval = %+v", good)
	}
	if none := BootstrapKappa(skewed, 0, 1); none.Usable {
		t.Fatalf("no resamples should be unusable: %+v", none)
	}
}

func TestNewMatrix_IgnoresOutOfRange(t *testing.T) {
	m := NewMatrix([]Pair{{1, 1}, {3, 2}, {0, 1}, {4, 3}})
	if m.Total() != 2 || m[2][1] != 1 || len(m.Pairs()) != 2 {
		t.Fatalf("matrix = %v", m)
	}
}
