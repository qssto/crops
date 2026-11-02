package service

import (
	"errors"
	"slices"
	"testing"

	"agri-steward/internal/model"
)

// testPerms 是仓库内的权限矩阵（审批 / 原文读取等用例与生产同一份）。
func testPerms(t *testing.T) *PermissionMatrix {
	t.Helper()
	m, err := LoadPermissionMatrix("../../policy/permission_matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 红线：policy/permission_matrix.yaml 的每个「角色 × 操作」都与期望表逐格比对；期望表未列出的组合必须拒绝。
func TestPermissionMatrix_RepoFile_AllRolesAllOps(t *testing.T) {
	m := testPerms(t)
	allowed := map[model.Role][]Operation{
		model.RoleFarmer:      {OpAsk, OpAdviceRead},
		model.RoleFarmManager: {OpAsk, OpAdviceRead, OpApprovalSubmit, OpApprovalDecideOther, OpOrderCancel, OpRawRead, OpOrderReconcile},
		model.RoleAgronomist: {OpAsk, OpAdviceRead, OpApprovalSubmit, OpApprovalDecidePesticide, OpApprovalDecideOther,
			OpOrderCancel, OpRawRead},
		model.RoleOperator:      {OpAsk, OpAdviceRead, OpOrderCancel},
		model.RoleAdmin:         {OpAsk, OpAdviceRead, OpOrderCancel, OpAuditExport},
		model.RoleAuditor:       {OpAdviceRead, OpAuditExport, OpRawRead},
		model.RolePlatformAdmin: {OpAdviceRead, OpOrderCancel, OpAuditExport, OpRawRead, OpFlagWrite, OpOrderReconcile},
		model.RoleKBExpert:      nil,
		model.RoleLegal:         nil,
	}
	if len(allowed) != len(Roles) {
		t.Fatalf("expected table covers %d roles, registry has %d", len(allowed), len(Roles))
	}
	for _, role := range Roles {
		for _, op := range Operations {
			want := slices.Contains(allowed[role], op)
			t.Run(string(role)+"/"+string(op), func(t *testing.T) {
				if got := m.Allows(op, role); got != want {
					t.Fatalf("Allows(%s, %s) = %v, want %v", op, role, got, want)
				}
			})
		}
	}
}

// 应失败用例：未声明的组合（未登记角色 / 未登记操作 / 文件未列出的角色 / 无角色 / nil 矩阵）一律拒绝。
func TestPermissionMatrix_DefaultDeny(t *testing.T) {
	m, err := ParsePermissionMatrix([]byte("version: 1\nroles:\n  agronomist: [ask]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var nilMatrix *PermissionMatrix
	tests := []struct {
		name  string
		m     *PermissionMatrix
		op    Operation
		roles []model.Role
		want  bool
	}{
		{name: "declared", m: m, op: OpAsk, roles: []model.Role{model.RoleAgronomist}, want: true},
		{name: "any role grants", m: m, op: OpAsk, roles: []model.Role{model.RoleOperator, model.RoleAgronomist}, want: true},
		{name: "op not declared for role", m: m, op: OpRawRead, roles: []model.Role{model.RoleAgronomist}},
		{name: "role not in file", m: m, op: OpAsk, roles: []model.Role{model.RoleFarmManager}},
		{name: "unregistered role", m: m, op: OpAsk, roles: []model.Role{"boss"}},
		{name: "unregistered op", m: m, op: "order.delete", roles: []model.Role{model.RoleAgronomist}},
		{name: "no roles", m: m, op: OpAsk},
		{name: "nil matrix", m: nilMatrix, op: OpAsk, roles: []model.Role{model.RoleAgronomist}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Allows(tt.op, tt.roles...); got != tt.want {
				t.Fatalf("Allows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePermissionMatrix_Invalid(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong version", body: "version: 2\nroles: {}\n"},
		{name: "missing version", body: "roles: {}\n"},
		{name: "unknown field", body: "version: 1\nroles: {}\nextra: 1\n"},
		{name: "unknown role", body: "version: 1\nroles:\n  boss: [ask]\n"},
		{name: "unknown op", body: "version: 1\nroles:\n  agronomist: [ask, order.delete]\n"},
		{name: "duplicate op", body: "version: 1\nroles:\n  agronomist: [ask, ask]\n"},
		{name: "duplicate role key", body: "version: 1\nroles:\n  agronomist: [ask]\n  agronomist: [raw.read]\n"},
		{name: "flag.write on farm role", body: "version: 1\nroles:\n  farm_manager: [flag.write]\n"},
		{name: "flag.write on kb_expert", body: "version: 1\nroles:\n  kb_expert: [flag.write]\n"},
		{name: "flag.write on legal", body: "version: 1\nroles:\n  legal: [flag.write]\n"},
		{name: "not yaml", body: "version: [\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePermissionMatrix([]byte(tt.body)); !errors.Is(err, ErrBadPermissionMatrix) {
				t.Fatalf("err = %v, want ErrBadPermissionMatrix", err)
			}
		})
	}
	if _, err := ParsePermissionMatrix([]byte("version: 1\nroles:\n  platform_admin: [flag.write]\n")); err != nil {
		t.Fatalf("flag.write on platform_admin: %v", err)
	}
	if _, err := LoadPermissionMatrix("no-such-file.yaml"); err == nil {
		t.Fatal("missing file must fail")
	}
}
