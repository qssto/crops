package reduce

import (
	"math"
	"slices"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 5e-5 }

// Wilson 已知答案（z = 1.96，手算见口径表附录）：8/10 → [0.4902, 0.9433]；0/10 → [0, 0.2775]；10/10 → [0.7225, 1]。
func TestWilson_KnownAnswers(t *testing.T) {
	tests := []struct {
		k, n   int
		lo, hi float64
	}{
		{8, 10, 0.49016, 0.94332},
		{0, 10, 0, 0.27753},
		{10, 10, 0.72247, 1},
		{1, 2, 0.09453, 0.90547},
	}
	for _, tt := range tests {
		lo, hi := Wilson(tt.k, tt.n)
		if !near(lo, tt.lo) || !near(hi, tt.hi) {
			t.Errorf("Wilson(%d, %d) = [%.5f, %.5f], want [%.5f, %.5f]", tt.k, tt.n, lo, hi, tt.lo, tt.hi)
		}
	}
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 0 {
		t.Fatalf("Wilson(0, 0) = [%v, %v]", lo, hi)
	}
}

// pass^k 已知答案：n=5、c=4、k=3 → C(4,3)/C(5,3) = 4/10 = 0.4。
func TestPassHatK_KnownAnswer(t *testing.T) {
	if got := PassHatK(5, 4, 3); !near(got, 0.4) {
		t.Fatalf("pass^3 = %v, want 0.4", got)
	}
}

func TestBootstrapMean(t *testing.T) {
	if lo, hi := BootstrapMean([]float64{0.5, 0.5, 0.5}, 1000, 1); lo != 0.5 || hi != 0.5 {
		t.Fatalf("constant = [%v, %v]", lo, hi)
	}
	xs := []float64{0, 0, 1, 1, 1, 0.5}
	lo1, hi1 := BootstrapMean(xs, 1000, 9)
	lo2, hi2 := BootstrapMean(xs, 1000, 9)
	if lo1 != lo2 || hi1 != hi2 || lo1 >= hi1 || lo1 < 0 || hi1 > 1 {
		t.Fatalf("seeded bootstrap = [%v, %v] / [%v, %v]", lo1, hi1, lo2, hi2)
	}
	if lo, hi := BootstrapMean(nil, 1000, 1); lo != 0 || hi != 0 {
		t.Fatalf("empty = [%v, %v]", lo, hi)
	}
}

// Holm（α = 0.05，m = 4）：排序后 0.005 ≤ 0.0125、0.01 ≤ 0.0167 拒绝，0.03 > 0.025 停止 → 只有下标 3、0 显著。
func TestHolm_KnownAnswer(t *testing.T) {
	got := Holm([]float64{0.01, 0.04, 0.03, 0.005}, 0.05)
	if want := []bool{true, false, false, true}; !slices.Equal(got, want) {
		t.Fatalf("Holm = %v, want %v", got, want)
	}
	// 未校正时 0.03、0.04 都 < 0.05，校正后不显著：说明切片标红前必须校正。
	if got := Holm([]float64{0.04}, 0.05); !got[0] {
		t.Fatal("single test 0.04 must reject")
	}
}

func TestDiffPValue(t *testing.T) {
	// 两比例差 z 检验：0.9 (n=100) vs 0.7 (n=100) → z ≈ 3.54，p ≈ 0.0004。
	p := TwoProportionP(90, 100, 70, 100)
	if p <= 0 || p > 0.001 {
		t.Fatalf("two-proportion p = %v", p)
	}
	if p := TwoProportionP(5, 10, 5, 10); p != 1 {
		t.Fatalf("equal proportions p = %v", p)
	}
	// 区间差：半宽换算标准误（1.96），差 0 → p = 1。
	if p := IntervalDiffP(0.5, 0.4, 0.6, 0.5, 0.4, 0.6); p != 1 {
		t.Fatalf("interval same p = %v", p)
	}
	if p := IntervalDiffP(0.9, 0.85, 0.95, 0.5, 0.45, 0.55); p > 1e-6 {
		t.Fatalf("interval far p = %v", p)
	}
}
