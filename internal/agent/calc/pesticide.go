package calc

import (
	"fmt"
	"strings"
	"time"

	"agri-steward/internal/agent/knowledge"
)

// Sprayer 是施药方式，决定亩施药液量的取值区间。
type Sprayer string

// 施药方式。
const (
	SprayerKnapsack Sprayer = "knapsack" // 背负式电动喷雾：common/operation_window/dosage-dilution#knapsack_spray_volume
	SprayerUAV      Sprayer = "uav"      // 无人机：common/operation_window/uav#uav_spray_volume
)

func (s Sprayer) param() (kbID, paramID string, err error) {
	switch s {
	case SprayerKnapsack:
		return KBDosageDilution, "knapsack_spray_volume", nil
	case SprayerUAV:
		return KBUAV, "uav_spray_volume", nil
	}
	return "", "", fmt.Errorf("%w: sprayer %q not in [knapsack uav]", ErrOutOfRange, s)
}

// perMuDose 校验药剂登记、用量单位为 g/亩 或 mL/亩、亩用量在登记区间内。
func (c *Calculator) perMuDose(u Use, dosePerMu float64) (knowledge.Registration, map[string]any, error) {
	g, err := c.Registration(u)
	if err != nil {
		return g, nil, err
	}
	if g.Dose.Unit != knowledge.DoseUnitGramPerMu && g.Dose.Unit != knowledge.DoseUnitMLPerMu {
		return g, nil, fmt.Errorf("%w: %s registered in %s, not per-mu dose", ErrUnitMismatch, u.ProductID, g.Dose.Unit)
	}
	if dosePerMu < g.Dose.Min || dosePerMu > g.Dose.Max {
		return g, nil, outOfRange("dose_per_mu", dosePerMu, g.Dose.Min, g.Dose.Max, g.Dose.Unit)
	}
	in := u.inputs()
	in["dose_per_mu"], in["dose_unit"] = dosePerMu, g.Dose.Unit
	in["dose_min"], in["dose_max"], in["registry"] = g.Dose.Min, g.Dose.Max, c.RegistryRef()
	return g, in, nil
}

// Dosage：总用量 = 亩用量 × 面积（common/operation_window/dosage-dilution#total_dose）。
// 亩用量须在登记区间内，否则返回 ErrOutOfRange（不截断）。
func (c *Calculator) Dosage(u Use, dosePerMu, areaMu float64) (Result, error) {
	g, in, err := c.perMuDose(u, dosePerMu)
	if err != nil {
		return Result{}, err
	}
	if err := requirePositive("area_mu", areaMu); err != nil {
		return Result{}, err
	}
	ref, err := c.formulaRef(KBDosageDilution, FormulaTotalDose)
	if err != nil {
		return Result{}, err
	}
	in["area_mu"] = areaMu
	return Result{Value: dosePerMu * areaMu, Unit: strings.TrimSuffix(g.Dose.Unit, "/亩"),
		Precision: PrecisionTotalDose, Trace: Trace{Inputs: in, Formula: ref}}, nil
}

// SprayVolume 返回施药方式的默认亩施药液量（知识库参数点值）；trace 写 <kb_id>@<version>#<param id>。
func (c *Calculator) SprayVolume(s Sprayer) (Result, error) {
	kbID, pid, err := s.param()
	if err != nil {
		return Result{}, err
	}
	p, err := c.paramRange(kbID, pid, nil)
	if err != nil {
		return Result{}, err
	}
	if p.Value == nil {
		return Result{}, fmt.Errorf("%w: %s %s has no point value", ErrMissingParam, kbID, pid)
	}
	e, err := c.entry(kbID)
	if err != nil {
		return Result{}, err
	}
	return Result{Value: *p.Value, Unit: p.Unit, Precision: PrecisionSprayVolume,
		Trace: Trace{Inputs: map[string]any{"sprayer": string(s)}, Formula: e.FormulaRef(pid)}}, nil
}

// Dilution：稀释倍数 = 亩施药液量(L) × 1000 ÷ 亩用量（common/operation_window/dosage-dilution#dilution_ratio）。
// 亩用量须在登记区间内，亩施药液量须在该施药方式的知识库区间内；g 与 mL 按 1 g ≈ 1 mL 折算（农艺惯例）。
func (c *Calculator) Dilution(u Use, dosePerMu float64, s Sprayer, sprayVolumeLPerMu float64) (Result, error) {
	_, in, err := c.perMuDose(u, dosePerMu)
	if err != nil {
		return Result{}, err
	}
	kbID, pid, err := s.param()
	if err != nil {
		return Result{}, err
	}
	p, err := c.paramRange(kbID, pid, nil)
	if err != nil {
		return Result{}, err
	}
	if p.Min == nil || p.Max == nil {
		return Result{}, fmt.Errorf("%w: %s %s needs min and max", ErrMissingParam, kbID, pid)
	}
	if sprayVolumeLPerMu < *p.Min || sprayVolumeLPerMu > *p.Max {
		return Result{}, outOfRange("spray_volume_l_per_mu", sprayVolumeLPerMu, *p.Min, *p.Max, p.Unit)
	}
	ref, err := c.formulaRef(KBDosageDilution, FormulaDilutionRatio)
	if err != nil {
		return Result{}, err
	}
	in["sprayer"], in["spray_volume_l_per_mu"] = string(s), sprayVolumeLPerMu
	return Result{Value: sprayVolumeLPerMu * 1000 / dosePerMu, Unit: knowledge.DoseUnitRatio,
		Precision: PrecisionDilution, Trace: Trace{Inputs: in, Formula: ref}}, nil
}

// DoseFromDilution：按稀释倍数登记的药剂（浸种等）由药液量求药量，药量(mL) = 药液(L) × 1000 ÷ 倍数。
// 倍数须在登记区间内；公式优先取防治对象条目（如 rice/pest/bakanae#dose_from_dilution），否则用通用条目。
func (c *Calculator) DoseFromDilution(u Use, ratio, waterL float64) (Result, error) {
	g, err := c.Registration(u)
	if err != nil {
		return Result{}, err
	}
	if g.Dose.Unit != knowledge.DoseUnitRatio {
		return Result{}, fmt.Errorf("%w: %s registered in %s, not dilution ratio", ErrUnitMismatch, u.ProductID, g.Dose.Unit)
	}
	if ratio < g.Dose.Min || ratio > g.Dose.Max {
		return Result{}, outOfRange("dilution_ratio", ratio, g.Dose.Min, g.Dose.Max, g.Dose.Unit)
	}
	if err := requirePositive("water_l", waterL); err != nil {
		return Result{}, err
	}
	ref, err := c.formulaRef(KBDosageDilution, FormulaDoseFromDilution)
	if err != nil {
		return Result{}, err
	}
	for _, e := range c.snap.Entries() {
		if _, ok := e.Formula(FormulaDoseFromDilution); ok && e.Target == u.Target && e.Crop == u.Crop {
			ref = e.FormulaRef(FormulaDoseFromDilution)
			break
		}
	}
	in := u.inputs()
	in["dilution_ratio"], in["water_l"] = ratio, waterL
	in["ratio_min"], in["ratio_max"], in["registry"] = g.Dose.Min, g.Dose.Max, c.RegistryRef()
	return Result{Value: waterL * 1000 / ratio, Unit: "mL", Precision: PrecisionTotalDose,
		Trace: Trace{Inputs: in, Formula: ref}}, nil
}

// PHIDirection 是安全间隔期推算方向。
type PHIDirection string

// 推算方向。
const (
	PHIForward  PHIDirection = "forward"  // 末次施药日 + PHI → 最早收获日（common/safety/phi#earliest_harvest）
	PHIBackward PHIDirection = "backward" // 预计收获日 − PHI → 最晚施药日（common/safety/phi#last_spray_date）
)

// DateResult 是日期结果：Date 为农场时区下的 YYYY-MM-DD，At 为该日 00:00（农场时区）。
type DateResult struct {
	Date  string    `json:"date"`
	At    time.Time `json:"at"`
	Unit  string    `json:"unit"`
	Trace Trace     `json:"trace"`
}

// PHIDate 按登记 PHI 推算日期。at 先换算到农场时区 loc 再取日历日（loc 必须显式传入，
// 不用 time.Local）；按日历日加减，跨夏令时不漂移。phi_days 为 null 时返回 ErrPHINotApplicable。
func (c *Calculator) PHIDate(u Use, dir PHIDirection, at time.Time, loc *time.Location) (DateResult, error) {
	if loc == nil {
		return DateResult{}, fmt.Errorf("%w: farm timezone is required", ErrOutOfRange)
	}
	if at.IsZero() {
		return DateResult{}, fmt.Errorf("%w: date is required", ErrOutOfRange)
	}
	g, err := c.Registration(u)
	if err != nil {
		return DateResult{}, err
	}
	if g.PHIDays == nil {
		return DateResult{}, fmt.Errorf("%w: %s for %s", ErrPHINotApplicable, u.ProductID, u.Target)
	}
	var formula, inName string
	days := *g.PHIDays
	switch dir {
	case PHIForward:
		formula, inName = FormulaEarliestHarvest, "spray_date"
	case PHIBackward:
		formula, inName, days = FormulaLastSprayDate, "expected_harvest", -days
	default:
		return DateResult{}, fmt.Errorf("%w: direction %q not in [forward backward]", ErrOutOfRange, dir)
	}
	ref, err := c.formulaRef(KBPHI, formula)
	if err != nil {
		return DateResult{}, err
	}
	y, m, d := at.In(loc).Date()
	day := time.Date(y, m, d+days, 0, 0, 0, 0, loc)
	in := u.inputs()
	in[inName], in["phi_days"], in["timezone"], in["registry"] = time.Date(y, m, d, 0, 0, 0, 0, loc).Format(time.DateOnly),
		*g.PHIDays, loc.String(), c.RegistryRef()
	return DateResult{Date: day.Format(time.DateOnly), At: day, Unit: "日期",
		Trace: Trace{Inputs: in, Formula: ref}}, nil
}
