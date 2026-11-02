package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/seed"
)

func repoSeedUsers(t *testing.T) seedUsers {
	t.Helper()
	d, err := seed.Load("../../configs/seed/fields.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return newSeedUsers(d)
}

// 解析成功路径需要 trial schema（FieldIDByCode 查库），由 run 集成链路覆盖；此处只测失败路径。
func TestSeedQuestion_UnknownRole(t *testing.T) {
	s := &sample.Sample{ID: "x", Context: sample.Context{Farm: "farm-demo-01", Field: "field-001", Role: "boss", User: "u-nobody"}}
	_, err := seedQuestion(1, 1, repoSeedUsers(t))(context.Background(), &app.App{}, s)
	if err == nil || !strings.Contains(err.Error(), `no seed user_roles binding for role "boss"`) {
		t.Fatalf("err = %v", err)
	}
}

// 样本用户与审批人取自种子 user_roles（原 configs/auth.yaml 的 1001 / 1002 / 1003 / 1004），农场 2 的绑定不混入。
func TestSeedUsers_RepoSeed(t *testing.T) {
	u := repoSeedUsers(t)
	for role, want := range map[model.Role]uint64{model.RoleFarmManager: 1001, model.RoleAgronomist: 1002, model.RoleOperator: 1003} {
		if got := u[role]; len(got) == 0 || got[0] != want {
			t.Fatalf("%s users = %v, want first %d", role, got, want)
		}
	}
	if slices.Contains(u[model.RoleFarmManager], 2001) || len(u[model.RolePlatformAdmin]) != 0 {
		t.Fatalf("primary farm users leak other bindings: %v", u)
	}
	first, second, err := scriptApprovers(u)
	if err != nil {
		t.Fatal(err)
	}
	agro := []model.Role{model.RoleAgronomist}
	if first.UserID != 1002 || second.UserID != 1004 || !slices.Equal(first.Roles, agro) || !slices.Equal(second.Roles, agro) {
		t.Fatalf("approvers = %+v, %+v", first, second)
	}
	if _, _, err := scriptApprovers(seedUsers{model.RoleAgronomist: {1002}}); err == nil {
		t.Fatal("want error with one agronomist")
	}
}
