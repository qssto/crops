package policy

import (
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// repoPolicy 返回仓库根 policy/ 下文件的路径。
func repoPolicy(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "policy", name)
}

const testPolicy = `version: 1
tools:
  - {name: w.read, level: read}
  - {name: w.write, level: write}
  - {name: dev.dispatch, level: actuate}
`

func mustParse(t *testing.T, s string) *ToolPolicy {
	t.Helper()
	tp, err := ParseToolPolicy([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func TestParseToolPolicy_Invalid(t *testing.T) {
	tests := []struct{ name, src string }{
		{"bad version", "version: 2\ntools:\n  - {name: a, level: read}\n"},
		{"unknown field", "version: 1\ntools:\n  - {name: a, level: read, extra: 1}\n"},
		{"empty name", "version: 1\ntools:\n  - {name: '', level: read}\n"},
		{"bad level", "version: 1\ntools:\n  - {name: a, level: admin}\n"},
		{"missing level", "version: 1\ntools:\n  - {name: a}\n"},
		{"duplicate", "version: 1\ntools:\n  - {name: a, level: read}\n  - {name: a, level: read}\n"},
		{"no tools", "version: 1\ntools: []\n"},
		{"not yaml", "{{"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseToolPolicy([]byte(tt.src)); !errors.Is(err, ErrBadToolPolicy) {
				t.Fatalf("err = %v, want ErrBadToolPolicy", err)
			}
		})
	}
}

func TestToolPolicy_Check(t *testing.T) {
	tp := mustParse(t, testPolicy)
	ok := []Spec{{Tool: "w.read", Level: LevelRead}, {Tool: "w.write", Level: LevelWrite}}
	tests := []struct {
		name     string
		registry []Spec
		want     []error
	}{
		{"ok", ok, nil},
		{"undeclared tool", append(slices.Clone(ok), Spec{Tool: "w.new", Level: LevelRead}), []error{ErrLevelUndeclared}},
		{"registry level empty", []Spec{{Tool: "w.read"}, {Tool: "w.write", Level: LevelWrite}}, []error{ErrLevelUndeclared}},
		{"level mismatch", []Spec{{Tool: "w.read", Level: LevelWrite}, {Tool: "w.write", Level: LevelWrite}}, []error{ErrLevelMismatch}},
		{"actuate visible", append(slices.Clone(ok), Spec{Tool: "dev.dispatch", Level: LevelActuate}), []error{ErrActuateVisible}},
		{"actuate visible with read level", append(slices.Clone(ok), Spec{Tool: "dev.dispatch", Level: LevelRead}), []error{ErrActuateVisible, ErrLevelMismatch}},
		{"stale policy", ok[:1], []error{ErrPolicyStale}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tp.Check(tt.registry)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			for _, w := range tt.want {
				if !errors.Is(err, w) {
					t.Errorf("err = %v, want %v", err, w)
				}
			}
		})
	}
}

func TestToolPolicy_Bind(t *testing.T) {
	tp := mustParse(t, testPolicy)
	specs := []Spec{{Tool: "w.read", Level: LevelRead, Args: needField{}}, {Tool: "w.write", Level: LevelWrite, Args: needField{}}}

	if _, err := tp.Bind(specs[:1], LevelRead); !errors.Is(err, ErrPolicyStale) {
		t.Fatalf("stale: err = %v", err)
	}
	if _, err := tp.Bind(specs, LevelRead, LevelActuate); !errors.Is(err, ErrBadToolPolicy) {
		t.Fatalf("allow actuate: err = %v", err)
	}
	p, err := tp.Bind(specs, LevelRead)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, tool string
		want       []error
	}{
		{"read passes", "w.read", nil},
		{"write denied", "w.write", []error{ErrLevelDenied}},
		{"reserved actuate denied and flagged", "dev.dispatch", []error{ErrActuateDenied, ErrLevelDenied}},
		{"unknown tool", "dev.nope", []error{ErrUnknownTool}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.Before(tt.tool, []byte(`{"field_id":1}`))
			if len(tt.want) == 0 && err != nil {
				t.Fatalf("err = %v", err)
			}
			for _, w := range tt.want {
				if !errors.Is(err, w) {
					t.Errorf("err = %v, want %v", err, w)
				}
			}
			if tt.tool != "dev.dispatch" && errors.Is(err, ErrActuateDenied) {
				t.Errorf("non-actuate tool flagged as actuate: %v", err)
			}
		})
	}
}

// 即使不经 Bind，注册为 actuate 的工具也兜底拒绝（allowed 里放了 actuate 也一样）。
func TestBefore_RegisteredActuateAlwaysDenied(t *testing.T) {
	p, err := New([]Spec{{Tool: "dev.x", Level: LevelActuate, Args: needField{}}}, LevelRead, LevelActuate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Before("dev.x", []byte(`{"field_id":1}`)); !errors.Is(err, ErrActuateDenied) {
		t.Fatalf("err = %v, want ErrActuateDenied", err)
	}
}

func TestLoad_RepoToolPolicy(t *testing.T) {
	tp, err := Load(repoPolicy(t, "tool_policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tp.Actuate(), []string{"device.dispatch", "device.dry_run"}; !slices.Equal(got, want) {
		t.Fatalf("Actuate() = %v, want %v", got, want)
	}
	for _, name := range []string{"weather.realtime", "crop.threshold", "field.soil", "device.status"} {
		if l, ok := tp.Level(name); !ok || l != LevelRead {
			t.Errorf("%s level = %q, %v; want read", name, l, ok)
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want error")
	}
	if _, err := LoadRiskMatrix(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want error")
	}
}

func TestLoadRiskMatrix_Repo(t *testing.T) {
	m, err := LoadRiskMatrix(repoPolicy(t, "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spray, err := m.For(model.WorkSpray)
	if err != nil {
		t.Fatal(err)
	}
	if spray.Level != model.RiskHigh || !spray.ConfirmRequired || !slices.Equal(spray.Roles, []model.Role{model.RoleAgronomist}) {
		t.Fatalf("SPRAY = %+v", spray)
	}
	if d, err := m.TTL(model.RiskHigh); err != nil || d != 2*time.Hour {
		t.Fatalf("TTL(HIGH) = %v, %v", d, err)
	}
	if _, err := m.For("PRUNE"); !errors.Is(err, ErrNoRisk) {
		t.Fatalf("unlisted work type: err = %v", err)
	}
	if _, err := m.TTL("MID"); !errors.Is(err, ErrNoRisk) {
		t.Fatalf("unlisted level: err = %v", err)
	}
	spray.Roles[0] = model.RoleOperator
	if again, _ := m.For(model.WorkSpray); again.Roles[0] != model.RoleAgronomist {
		t.Fatal("For must return a copy of Roles")
	}
}

func TestParseRiskMatrix_Invalid(t *testing.T) {
	const (
		ttl   = "approval_ttl: {HIGH: 2h, LOW: 6h}\n"
		head  = "version: 1\nwork_types:\n"
		spray = "  SPRAY: {risk_level: HIGH, required_role: [agronomist], confirm_required: true}\n"
	)
	// with 是带合法 SPRAY 的矩阵，只让 line 的问题触发失败。
	with := func(line string) string { return head + spray + line + ttl }
	tests := []struct{ name, src, want string }{
		{"bad version", "version: 2\nwork_types:\n" + spray + ttl, "version 2"},
		{"unknown field", with("  IRRIGATE: {risk_level: LOW, required_role: [farm_manager], confirm_required: false, x: 1}\n"), "decode"},
		{"spray low", head + "  SPRAY: {risk_level: LOW, required_role: [agronomist], confirm_required: true}\n" + ttl, "SPRAY must be HIGH"},
		{"spray no confirm", head + "  SPRAY: {risk_level: HIGH, required_role: [agronomist], confirm_required: false}\n" + ttl, "HIGH requires confirm_required"},
		{"spray missing", head + "  IRRIGATE: {risk_level: LOW, required_role: [farm_manager], confirm_required: false}\n" + ttl, "SPRAY must be listed"},
		{"high no confirm", with("  IRRIGATE: {risk_level: HIGH, required_role: [agronomist], confirm_required: false}\n"), "HIGH requires confirm_required"},
		{"empty roles", with("  IRRIGATE: {risk_level: LOW, required_role: [], confirm_required: false}\n"), "required_role is empty"},
		{"platform admin", with("  IRRIGATE: {risk_level: LOW, required_role: [platform_admin], confirm_required: false}\n"), "cannot approve"},
		{"duplicate role", with("  IRRIGATE: {risk_level: LOW, required_role: [agronomist, agronomist], confirm_required: false}\n"), "duplicate role"},
		{"unknown work type", with("  PRUNE: {risk_level: LOW, required_role: [agronomist], confirm_required: false}\n"), "unknown work type"},
		{"unknown risk", with("  IRRIGATE: {risk_level: MID, required_role: [agronomist], confirm_required: false}\n"), "risk_level \"MID\""},
		{"missing ttl", head + spray + "  IRRIGATE: {risk_level: LOW, required_role: [agronomist], confirm_required: false}\napproval_ttl: {HIGH: 2h}\n", "no approval_ttl for LOW"},
		{"bad ttl", head + spray + "approval_ttl: {HIGH: soon}\n", "positive duration"},
		{"zero ttl", head + spray + "approval_ttl: {HIGH: 0s}\n", "positive duration"},
		{"ttl unknown level", head + spray + "approval_ttl: {HIGH: 1h, MID: 1h}\n", "unknown risk level"},
		{"no work types", "version: 1\nwork_types: {}\n" + ttl, "SPRAY must be listed"},
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
