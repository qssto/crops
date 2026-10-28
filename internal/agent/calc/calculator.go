package calc

import (
	"errors"
	"fmt"

	"agri-steward/internal/agent/knowledge"
)

// 计算依赖的知识条目与公式 id（与 knowledge/ front-matter 一致）。
const (
	KBDosageDilution = "common/operation_window/dosage-dilution"
	KBUAV            = "common/operation_window/uav"
	KBPHI            = "common/safety/phi"
	KBNBudget        = "rice/fertilization/n-budget"
	KBTillering      = "rice/fertilization/tillering"
	KBWaterVolume    = "rice/irrigation/water-volume"

	FormulaTotalDose        = "total_dose"
	FormulaDilutionRatio    = "dilution_ratio"
	FormulaDoseFromDilution = "dose_from_dilution"
	FormulaLastSprayDate    = "last_spray_date"
	FormulaEarliestHarvest  = "earliest_harvest"
	FormulaNForStage        = "n_for_stage"
	FormulaProductPerMu     = "product_per_mu"
	FormulaProductTotal     = "product_total"
	FormulaWaterM3          = "water_m3"
)

// registryRef 是登记表在 trace 中的写法。
const registryRef = "common/pesticide_registry"

// required 是 New 时必须存在的 (kb_id, formula id)；缺一即拒绝构造（fail fast）。
var required = [][2]string{
	{KBDosageDilution, FormulaTotalDose}, {KBDosageDilution, FormulaDilutionRatio},
	{KBDosageDilution, FormulaDoseFromDilution},
	{KBPHI, FormulaLastSprayDate}, {KBPHI, FormulaEarliestHarvest},
	{KBNBudget, FormulaNForStage}, {KBNBudget, FormulaProductPerMu}, {KBNBudget, FormulaProductTotal},
	{KBWaterVolume, FormulaWaterM3},
}

// Calculator 绑定一个知识版本，只读、可并发使用。
type Calculator struct {
	snap *knowledge.Snapshot
	reg  *knowledge.Registry
}

// New 用知识快照构造 Calculator；所需条目或公式缺失时报错。
func New(snap *knowledge.Snapshot) (*Calculator, error) {
	if snap == nil || snap.Registry() == nil {
		return nil, errors.New("calc: snapshot or registry is nil")
	}
	var errs []error
	for _, r := range required {
		e, ok := snap.Entry(r[0])
		if !ok {
			errs = append(errs, fmt.Errorf("%w: entry %s", ErrMissingParam, r[0]))
			continue
		}
		if _, ok := e.Formula(r[1]); !ok {
			errs = append(errs, fmt.Errorf("%w: formula %s#%s", ErrMissingParam, r[0], r[1]))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Calculator{snap: snap, reg: snap.Registry()}, nil
}

// RegistryRef 返回登记表引用 common/pesticide_registry@<version>。
func (c *Calculator) RegistryRef() string { return registryRef + "@" + c.reg.Version }

func (c *Calculator) entry(kbID string) (*knowledge.Entry, error) {
	e, ok := c.snap.Entry(kbID)
	if !ok {
		return nil, fmt.Errorf("%w: entry %s", ErrMissingParam, kbID)
	}
	return e, nil
}

// formulaRef 返回 <kb_id>@<version>#<formula id>，公式不存在时报错。
func (c *Calculator) formulaRef(kbID, formulaID string) (string, error) {
	e, err := c.entry(kbID)
	if err != nil {
		return "", err
	}
	if _, ok := e.Formula(formulaID); !ok {
		return "", fmt.Errorf("%w: formula %s#%s", ErrMissingParam, kbID, formulaID)
	}
	return e.FormulaRef(formulaID), nil
}

// param 取参数点值（无 value 时报错）。
func (c *Calculator) param(kbID, id string, when map[string]any) (float64, error) {
	p, err := c.paramRange(kbID, id, when)
	if err != nil {
		return 0, err
	}
	if p.Value == nil {
		return 0, fmt.Errorf("%w: %s %s has no point value", ErrMissingParam, kbID, id)
	}
	return *p.Value, nil
}

func (c *Calculator) paramRange(kbID, id string, when map[string]any) (knowledge.Param, error) {
	e, err := c.entry(kbID)
	if err != nil {
		return knowledge.Param{}, err
	}
	p, ok := e.Param(id, when)
	if !ok {
		return knowledge.Param{}, fmt.Errorf("%w: %s param %s when %v", ErrMissingParam, kbID, id, when)
	}
	return p, nil
}

// Candidates 返回 crop × target × region 的登记药剂 id（登记表顺序）；空集返回 ErrNoCandidates。
// out_of_set_test 产品没有水稻登记，按三者过滤后自然不会出现。
func (c *Calculator) Candidates(crop, target, region string) ([]string, error) {
	var out []string
	for _, p := range c.reg.Products {
		for _, g := range p.Registrations {
			if g.Matches(crop, target, region) {
				out = append(out, p.ID)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: crop=%s target=%s region=%s", ErrNoCandidates, crop, target, region)
	}
	return out, nil
}

// Use 指定药剂与适用范围：作物 × 防治对象 × 区域，用于查登记用量与 PHI。
type Use struct {
	ProductID string
	Crop      string
	Target    string
	Region    string
}

func (u Use) inputs() map[string]any {
	return map[string]any{"product_id": u.ProductID, "crop": u.Crop, "target": u.Target, "region": u.Region}
}

// Registration 返回药剂在该适用范围下的登记；无登记返回 ErrNotRegistered。
func (c *Calculator) Registration(u Use) (knowledge.Registration, error) {
	p, ok := c.reg.Product(u.ProductID)
	if ok {
		for _, g := range p.Registrations {
			if g.Matches(u.Crop, u.Target, u.Region) {
				return g, nil
			}
		}
	}
	return knowledge.Registration{}, fmt.Errorf("%w: %s for crop=%s target=%s region=%s",
		ErrNotRegistered, u.ProductID, u.Crop, u.Target, u.Region)
}
