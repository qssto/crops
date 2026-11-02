package knowledge

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

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

// Registry 是农药登记表：药剂候选集合的唯一来源，也是安全间隔期（PHI）的唯一来源。加载后只读。
type Registry struct {
	Version       string            `yaml:"version"`
	Author        string            `yaml:"author"`
	ReviewStatus  string            `yaml:"review_status"`
	EffectiveFrom string            `yaml:"effective_from"`
	Targets       map[string]string `yaml:"targets"`
	// Formulations 是药剂名归一时从结尾剥掉的剂型名（与 aliases 同属药名写法知识）；加载时按字数从长到短排序，
	// 避免「悬浮剂」先于「可分散油悬浮剂」匹配。
	Formulations []string  `yaml:"formulations"`
	Products     []Product `yaml:"products"`
}

// Product 是一个农药产品。
type Product struct {
	ID             string  `yaml:"id"`
	CommonName     string  `yaml:"common_name"`
	Formulation    string  `yaml:"formulation"`
	Content        string  `yaml:"content"`
	RegistrationNo *string `yaml:"registration_no"`
	// Aliases 是农事记录 / 作业单里可能出现的商品名、俗称（含量 + 剂型的写法由 Registry.NormalizeProductName 归一，不必列）。
	Aliases      []string `yaml:"aliases,omitempty"`
	OutOfSetTest bool     `yaml:"out_of_set_test,omitempty"`
	// Candidate 缺省为 true；false 表示只作 PHI 来源（如农户常用但不推荐的药，用于识别历史施药的安全间隔期），
	// 不进候选集合、不能作为作业单所用药剂（calc.Candidates / calc.Registration 均跳过）。
	Candidate     *bool          `yaml:"candidate,omitempty"`
	Registrations []Registration `yaml:"registrations"`
}

// IsCandidate 表示产品可进候选集合（candidate 缺省或为 true）。
func (p *Product) IsCandidate() bool { return p.Candidate == nil || *p.Candidate }

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
	slices.SortStableFunc(reg.Formulations, func(a, b string) int {
		return cmp.Compare(utf8.RuneCountInString(b), utf8.RuneCountInString(a))
	})
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
	if len(r.Formulations) == 0 {
		fail("formulations required")
	}
	forms := map[string]bool{}
	for i, f := range r.Formulations {
		if strings.TrimSpace(f) != f || f == "" || forms[f] {
			fail("formulations[%d]: %q empty, padded or duplicate", i, f)
		}
		forms[f] = true
	}
	seen := map[string]bool{}
	for i, p := range r.Products {
		if p.ID == "" || seen[p.ID] {
			fail("products[%d]: id %q empty or duplicate", i, p.ID)
		}
		seen[p.ID] = true
		for _, a := range p.Aliases {
			if r.NormalizeProductName(a, "") == "" {
				fail("%s: alias %q empty", p.ID, a)
			}
		}
		phiOnly := !p.IsCandidate()
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
			if !phiOnly || g.Dose != (DoseRange{}) { // 仅 PHI 来源的条目可不写用量，写了仍须合法
				validateDose(g.Dose, where, fail)
			}
			if g.PHIDays != nil && *g.PHIDays < 0 {
				fail("%s: phi_days must be >= 0", where)
			}
			if phiOnly && g.PHIDays == nil {
				fail("%s: candidate:false product is a PHI source, phi_days required", where)
			}
		}
	}
	return errors.Join(errs...)
}

func validateDose(d DoseRange, where string, fail func(format string, args ...any)) {
	switch d.Unit {
	case DoseUnitGramPerMu, DoseUnitMLPerMu, DoseUnitRatio:
	default:
		fail("%s: dose.unit %q not in [g/亩 mL/亩 倍]", where, d.Unit)
	}
	if d.Min <= 0 || d.Min > d.Max {
		fail("%s: dose needs 0 < min <= max", where)
	}
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

// NormalizeProductName 把农事记录里的药剂写法归一为通用名候选：去空白，去开头的含量（如「24%」「200 g/L」「5％」），
// 再去结尾的剂型（formulation 非空时先试它，再按登记表 formulations 从长到短试）。如「24%噻呋酰胺悬浮剂」→「噻呋酰胺」。
func (r *Registry) NormalizeProductName(name, formulation string) string {
	s := strings.Join(strings.Fields(name), "")
	s = contentPrefix.ReplaceAllString(s, "")
	for _, f := range append([]string{formulation}, r.Formulations...) {
		if f != "" && strings.HasSuffix(s, f) && len(s) > len(f) {
			s = strings.TrimSuffix(s, f)
			break
		}
	}
	return s
}

// contentPrefix 匹配开头的有效成分含量：数字 + %/％ 或 数字 + g/L、克/升 等。
var contentPrefix = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:%|％|(?:g|克)/(?:L|l|升)|(?:g|克)/(?:kg|千克))`)

// Resolve 按名称查产品（含 candidate:false 的仅 PHI 来源条目）：先按 id / 通用名 / 别名精确匹配，否则按
// NormalizeProductName 归一后比较通用名与别名。可能返回多个产品（同一通用名的不同剂型）；查不到返回 nil。
func (r *Registry) Resolve(name string) []*Product {
	if out := r.match(func(p *Product, n string) bool { return n == name }); len(out) > 0 {
		return out
	}
	return r.match(func(p *Product, n string) bool {
		norm := r.NormalizeProductName(name, p.Formulation)
		return norm != "" && r.NormalizeProductName(n, p.Formulation) == norm
	})
}

// match 返回 id / 通用名 / 任一别名满足 eq 的产品。
func (r *Registry) match(eq func(p *Product, name string) bool) []*Product {
	var out []*Product
	for i := range r.Products {
		p := &r.Products[i]
		if eq(p, p.ID) || eq(p, p.CommonName) || slices.ContainsFunc(p.Aliases, func(a string) bool { return eq(p, a) }) {
			out = append(out, p)
		}
	}
	return out
}
