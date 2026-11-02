//go:build integration

package repository_test

import (
	"slices"
	"testing"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1-L1-A：user_role 三条查询与 upsert 幂等（种子 configs/seed/fields.yaml 的 user_roles）。
func TestUserRoleRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	repo := repository.UserRoleRepo{DB: slot.DB}

	listTests := []struct {
		name               string
		tenant, farm, user uint64
		want               []model.RoleBinding
	}{
		{name: "farm binding", tenant: 1, farm: 1, user: 1001,
			want: []model.RoleBinding{{TenantID: 1, FarmID: 1, Role: model.RoleFarmManager}}},
		{name: "other farm has none", tenant: 1, farm: 2, user: 1001},
		{name: "platform binding at any farm", tenant: 1, farm: 2, user: 9001,
			want: []model.RoleBinding{{TenantID: 0, FarmID: 0, Role: model.RolePlatformAdmin}}},
		{name: "farm 2 manager", tenant: 1, farm: 2, user: 2001,
			want: []model.RoleBinding{{TenantID: 1, FarmID: 2, Role: model.RoleFarmManager}}},
		{name: "unknown user", tenant: 1, farm: 1, user: 4242},
	}
	for _, tt := range listTests {
		t.Run("ListRoles/"+tt.name, func(t *testing.T) {
			got, err := repo.ListRoles(t.Context(), tt.tenant, tt.farm, tt.user)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("ListRoles = %+v, want %+v", got, tt.want)
			}
		})
	}

	hasTests := []struct {
		farm uint64
		role model.Role
		want bool
	}{
		{farm: 1, role: model.RoleAgronomist, want: true},
		{farm: 2, role: model.RoleAgronomist},
		{farm: 2, role: model.RoleFarmManager, want: true},
	}
	for _, tt := range hasTests {
		got, err := repo.FarmHasRole(t.Context(), 1, tt.farm, tt.role)
		if err != nil || got != tt.want {
			t.Fatalf("FarmHasRole(1,%d,%s) = %v, %v; want %v", tt.farm, tt.role, got, err, tt.want)
		}
	}

	for farm, want := range map[uint64][]uint64{1: {1}, 2: {1}, 99: nil} {
		got, err := repo.FarmTenants(t.Context(), farm)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("FarmTenants(%d) = %v, %v; want %v", farm, got, err, want)
		}
	}

	// 同一绑定重复 upsert 只保留一行，granted_by 取最新值
	ur := model.UserRole{TenantID: 1, FarmID: 1, UserID: 1001, Role: model.RoleFarmManager, GrantedBy: 7}
	for range 2 {
		if err := slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
			return repo.UpsertUserRoleTx(t.Context(), tx, ur, t0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM user_role WHERE tenant_id=1 AND farm_id=1 AND user_id=1001
  AND role='farm_manager' AND granted_by=7`); n != 1 {
		t.Fatalf("upserted rows = %d, want 1", n)
	}
	if err := repo.UpsertUserRoleTx(t.Context(), slot.DB, ur, t0); err == nil {
		t.Fatal("upsert outside tx must fail")
	}
}
