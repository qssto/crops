package policy

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
)

func TestLoadRiskMatrix_Repo(t *testing.T) {
	m, err := LoadRiskMatrix(repoPolicy(t, "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	small := 10.0
	spray, err := m.Assess(RiskInput{WorkType: model.WorkSpray, AreaMu: &small})
	if err != nil {
		t.Fatal(err)
	}
	if spray.Level != model.RiskHigh || !spray.ConfirmRequired || !slices.Equal(spray.Roles, []model.Role{model.RoleAgronomist}) ||
		!slices.Equal(spray.Reasons, []string{model.RiskReasonPesticide}) {
		t.Fatalf("SPRAY = %+v", spray)
	}
	if d, err := m.TTL(model.RiskHigh); err != nil || d != 2*time.Hour {
		t.Fatalf("TTL(HIGH) = %v, %v", d, err)
	}
	if d, err := m.TTL(model.RiskLow); err != nil || d != 6*time.Hour {
		t.Fatalf("TTL(LOW) = %v, %v", d, err)
	}
	if _, err := m.Assess(RiskInput{WorkType: "PRUNE"}); !errors.Is(err, ErrNoRisk) {
		t.Fatalf("unlisted work type: err = %v", err)
	}
	if _, err := m.TTL("MID"); !errors.Is(err, ErrNoRisk) {
		t.Fatalf("unlisted level: err = %v", err)
	}
	spray.Roles[0] = model.RoleOperator
	if again, _ := m.Assess(RiskInput{WorkType: model.WorkSpray, AreaMu: &small}); again.Roles[0] != model.RoleAgronomist {
		t.Fatal("Assess must return a copy of Roles")
	}
}

// riskSrc 是合法的 v2 矩阵；各用例替换其中一段触发失败。
const (
	riskHead = "version: 2\n"
	riskWork = "work_types:\n  SPRAY: {required_role: [agronomist]}\n  FERTILIZE: {required_role: [agronomist, farm_manager]}\n" +
		"  IRRIGATE: {required_role: [farm_manager, agronomist]}\n"
	riskRules  = "rules:\n  large_area_mu: 50\n  near_threshold: {wind_speed_max: 0.2}\n"
	riskLevels = "levels:\n  HIGH: {confirm_required: true, approval_ttl: 2h}\n  LOW: {confirm_required: false, approval_ttl: 6h}\n"
)

func TestParseRiskMatrix_Valid(t *testing.T) {
	if _, err := ParseRiskMatrix([]byte(riskHead + riskWork + riskRules + riskLevels)); err != nil {
		t.Fatal(err)
	}
}

func TestParseRiskMatrix_Invalid(t *testing.T) {
	work := func(lines string) string { return "work_types:\n  SPRAY: {required_role: [agronomist]}\n" + lines }
	tests := []struct{ name, src, want string }{
		{"v1 file", "version: 1\n" + riskWork + riskRules + riskLevels, "version 1"},
		{"unknown field", riskHead + riskWork + riskRules + riskLevels + "extra: 1\n", "decode"},
		{"v1 risk_level field", riskHead + "work_types:\n  SPRAY: {risk_level: HIGH, required_role: [agronomist]}\n" + riskRules + riskLevels, "decode"},
		{"pesticide without agronomist", riskHead + "work_types:\n  SPRAY: {required_role: [farm_manager]}\n" + riskRules + riskLevels,
			"pesticide work requires agronomist"},
		{"spray missing", riskHead + "work_types:\n  IRRIGATE: {required_role: [farm_manager]}\n" + riskRules + riskLevels, "SPRAY must be listed"},
		{"high no confirm", riskHead + riskWork + riskRules + "levels:\n  HIGH: {confirm_required: false, approval_ttl: 2h}\n  LOW: {approval_ttl: 6h}\n",
			"HIGH requires confirm_required"},
		{"unknown work type", riskHead + work("  PRUNE: {required_role: [agronomist]}\n") + riskRules + riskLevels, "unknown work type"},
		{"empty roles", riskHead + work("  IRRIGATE: {required_role: []}\n") + riskRules + riskLevels, "required_role is empty"},
		{"platform admin", riskHead + work("  IRRIGATE: {required_role: [platform_admin]}\n") + riskRules + riskLevels, "cannot approve"},
		{"duplicate role", riskHead + work("  IRRIGATE: {required_role: [agronomist, agronomist]}\n") + riskRules + riskLevels, "duplicate role"},
		{"missing LOW", riskHead + riskWork + riskRules + "levels:\n  HIGH: {confirm_required: true, approval_ttl: 2h}\n", "levels.LOW must be listed"},
		{"unknown level", riskHead + riskWork + riskRules + riskLevels + "  MID: {approval_ttl: 1h}\n", "unknown risk level"},
		{"bad ttl", riskHead + riskWork + riskRules + "levels:\n  HIGH: {confirm_required: true, approval_ttl: soon}\n  LOW: {approval_ttl: 6h}\n",
			"positive duration"},
		{"zero ttl", riskHead + riskWork + riskRules + "levels:\n  HIGH: {confirm_required: true, approval_ttl: 2h}\n  LOW: {approval_ttl: 0s}\n",
			"positive duration"},
		{"no area threshold", riskHead + riskWork + "rules:\n  near_threshold: {wind_speed_max: 0.2}\n" + riskLevels, "large_area_mu"},
		{"negative area threshold", riskHead + riskWork + "rules:\n  large_area_mu: -1\n  near_threshold: {wind_speed_max: 0.2}\n" + riskLevels,
			"large_area_mu"},
		{"no near threshold", riskHead + riskWork + "rules:\n  large_area_mu: 50\n" + riskLevels, "near_threshold must list"},
		{"margin too big", riskHead + riskWork + "rules:\n  large_area_mu: 50\n  near_threshold: {wind_speed_max: 1}\n" + riskLevels, "must be in (0, 1)"},
		{"margin zero", riskHead + riskWork + "rules:\n  large_area_mu: 50\n  near_threshold: {wind_speed_max: 0}\n" + riskLevels, "must be in (0, 1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRiskMatrix([]byte(tt.src))
			if !errors.Is(err, ErrBadRiskMatrix) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrBadRiskMatrix containing %q", err, tt.want)
			}
		})
	}
}

// TestRiskMatrix_CheckRuleIDs（P1-L5 审查修复 11）：near_threshold 列出的 rule_id 须全部为已注册规则。
func TestRiskMatrix_CheckRuleIDs(t *testing.T) {
	m, err := ParseRiskMatrix([]byte(riskHead + riskWork + "rules:\n  large_area_mu: 50\n  near_threshold: {wind_speed_max: 0.2, temp: 0.1}\n" +
		riskLevels))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		known []string
		want  []string // 期望错误里出现的 rule_id；空表示通过
	}{
		{"all registered", []string{"wind_speed_max", "temp"}, nil},
		{"one unknown", []string{"wind_speed_max"}, []string{"near_threshold.temp"}},
		{"both unknown", nil, []string{"near_threshold.temp", "near_threshold.wind_speed_max"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := m.CheckRuleIDs(func(id string) bool { return slices.Contains(tt.known, id) })
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrBadRiskMatrix) {
				t.Fatalf("err = %v, want ErrBadRiskMatrix", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("err = %v, want it to mention %q", err, w)
				}
			}
		})
	}
}

// check 构造一条 PRE_APPROVAL 校验行。
func check(rule string, passed bool, observed any, op string, threshold float64) model.ConstraintCheck {
	obs, _ := json.Marshal(map[string]any{"value": observed, "source": "mock"})
	th, _ := json.Marshal(map[string]any{"kb": "kb:x@v1", "op": op, "value": threshold})
	return model.ConstraintCheck{Phase: model.PhasePreApproval, RuleID: rule, Passed: passed, Observed: obs, Threshold: th}
}

func TestRiskMatrix_Assess(t *testing.T) {
	m, err := ParseRiskMatrix([]byte(riskHead + riskWork + riskRules + riskLevels))
	if err != nil {
		t.Fatal(err)
	}
	area := func(v float64) *float64 { return &v }
	near := check("wind_speed_max", true, 2.5, "<=", 3) // |3 − 2.5| = 0.5 ≤ 0.6
	far := check("wind_speed_max", true, 1.0, "<=", 3)  // 2.0 > 0.6
	edge := check("wind_speed_max", true, 2.4, "<=", 3) // 0.6 = 裕度，算临界
	failed := check("wind_speed_max", false, 2.9, "<=", 3)
	unlisted := check("temp_max", true, 34, "<=", 35) // 未列入 near_threshold
	nilValue := check("wind_speed_max", true, nil, "<=", 3)
	dispatchPhase := near
	dispatchPhase.Phase = model.PhasePreDispatch
	nearReason := model.NearThresholdReason("wind_speed_max")
	unknown := model.RiskReasonLargeAreaUnknown
	ten := area(10)
	tests := []struct {
		name   string
		in     RiskInput
		level  model.RiskLevel
		reason []string
	}{
		{"none hit", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{far}}, model.RiskLow, nil},
		{"irrigate small area", RiskInput{WorkType: model.WorkIrrigate, AreaMu: ten}, model.RiskLow, nil},
		// 面积未知失败关闭：缺失 / 0 / 负数 / NaN / Inf 都记 LARGE_AREA:unknown → HIGH
		{"fertilize no area", RiskInput{WorkType: model.WorkFertilize}, model.RiskHigh, []string{unknown}},
		{"irrigate no area", RiskInput{WorkType: model.WorkIrrigate}, model.RiskHigh, []string{unknown}},
		{"fertilize zero area", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(0)}, model.RiskHigh, []string{unknown}},
		{"fertilize negative area", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(-5)}, model.RiskHigh, []string{unknown}},
		{"fertilize NaN area", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(math.NaN())}, model.RiskHigh, []string{unknown}},
		{"fertilize Inf area", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(math.Inf(1))}, model.RiskHigh, []string{unknown}},
		{"spray no area", RiskInput{WorkType: model.WorkSpray}, model.RiskHigh, []string{model.RiskReasonPesticide, unknown}},
		{"no area + near", RiskInput{WorkType: model.WorkFertilize, Checks: []model.ConstraintCheck{near}}, model.RiskHigh,
			[]string{unknown, nearReason}},
		{"pesticide only", RiskInput{WorkType: model.WorkSpray, AreaMu: ten, Checks: []model.ConstraintCheck{far}}, model.RiskHigh,
			[]string{model.RiskReasonPesticide}},
		{"large area only", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(50)}, model.RiskHigh, []string{model.RiskReasonLargeArea}},
		{"irrigate large area", RiskInput{WorkType: model.WorkIrrigate, AreaMu: area(60)}, model.RiskHigh, []string{model.RiskReasonLargeArea}},
		{"area just below", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(49.99)}, model.RiskLow, nil},
		{"near threshold only", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{near}}, model.RiskHigh,
			[]string{nearReason}},
		{"near threshold at margin", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{edge}}, model.RiskHigh,
			[]string{nearReason}},
		{"all three in order", RiskInput{WorkType: model.WorkSpray, AreaMu: area(80), Checks: []model.ConstraintCheck{near, near}},
			model.RiskHigh, []string{model.RiskReasonPesticide, model.RiskReasonLargeArea, nearReason}},
		{"area + near", RiskInput{WorkType: model.WorkFertilize, AreaMu: area(60), Checks: []model.ConstraintCheck{near}}, model.RiskHigh,
			[]string{model.RiskReasonLargeArea, nearReason}},
		{"failed check is not near", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{failed}}, model.RiskLow, nil},
		{"unlisted rule is not near", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{unlisted}}, model.RiskLow, nil},
		{"nil observed is not near", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{nilValue}}, model.RiskLow, nil},
		{"pre_dispatch row ignored", RiskInput{WorkType: model.WorkFertilize, AreaMu: ten, Checks: []model.ConstraintCheck{dispatchPhase}}, model.RiskLow, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := m.Assess(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if r.Level != tt.level || !slices.Equal(r.Reasons, tt.reason) {
				t.Fatalf("Assess = %s %v, want %s %v", r.Level, r.Reasons, tt.level, tt.reason)
			}
			if r.ConfirmRequired != (tt.level == model.RiskHigh) {
				t.Fatalf("confirm_required = %v for %s", r.ConfirmRequired, r.Level)
			}
		})
	}
}
