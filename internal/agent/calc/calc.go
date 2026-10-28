// Package calc 是规则层计算（红线资产）：药剂候选集合、亩用量 → 总用量、稀释倍数、
// PHI 推算、施肥量、灌水量。所有数值只从知识库 front-matter 与农药登记表取参数，
// 每个结果带计算过程 Trace，供 L4 回填 AdviceCard.numbers 与 numbers_match_calc 比对。
//
// 数值类型选 float64 而非 decimal：输入来自 YAML 浮点、量级 ≤ 1e6、有效小数 ≤ 4 位，
// float64 相对误差约 1e-16，远小于输出精度（最细 0.01）；中间结果保留全精度，
// 只在输出（Round）时按精度舍入，比较一律按精度进行。不引入 decimal 依赖。
package calc

import (
	"errors"
	"fmt"
	"math"
)

// 输出精度（numbers_match_calc 按字段精度比较；样本 numbers[].precision 可更粗，不可更细）。
const (
	PrecisionDosePerMu   = 0.1  // 亩用量 g/亩、mL/亩
	PrecisionTotalDose   = 0.1  // 总用量 g、mL
	PrecisionDilution    = 1.0  // 稀释倍数 倍
	PrecisionSprayVolume = 0.1  // 亩施药液量 L/亩
	PrecisionNPerMu      = 0.01 // 纯氮 kg N/亩
	PrecisionFertPerMu   = 0.1  // 肥料实物 kg/亩
	PrecisionFertTotal   = 0.5  // 肥料总量 kg
	PrecisionWaterM3     = 1.0  // 灌水量 m3
	PrecisionDays        = 1.0  // 天数
)

// 哨兵错误；调用方用 errors.Is 判断。超范围一律报错，不截断到边界值。
var (
	ErrNoCandidates     = errors.New("calc: no registered product for crop × target × region")
	ErrNotRegistered    = errors.New("calc: product not registered for crop × target × region")
	ErrOutOfRange       = errors.New("calc: input out of range")
	ErrUnitMismatch     = errors.New("calc: unit mismatch")
	ErrPHINotApplicable = errors.New("calc: phi not applicable (phi_days is null)")
	ErrMissingParam     = errors.New("calc: knowledge param or formula missing")
)

// Trace 是计算过程：输入（含取自知识库的参数）与公式出处 <kb_id>@<version>#<formula id>。
type Trace struct {
	Inputs  map[string]any `json:"inputs"`
	Formula string         `json:"formula"`
}

// Result 是一个数值结果，Value 为全精度；输出时调用 Rounded。
type Result struct {
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	Precision float64 `json:"precision"`
	Trace     Trace   `json:"trace"`
}

// Rounded 按 Precision 舍入后的值。
func (r Result) Rounded() float64 { return Round(r.Value, r.Precision) }

// Round 把 v 舍入到 precision 的整数倍（四舍五入，远离零）；precision ≤ 0 时原样返回。
func Round(v, precision float64) float64 {
	if precision <= 0 {
		return v
	}
	q := v / precision
	// 抵消 0.15/0.05 这类二进制表示误差，避免 2.9999999 舍成 2
	q = math.Round(q*1e9) / 1e9
	r := math.Round(q) * precision
	// 去掉 0.1*3 = 0.30000000000000004 这类尾差
	return math.Round(r*1e9) / 1e9
}

// Equal 判断两个值在 precision 下是否相等（numbers_match_calc 用）。
func Equal(a, b, precision float64) bool { return Round(a, precision) == Round(b, precision) }

func outOfRange(name string, v, min, max float64, unit string) error {
	return fmt.Errorf("%w: %s %g %s not in [%g, %g]", ErrOutOfRange, name, v, unit, min, max)
}

func requirePositive(name string, v float64) error {
	if !(v > 0) || math.IsInf(v, 0) {
		return fmt.Errorf("%w: %s must be > 0, got %g", ErrOutOfRange, name, v)
	}
	return nil
}
