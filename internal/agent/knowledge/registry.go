package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// RegistryFile 是农药登记表在 knowledge/ 下的路径。
const RegistryFile = "common/pesticide_registry.yaml"

// 登记用量单位（README §7）。
const (
	DoseUnitGramPerMu = "g/亩"
	DoseUnitMLPerMu   = "mL/亩"
	DoseUnitRatio     = "倍"
)

// Registry 是农药登记表：药剂候选集合的唯一来源。加载后只读。
type Registry struct {
	Version       string            `yaml:"version"`
	Author        string            `yaml:"author"`
	ReviewStatus  string            `yaml:"review_status"`
	EffectiveFrom string            `yaml:"effective_from"`
	Targets       map[string]string `yaml:"targets"`
	Products      []Product         `yaml:"products"`
}

// Product 是一个农药产品。
type Product struct {
	ID             string         `yaml:"id"`
	CommonName     string         `yaml:"common_name"`
	Formulation    string         `yaml:"formulation"`
	Content        string         `yaml:"content"`
	RegistrationNo *string        `yaml:"registration_no"`
	OutOfSetTest   bool           `yaml:"out_of_set_test,omitempty"`
	Registrations  []Registration `yaml:"registrations"`
}

// Registration 是一条登记：作物 × 防治对象 × 区域 → 用量、方法、安全间隔期。
type Registration struct {
	Crop              string    `yaml:"crop"`
	Target            string    `yaml:"target"`
	Dose              DoseRange `yaml:"dose"`
	Method            string    `yaml:"method"`
	DilutionNote      string    `yaml:"dilution_note"`
	PHIDays           *int      `yaml:"phi_days"` // nil 表示不适用（浸种、播后封闭）
	MaxTimesPerSeason int       `yaml:"max_times_per_season"`
	Regions           []string  `yaml:"regions"`
	Confidence        string    `yaml:"confidence"`
}

// DoseRange 是登记用量区间。
type DoseRange struct {
	Min  float64 `yaml:"min"`
	Max  float64 `yaml:"max"`
	Unit string  `yaml:"unit"`
}

// Matches 判断登记是否适用于 crop × target × region（regions 含 "*" 视为全国登记）。
func (r Registration) Matches(crop, target, region string) bool {
	return r.Crop == crop && r.Target == target &&
		(slices.Contains(r.Regions, Wildcard) || slices.Contains(r.Regions, region))
}

func parseRegistry(raw []byte) (*Registry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	reg := &Registry{}
	if err := dec.Decode(reg); err != nil {
		return nil, fmt.Errorf("%s: %w", RegistryFile, err)
	}
	if err := reg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", RegistryFile, err)
	}
	return reg, nil
}

func (r *Registry) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if r.Version == "" || r.Author == "" || r.EffectiveFrom == "" || len(r.Targets) == 0 {
		fail("version / author / effective_from / targets are required")
	}
	if r.Author == authorClaudeDraft && r.ReviewStatus != reviewPendingExpert {
		fail("author claude-draft must stay pending_expert")
	}
	seen := map[string]bool{}
	for i, p := range r.Products {
		if p.ID == "" || seen[p.ID] {
			fail("products[%d]: id %q empty or duplicate", i, p.ID)
		}
		seen[p.ID] = true
		if len(p.Registrations) == 0 {
			fail("%s: registrations required", p.ID)
		}
		for j, g := range p.Registrations {
			where := fmt.Sprintf("%s registrations[%d]", p.ID, j)
			if _, ok := r.Targets[g.Target]; !ok {
				fail("%s: target %q not in targets", where, g.Target)
			}
			if g.Crop == "" || len(g.Regions) == 0 {
				fail("%s: crop and regions required", where)
			}
			switch g.Dose.Unit {
			case DoseUnitGramPerMu, DoseUnitMLPerMu, DoseUnitRatio:
			default:
				fail("%s: dose.unit %q not in [g/亩 mL/亩 倍]", where, g.Dose.Unit)
			}
			if g.Dose.Min <= 0 || g.Dose.Min > g.Dose.Max {
				fail("%s: dose needs 0 < min <= max", where)
			}
			if g.PHIDays != nil && *g.PHIDays < 0 {
				fail("%s: phi_days must be >= 0", where)
			}
		}
	}
	return errors.Join(errs...)
}

// Product 按 id 查产品。
func (r *Registry) Product(id string) (*Product, bool) {
	for i := range r.Products {
		if r.Products[i].ID == id {
			return &r.Products[i], true
		}
	}
	return nil, false
}
