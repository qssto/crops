package main

import (
	"context"
	"fmt"
	"slices"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/seed"
	"agri-steward/internal/service"
)

// seedUsers 是种子主农场上的角色绑定：角色 → 按 user_id 升序的用户（取自种子 user_roles，P1 起替代 configs/auth.yaml）。
// 样本 context.user（u-fm-01 / u-agro-01 / u-op-01）按 context.role 取该角色 user_id 最小的用户，即 1001 / 1002 / 1003。
type seedUsers map[model.Role][]uint64

func newSeedUsers(d *seed.Data) seedUsers {
	u := seedUsers{}
	for _, b := range d.UserRoles {
		if b.TenantID == d.Farm.TenantID && b.FarmID == d.Farm.FarmID {
			u[b.Role] = append(u[b.Role], b.UserID)
		}
	}
	for _, ids := range u {
		slices.Sort(ids)
	}
	return u
}

// rolesOf 返回用户在主农场上的全部角色（按登记顺序，与身份解析一致）。
func (u seedUsers) rolesOf(userID uint64) []model.Role {
	var out []model.Role
	for _, r := range service.Roles {
		if slices.Contains(u[r], userID) {
			out = append(out, r)
		}
	}
	return out
}

// scriptApprovers 是脚本审批人：主农场 user_id 最小的两位农技员（种子中的 1002 与 1004），第一审批人与 HIGH 票二次确认人。
func scriptApprovers(u seedUsers) (first, second env.Identity, err error) {
	agro := u[model.RoleAgronomist]
	if len(agro) < 2 {
		return first, second, fmt.Errorf("eval: seed needs 2 agronomists on the primary farm, got %d", len(agro))
	}
	return env.Identity{UserID: agro[0], Roles: u.rolesOf(agro[0])}, env.Identity{UserID: agro[1], Roles: u.rolesOf(agro[1])}, nil
}

// seedQuestion 按种子主农场把样本情境解析为 Scope / FieldID：user 取 context.role 在种子中 user_id 最小的绑定，
// Roles 取该用户在主农场上的全部绑定；field 编码在本 trial schema 中查 field_profile.id。
func seedQuestion(tenantID, farmID uint64, users seedUsers) evalkit.QuestionFunc {
	return func(ctx context.Context, a *app.App, s *sample.Sample) (agent.Question, error) {
		q := evalkit.QuestionOf(s)
		ids := users[model.Role(s.Context.Role)]
		if len(ids) == 0 {
			return q, fmt.Errorf("sample %s: context.user %q: no seed user_roles binding for role %q",
				s.ID, s.Context.User, s.Context.Role)
		}
		fieldID, err := repository.ProfileRepo{}.FieldIDByCode(ctx, a.DB, tenantID, farmID, s.Context.Field)
		if err != nil {
			return q, fmt.Errorf("sample %s: %w", s.ID, err)
		}
		q.Scope = model.Scope{TenantID: tenantID, FarmID: farmID, UserID: ids[0], Roles: users.rolesOf(ids[0])}
		q.FieldID = fieldID
		return q, nil
	}
}
