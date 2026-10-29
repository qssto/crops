package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
)

// RiskMatrixVersion 是本包支持的 risk_matrix.yaml 版本。
const RiskMatrixVersion = 1

var (
	// ErrBadRiskMatrix 是 risk_matrix.yaml 不合法（加载期，启动失败）。
	ErrBadRiskMatrix = errors.New("policy: invalid risk_matrix")
	// ErrNoRisk 是作业类型或风险等级未在矩阵中列出（开票期，不默认放行）。
	ErrNoRisk = errors.New("policy: not listed in risk_matrix")
)

// Risk 是一种作业类型的审批要求。
type Risk struct {
	Level           model.RiskLevel
	Roles           []model.Role // 可审批角色，非空
	ConfirmRequired bool         // HIGH 必为 true：需第二审批人确认
}

// RiskMatrix 是 policy/risk_matrix.yaml。构造后只读，并发安全。
type RiskMatrix struct {
	work map[model.WorkType]Risk
	ttl  map[model.RiskLevel]time.Duration
}

type riskMatrixFile struct {
	Version   int `yaml:"version"`
	WorkTypes map[model.WorkType]struct {
		RiskLevel       model.RiskLevel `yaml:"risk_level"`
		RequiredRole    []model.Role    `yaml:"required_role"`
		ConfirmRequired bool            `yaml:"confirm_required"`
	} `yaml:"work_types"`
	ApprovalTTL map[model.RiskLevel]string `yaml:"approval_ttl"`
}

// 可作为农场作业单审批人的角色（platform_admin 不审批）。
var approverRoles = []model.Role{model.RoleFarmManager, model.RoleAgronomist, model.RoleOperator}

// 已知作业类型与风险等级；requiredWorkTypes 是矩阵必须列出的作业类型（P0 定版 SPRAY 一律 HIGH，缺失即启动失败）。
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

// ParseRiskMatrix 解析并校验矩阵：必须列出 SPRAY，且 SPRAY 必须 HIGH 且需确认；HIGH 必须需确认；required_role 非空、不重复、
// 取值限 farm_manager / agronomist / operator；出现的风险等级都要有正的 approval_ttl。
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
	m := &RiskMatrix{work: make(map[model.WorkType]Risk, len(f.WorkTypes)), ttl: make(map[model.RiskLevel]time.Duration, len(f.ApprovalTTL))}
	var errs []error
	for _, lv := range sortedKeys(f.ApprovalTTL) {
		if !slices.Contains(knownRisks, lv) {
			errs = append(errs, fmt.Errorf("%w: approval_ttl: unknown risk level %q", ErrBadRiskMatrix, lv))
			continue
		}
		d, err := time.ParseDuration(f.ApprovalTTL[lv])
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%w: approval_ttl.%s %q must be a positive duration", ErrBadRiskMatrix, lv, f.ApprovalTTL[lv]))
			continue
		}
		m.ttl[lv] = d
	}
	for _, wt := range sortedKeys(f.WorkTypes) {
		e := f.WorkTypes[wt]
		r := Risk{Level: e.RiskLevel, Roles: slices.Clone(e.RequiredRole), ConfirmRequired: e.ConfirmRequired}
		if err := checkRisk(wt, r); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, ok := m.ttl[r.Level]; !ok {
			errs = append(errs, fmt.Errorf("%w: %s: no approval_ttl for %s", ErrBadRiskMatrix, wt, r.Level))
			continue
		}
		m.work[wt] = r
	}
	for _, wt := range requiredWorkTypes {
		if _, ok := f.WorkTypes[wt]; !ok {
			errs = append(errs, fmt.Errorf("%w: work type %s must be listed", ErrBadRiskMatrix, wt))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// checkRisk 校验单个作业类型条目。
func checkRisk(wt model.WorkType, r Risk) error {
	var errs []error
	if !slices.Contains(knownWorkTypes, wt) {
		errs = append(errs, fmt.Errorf("%w: unknown work type %q", ErrBadRiskMatrix, wt))
	}
	if !slices.Contains(knownRisks, r.Level) {
		errs = append(errs, fmt.Errorf("%w: %s: risk_level %q, want LOW / HIGH", ErrBadRiskMatrix, wt, r.Level))
	}
	if wt == model.WorkSpray && r.Level != model.RiskHigh {
		errs = append(errs, fmt.Errorf("%w: SPRAY must be HIGH, got %q", ErrBadRiskMatrix, r.Level))
	}
	if r.Level == model.RiskHigh && !r.ConfirmRequired {
		errs = append(errs, fmt.Errorf("%w: %s: HIGH requires confirm_required: true", ErrBadRiskMatrix, wt))
	}
	if len(r.Roles) == 0 {
		errs = append(errs, fmt.Errorf("%w: %s: required_role is empty", ErrBadRiskMatrix, wt))
	}
	seen := make(map[model.Role]bool, len(r.Roles))
	for _, role := range r.Roles {
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

// For 返回作业类型的审批要求（Roles 为副本）；未列出返回 ErrNoRisk。
func (m *RiskMatrix) For(wt model.WorkType) (Risk, error) {
	r, ok := m.work[wt]
	if !ok {
		return Risk{}, fmt.Errorf("%w: work type %q", ErrNoRisk, wt)
	}
	r.Roles = slices.Clone(r.Roles)
	return r, nil
}

// TTL 返回该风险等级票据 PREPARING → OPEN 后的有效期；未列出返回 ErrNoRisk。
func (m *RiskMatrix) TTL(level model.RiskLevel) (time.Duration, error) {
	d, ok := m.ttl[level]
	if !ok {
		return 0, fmt.Errorf("%w: approval_ttl for %q", ErrNoRisk, level)
	}
	return d, nil
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
