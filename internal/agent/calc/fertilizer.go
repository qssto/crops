package calc

import "fmt"

// FertStage 是施肥时期，对应 rice/fertilization/n-budget 的 <stage>_ratio 参数。
type FertStage string

// 施肥时期。
const (
	FertBasal          FertStage = "basal"
	FertTillering      FertStage = "tillering"
	FertPanicleProtect FertStage = "panicle_protect"
	FertPaniclePromote FertStage = "panicle_promote"
)

// FertProduct 是肥料实物，对应 <product>_n_content 参数。
type FertProduct string

// 肥料实物。
const (
	ProductUrea     FertProduct = "urea"     // 尿素 46% N
	ProductCompound FertProduct = "compound" // 45% 复合肥（15-15-15）
)

// FertSplit 是分蘖肥分次：none 不分次；first / second 取 rice/fertilization/tillering 的分次比例。
type FertSplit string

// 分次。
const (
	SplitNone   FertSplit = ""
	SplitFirst  FertSplit = "first"
	SplitSecond FertSplit = "second"
)

// FertInput 是施肥量计算输入。SoilNLevel 取 field.profile.soil_n_level（低/中/高）。
type FertInput struct {
	SoilNLevel string
	Stage      FertStage
	Product    FertProduct
	Split      FertSplit // 仅 Stage=tillering 时可非空
	AreaMu     float64
}

// FertResult 是施肥三步结果：纯氮 / 亩实物 / 总实物，各自带公式出处。
type FertResult struct {
	NPerMu       Result `json:"n_per_mu"`
	ProductPerMu Result `json:"product_per_mu"`
	ProductTotal Result `json:"product_total"`
}

// Fertilizer 按 rice/fertilization/n-budget 计算：
// 纯氮 = 总施氮量(soil_n_level) × 时期占比 [× 分次比例]（#n_for_stage）；
// 亩实物 = 纯氮 ÷ 含氮量（#product_per_mu）；总量 = 亩实物 × 面积（#product_total）。
// 未知土壤氮水平 / 时期 / 肥料返回 ErrOutOfRange（知识库没有该 when 分支时为 ErrMissingParam）。
func (c *Calculator) Fertilizer(in FertInput) (FertResult, error) {
	switch in.Stage {
	case FertBasal, FertTillering, FertPanicleProtect, FertPaniclePromote:
	default:
		return FertResult{}, fmt.Errorf("%w: stage %q not in [basal tillering panicle_promote panicle_protect]", ErrOutOfRange, in.Stage)
	}
	switch in.Product {
	case ProductUrea, ProductCompound:
	default:
		return FertResult{}, fmt.Errorf("%w: product %q not in [urea compound]", ErrOutOfRange, in.Product)
	}
	switch in.SoilNLevel {
	case "低", "中", "高":
	default:
		return FertResult{}, fmt.Errorf("%w: soil_n_level %q not in [低 中 高]", ErrOutOfRange, in.SoilNLevel)
	}
	if err := requirePositive("area_mu", in.AreaMu); err != nil {
		return FertResult{}, err
	}
	total, err := c.param(KBNBudget, "total_n_per_mu", map[string]any{"soil_n_level": in.SoilNLevel})
	if err != nil {
		return FertResult{}, err
	}
	ratio, err := c.param(KBNBudget, string(in.Stage)+"_ratio", nil)
	if err != nil {
		return FertResult{}, err
	}
	nIn := map[string]any{"soil_n_level": in.SoilNLevel, "total_n_per_mu": total,
		"stage": string(in.Stage), "stage_ratio": ratio}
	n := total * ratio
	if in.Split != SplitNone {
		if in.Stage != FertTillering || (in.Split != SplitFirst && in.Split != SplitSecond) {
			return FertResult{}, fmt.Errorf("%w: split %q only allowed as first/second for tillering", ErrOutOfRange, in.Split)
		}
		pid := "tillering_split_" + string(in.Split)
		split, err := c.param(KBTillering, pid, nil)
		if err != nil {
			return FertResult{}, err
		}
		e, err := c.entry(KBTillering)
		if err != nil {
			return FertResult{}, err
		}
		n *= split
		nIn["split"], nIn["split_ratio"], nIn["split_ref"] = string(in.Split), split, e.FormulaRef(pid)
	}
	content, err := c.param(KBNBudget, string(in.Product)+"_n_content", nil)
	if err != nil {
		return FertResult{}, err
	}
	refs := make([]string, 3)
	for i, f := range []string{FormulaNForStage, FormulaProductPerMu, FormulaProductTotal} {
		if refs[i], err = c.formulaRef(KBNBudget, f); err != nil {
			return FertResult{}, err
		}
	}
	perMu := n / content
	return FertResult{
		NPerMu: Result{Value: n, Unit: "kg N/亩", Precision: PrecisionNPerMu, Trace: Trace{Inputs: nIn, Formula: refs[0]}},
		ProductPerMu: Result{Value: perMu, Unit: "kg/亩", Precision: PrecisionFertPerMu, Trace: Trace{
			Inputs:  map[string]any{"n_kg_per_mu": n, "product": string(in.Product), "product_n_content": content},
			Formula: refs[1]}},
		ProductTotal: Result{Value: perMu * in.AreaMu, Unit: "kg", Precision: PrecisionFertTotal, Trace: Trace{
			Inputs:  map[string]any{"product_kg_per_mu": perMu, "area_mu": in.AreaMu},
			Formula: refs[2]}},
	}, nil
}

// WaterVolume 按 rice/irrigation/water-volume#water_m3：
// 灌水量 = 面积 × 666.67 × (目标水层 − 当前水层)/100 × 损失系数(after_drain)。目标须高于当前。
func (c *Calculator) WaterVolume(areaMu, targetCm, currentCm float64, afterDrain bool) (Result, error) {
	if err := requirePositive("area_mu", areaMu); err != nil {
		return Result{}, err
	}
	if currentCm < 0 {
		return Result{}, fmt.Errorf("%w: current_depth_cm must be >= 0, got %g", ErrOutOfRange, currentCm)
	}
	if err := requirePositive("target_depth_cm - current_depth_cm", targetCm-currentCm); err != nil {
		return Result{}, err
	}
	m2, err := c.param(KBWaterVolume, "m2_per_mu", nil)
	if err != nil {
		return Result{}, err
	}
	loss, err := c.param(KBWaterVolume, "loss_factor", map[string]any{"after_drain": afterDrain})
	if err != nil {
		return Result{}, err
	}
	ref, err := c.formulaRef(KBWaterVolume, FormulaWaterM3)
	if err != nil {
		return Result{}, err
	}
	in := map[string]any{"area_mu": areaMu, "target_depth_cm": targetCm, "current_depth_cm": currentCm,
		"after_drain": afterDrain, "m2_per_mu": m2, "loss_factor": loss}
	return Result{Value: areaMu * m2 * (targetCm - currentCm) / 100 * loss, Unit: "m3",
		Precision: PrecisionWaterM3, Trace: Trace{Inputs: in, Formula: ref}}, nil
}
