package constraint

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
)

// RegulationMapVersion 是本包支持的 policy/regulation_map.yaml 版本。
const RegulationMapVersion = 1

// UnmappedPrefix 是未映射法规条目在 constraint_check.rule_id 中的前缀（reg:<id>）。
const UnmappedPrefix = "reg:"

// ErrBadRegulationMap 表示 regulation_map.yaml 不合法（加载期，启动失败）。
var ErrBadRegulationMap = errors.New("constraint: invalid regulation_map")

// 法规条目的映射方式。
const (
	KindChecker      = "checker"      // 由约束 Checker 判定
	KindPrecondition = "precondition" // 由法规前置条件判定（校验顺序最前）
	KindUnmapped     = "unmapped"     // 尚无工程判定：适用即失败（失败关闭）
)

// AppliesTo 是法规条目的适用范围；空列表表示不限。
type AppliesTo struct {
	WorkTypes     []model.WorkType     `yaml:"work_types"`
	DeviceTypes   []string             `yaml:"device_types"`
	DispatchModes []model.DispatchMode `yaml:"dispatch_modes"`
}

// RegulationEntry 是 regulation_map.yaml 的一条法规条款映射。
type RegulationEntry struct {
	ID         string    `yaml:"id"`
	Title      string    `yaml:"title"`
	Section    string    `yaml:"section"`
	Kind       string    `yaml:"kind"`
	RuleID     string    `yaml:"rule_id"`
	AppliesTo  AppliesTo `yaml:"applies_to"`
	LegalBasis string    `yaml:"legal_basis"`
	Note       string    `yaml:"note"`
}

// Applies 判断条目是否适用于该作业单。
func (e RegulationEntry) Applies(wt model.WorkType, device string, mode model.DispatchMode) bool {
	a := e.AppliesTo
	return (len(a.WorkTypes) == 0 || slices.Contains(a.WorkTypes, wt)) &&
		(len(a.DeviceTypes) == 0 || slices.Contains(a.DeviceTypes, device)) &&
		(len(a.DispatchModes) == 0 || slices.Contains(a.DispatchModes, mode))
}

// CheckRuleID 是该条目在 constraint_check 中的 rule_id：checker / precondition 为所映射的 rule_id，unmapped 为 reg:<id>。
func (e RegulationEntry) CheckRuleID() string {
	if e.Kind == KindUnmapped {
		return UnmappedPrefix + e.ID
	}
	return e.RuleID
}

// RegulationMap 是 policy/regulation_map.yaml。构造后只读，并发安全。
type RegulationMap struct {
	entries []RegulationEntry
}

type regulationMapFile struct {
	Version int               `yaml:"version"`
	Entries []RegulationEntry `yaml:"entries"`
}

var (
	knownKinds   = []string{KindChecker, KindPrecondition, KindUnmapped}
	knownWork    = []model.WorkType{model.WorkSpray, model.WorkFertilize, model.WorkIrrigate}
	knownDevices = []string{DeviceKnapsack, DeviceUAV, DeviceManual, DeviceValve}
	knownModes   = []model.DispatchMode{model.DispatchDryRun, model.DispatchReal}
)

// ParseRegulationMap 严格解析（未知字段即失败）并校验：id 唯一且非空；kind 合法；checker / precondition 的 rule_id
// 已在 reg 注册且种类一致；unmapped 不带 rule_id，reg:<id> 不超过 RuleIDMax；适用范围取值合法；
// legal_basis 必填（未定为「待法务填写」）。
func ParseRegulationMap(b []byte, reg *Registry) (*RegulationMap, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrBadRegulationMap)
	}
	var f regulationMapFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadRegulationMap, err)
	}
	if f.Version != RegulationMapVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadRegulationMap, f.Version, RegulationMapVersion)
	}
	var errs []error
	seen := make(map[string]bool, len(f.Entries))
	for _, e := range f.Entries {
		if seen[e.ID] {
			errs = append(errs, fmt.Errorf("%w: duplicate id %q", ErrBadRegulationMap, e.ID))
			continue
		}
		seen[e.ID] = true
		errs = append(errs, checkEntry(e, reg))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &RegulationMap{entries: slices.Clone(f.Entries)}, nil
}

func checkEntry(e RegulationEntry, reg *Registry) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrBadRegulationMap, e.ID, fmt.Sprintf(format, a...))
	}
	var errs []error
	if e.ID == "" || e.Title == "" || e.LegalBasis == "" {
		errs = append(errs, bad("id, title and legal_basis are required"))
	}
	switch e.Kind {
	case KindChecker, KindPrecondition:
		switch {
		case !reg.Has(e.RuleID):
			errs = append(errs, bad("rule_id %q not registered", e.RuleID))
		case reg.IsPrecondition(e.RuleID) != (e.Kind == KindPrecondition):
			errs = append(errs, bad("rule_id %q is not a %s", e.RuleID, e.Kind))
		}
	case KindUnmapped:
		if e.RuleID != "" {
			errs = append(errs, bad("unmapped entry must not set rule_id"))
		}
		if len(e.CheckRuleID()) > RuleIDMax {
			errs = append(errs, bad("%q exceeds %d", e.CheckRuleID(), RuleIDMax))
		}
	default:
		errs = append(errs, bad("kind %q, want one of %v", e.Kind, knownKinds))
	}
	errs = append(errs, unknown(bad, "work_types", e.AppliesTo.WorkTypes, knownWork),
		unknown(bad, "device_types", e.AppliesTo.DeviceTypes, knownDevices),
		unknown(bad, "dispatch_modes", e.AppliesTo.DispatchModes, knownModes))
	return errors.Join(errs...)
}

func unknown[T comparable](bad func(string, ...any) error, field string, got, known []T) error {
	for _, v := range got {
		if !slices.Contains(known, v) {
			return bad("%s: unknown value %v", field, v)
		}
	}
	return nil
}

// Applicable 返回适用于该作业单的条目（文件顺序）。
func (m *RegulationMap) Applicable(wt model.WorkType, device string, mode model.DispatchMode) []RegulationEntry {
	var out []RegulationEntry
	for _, e := range m.entries {
		if e.Applies(wt, device, mode) {
			out = append(out, e)
		}
	}
	return out
}

// Entries 返回全部条目（副本）。
func (m *RegulationMap) Entries() []RegulationEntry { return slices.Clone(m.entries) }
