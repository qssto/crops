package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
)

// RiskMatrixVersion 是本包支持的 risk_matrix.yaml 版本（P1 L5 起按规则判定，v2）。
const RiskMatrixVersion = 2

var (
	// ErrBadRiskMatrix 是 risk_matrix.yaml 不合法（加载期，启动失败）。
	ErrBadRiskMatrix = errors.New("policy: invalid risk_matrix")
	// ErrNoRisk 是作业类型或风险等级未在矩阵中列出（开票期，不默认放行）。
	ErrNoRisk = errors.New("policy: not listed in risk_matrix")
)

// Risk 是一张作业单的审批定级。
type Risk struct {
	Level           model.RiskLevel
	Roles           []model.Role // 可审批角色，非空
	ConfirmRequired bool         // HIGH 必为 true：需第二审批人确认
	// Reasons 是命中的风险原因（按出现顺序：PESTICIDE → LARGE_AREA → NEAR_THRESHOLD:{rule_id}…），为空即 LOW。
	Reasons []string
}

// RiskInput 是定级输入：作业类型、作业面积（payload.area_mu，未知为 nil）与 PRE_APPROVAL 复核后的校验行。
type RiskInput struct {
	WorkType model.WorkType
	AreaMu   *float64
	Checks   []model.ConstraintCheck
}

// RiskMatrix 是 policy/risk_matrix.yaml。构造后只读，并发安全。
type RiskMatrix struct {
	roles       map[model.WorkType][]model.Role
	levels      map[model.RiskLevel]levelRule
	largeAreaMu float64
	near        map[string]float64 // rule_id → 相对裕度（|阈值 − 实测| ≤ 裕度 × |阈值| 即临界）
}

type levelRule struct {
	confirm bool
	ttl     time.Duration
}

type riskMatrixFile struct {
	Version   int `yaml:"version"`
	WorkTypes map[model.WorkType]struct {
		RequiredRole []model.Role `yaml:"required_role"`
	} `yaml:"work_types"`
	Rules struct {
		LargeAreaMu   float64            `yaml:"large_area_mu"`
		NearThreshold map[string]float64 `yaml:"near_threshold"`
	} `yaml:"rules"`
	Levels map[model.RiskLevel]struct {
		ConfirmRequired bool   `yaml:"confirm_required"`
		ApprovalTTL     string `yaml:"approval_ttl"`
	} `yaml:"levels"`
}

// 可作为农场作业单审批人的角色（platform_admin 不审批）。
var approverRoles = []model.Role{model.RoleFarmManager, model.RoleAgronomist, model.RoleOperator}

// 已知作业类型与风险等级；requiredWorkTypes 是矩阵必须列出的作业类型（用药类，缺失即启动失败）。
var (
	knownWorkTypes    = []model.WorkType{model.WorkSpray, model.WorkFertilize, model.WorkIrrigate}
	knownRisks        = []model.RiskLevel{model.RiskLow, model.RiskHigh}
	requiredWorkTypes = []model.WorkType{model.WorkSpray}
)

// LoadRiskMatrix 读取并校验 risk_matrix.yaml。
func LoadRiskMatrix(path string) (*RiskMatrix, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read risk_matrix: %w", err)
	}
	m, err := ParseRiskMatrix(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ParseRiskMatrix 解析并校验矩阵 v2：必须列出 SPRAY；用药类 required_role 必须含 agronomist；required_role 非空、不重复、
// 取值限 farm_manager / agronomist / operator；levels 必须恰好列出 LOW / HIGH，各有正的 approval_ttl，HIGH 必须
// confirm_required；large_area_mu > 0；near_threshold 至少一项，rule_id 非空、裕度在 (0, 1)。
func ParseRiskMatrix(b []byte) (*RiskMatrix, error) {
	var f riskMatrixFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadRiskMatrix, err)
	}
	if f.Version != RiskMatrixVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadRiskMatrix, f.Version, RiskMatrixVersion)
	}
	m := &RiskMatrix{roles: make(map[model.WorkType][]model.Role, len(f.WorkTypes)),
		levels: make(map[model.RiskLevel]levelRule, len(f.Levels)), largeAreaMu: f.Rules.LargeAreaMu,
		near: make(map[string]float64, len(f.Rules.NearThreshold))}
	errs := parseLevels(&f, m)
	for _, wt := range sortedKeys(f.WorkTypes) {
		roles := slices.Clone(f.WorkTypes[wt].RequiredRole)
		if err := checkRoles(wt, roles); err != nil {
			errs = append(errs, err)
			continue
		}
		m.roles[wt] = roles
	}
	for _, wt := range requiredWorkTypes {
		if _, ok := f.WorkTypes[wt]; !ok {
			errs = append(errs, fmt.Errorf("%w: work type %s must be listed", ErrBadRiskMatrix, wt))
		}
	}
	errs = append(errs, parseRules(&f, m)...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// parseLevels 校验 levels：LOW / HIGH 都要有、无未知等级、TTL 为正、HIGH 须确认。
func parseLevels(f *riskMatrixFile, m *RiskMatrix) []error {
	var errs []error
	for _, lv := range sortedKeys(f.Levels) {
		e := f.Levels[lv]
		if !slices.Contains(knownRisks, lv) {
			errs = append(errs, fmt.Errorf("%w: levels: unknown risk level %q", ErrBadRiskMatrix, lv))
			continue
		}
		d, err := time.ParseDuration(e.ApprovalTTL)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%w: levels.%s.approval_ttl %q must be a positive duration", ErrBadRiskMatrix, lv, e.ApprovalTTL))
			continue
		}
		if lv == model.RiskHigh && !e.ConfirmRequired {
			errs = append(errs, fmt.Errorf("%w: levels.HIGH requires confirm_required: true", ErrBadRiskMatrix))
			continue
		}
		m.levels[lv] = levelRule{confirm: e.ConfirmRequired, ttl: d}
	}
	for _, lv := range knownRisks {
		if _, ok := f.Levels[lv]; !ok {
			errs = append(errs, fmt.Errorf("%w: levels.%s must be listed", ErrBadRiskMatrix, lv))
		}
	}
	return errs
}

// parseRules 校验判定参数（面积阈值、临界裕度）。
func parseRules(f *riskMatrixFile, m *RiskMatrix) []error {
	var errs []error
	if !(f.Rules.LargeAreaMu > 0) || math.IsInf(f.Rules.LargeAreaMu, 0) {
		errs = append(errs, fmt.Errorf("%w: rules.large_area_mu %v must be > 0", ErrBadRiskMatrix, f.Rules.LargeAreaMu))
	}
	if len(f.Rules.NearThreshold) == 0 {
		errs = append(errs, fmt.Errorf("%w: rules.near_threshold must list at least one rule_id", ErrBadRiskMatrix))
	}
	for _, id := range sortedKeys(f.Rules.NearThreshold) {
		ratio := f.Rules.NearThreshold[id]
		switch {
		case id == "":
			errs = append(errs, fmt.Errorf("%w: rules.near_threshold: empty rule_id", ErrBadRiskMatrix))
		case !(ratio > 0 && ratio < 1):
			errs = append(errs, fmt.Errorf("%w: rules.near_threshold.%s margin %v must be in (0, 1)", ErrBadRiskMatrix, id, ratio))
		default:
			m.near[id] = ratio
		}
	}
	return errs
}

// checkRoles 校验单个作业类型的可审批角色；用药类必须含 agronomist（§6.6 功能任务 1）。
func checkRoles(wt model.WorkType, roles []model.Role) error {
	var errs []error
	if !slices.Contains(knownWorkTypes, wt) {
		errs = append(errs, fmt.Errorf("%w: unknown work type %q", ErrBadRiskMatrix, wt))
	}
	if len(roles) == 0 {
		errs = append(errs, fmt.Errorf("%w: %s: required_role is empty", ErrBadRiskMatrix, wt))
	}
	if model.IsPesticide(wt) && !slices.Contains(roles, model.RoleAgronomist) {
		errs = append(errs, fmt.Errorf("%w: %s: pesticide work requires agronomist in required_role", ErrBadRiskMatrix, wt))
	}
	seen := make(map[model.Role]bool, len(roles))
	for _, role := range roles {
		switch {
		case !slices.Contains(approverRoles, role):
			errs = append(errs, fmt.Errorf("%w: %s: role %q cannot approve farm work orders", ErrBadRiskMatrix, wt, role))
		case seen[role]:
			errs = append(errs, fmt.Errorf("%w: %s: duplicate role %q", ErrBadRiskMatrix, wt, role))
		}
		seen[role] = true
	}
	return errors.Join(errs...)
}

// Assess 按规则定级：PESTICIDE（用药类）、LARGE_AREA（area_mu ≥ large_area_mu）、LARGE_AREA:unknown（area_mu 缺失 /
// 非正 / 非有限数：矩阵列出的作业类型都是按亩计的田间作业，面积无从核对即失败关闭）、NEAR_THRESHOLD:{rule_id}
// （PRE_APPROVAL 通过行离阈值在裕度内）任一命中 → HIGH，否则 LOW；Roles 为副本。作业类型未列出返回 ErrNoRisk。
func (m *RiskMatrix) Assess(in RiskInput) (Risk, error) {
	roles, ok := m.roles[in.WorkType]
	if !ok {
		return Risk{}, fmt.Errorf("%w: work type %q", ErrNoRisk, in.WorkType)
	}
	var reasons []string
	if model.IsPesticide(in.WorkType) {
		reasons = append(reasons, model.RiskReasonPesticide)
	}
	switch a := in.AreaMu; {
	case a == nil || !(*a > 0) || math.IsInf(*a, 0):
		reasons = append(reasons, model.RiskReasonLargeAreaUnknown)
	case *a >= m.largeAreaMu:
		reasons = append(reasons, model.RiskReasonLargeArea)
	}
	for _, c := range in.Checks {
		if r := model.NearThresholdReason(c.RuleID); m.nearThreshold(c) && !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	level := model.RiskLow
	if len(reasons) > 0 {
		level = model.RiskHigh
	}
	return Risk{Level: level, Roles: slices.Clone(roles), ConfirmRequired: m.levels[level].confirm, Reasons: reasons}, nil
}

// nearThreshold：PRE_APPROVAL 通过行、rule_id 在 near_threshold 中、实测与阈值都是数值、比较符为大小比较，
// 且 |阈值 − 实测| ≤ 裕度 × |阈值|。实测缺失（「不适用」类通过）或无法解析的行不算临界。
func (m *RiskMatrix) nearThreshold(c model.ConstraintCheck) bool {
	ratio, ok := m.near[c.RuleID]
	if !ok || !c.Passed || c.Phase != model.PhasePreApproval {
		return false
	}
	var obs struct {
		Value *float64 `json:"value"`
	}
	var th struct {
		Op    string   `json:"op"`
		Value *float64 `json:"value"`
	}
	if json.Unmarshal(c.Observed, &obs) != nil || json.Unmarshal(c.Threshold, &th) != nil || obs.Value == nil || th.Value == nil {
		return false
	}
	switch th.Op {
	case "<", "<=", ">", ">=":
	default:
		return false
	}
	return math.Abs(*th.Value-*obs.Value) <= ratio*math.Abs(*th.Value)
}

// CheckRuleIDs 交叉校验 rules.near_threshold 的 rule_id 都是已注册的约束规则（known 由约束注册表给出，
// 本包不依赖 guardrail）；未注册的 rule_id 永远不会命中临界，启动期即报 ErrBadRiskMatrix（装配层调用）。
func (m *RiskMatrix) CheckRuleIDs(known func(ruleID string) bool) error {
	var errs []error
	for _, id := range sortedKeys(m.near) {
		if !known(id) {
			errs = append(errs, fmt.Errorf("%w: rules.near_threshold.%s is not a registered constraint rule", ErrBadRiskMatrix, id))
		}
	}
	return errors.Join(errs...)
}

// TTL 返回该风险等级票据 PREPARING → OPEN 后的有效期；未列出返回 ErrNoRisk。
func (m *RiskMatrix) TTL(level model.RiskLevel) (time.Duration, error) {
	l, ok := m.levels[level]
	if !ok {
		return 0, fmt.Errorf("%w: approval_ttl for %q", ErrNoRisk, level)
	}
	return l.ttl, nil
}

// sortedKeys 返回 map 的键（排序），保证错误信息顺序稳定。
func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
