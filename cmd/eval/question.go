package main

import (
	"context"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// evalUsers 是样本 context.user → user_id（与 configs/auth.yaml 注释一致）。
var evalUsers = map[string]uint64{"u-fm-01": 1001, "u-agro-01": 1002, "u-op-01": 1003}

// scriptApprovers 是脚本审批人的两位农技员（configs/auth.yaml 的 1002 与 1004）：第一审批人与 HIGH 票二次确认人。
func scriptApprovers() (first, second env.Identity) {
	agro := []model.Role{model.RoleAgronomist}
	return env.Identity{UserID: 1002, Roles: agro}, env.Identity{UserID: 1004, Roles: agro}
}

// seedQuestion 按种子农场把样本情境解析为 Scope / FieldID：user 查 evalUsers，role 取 context.role，
// field 编码在本 trial schema 中查 field_profile.id。
func seedQuestion(tenantID, farmID uint64) evalkit.QuestionFunc {
	return func(ctx context.Context, a *app.App, s *sample.Sample) (agent.Question, error) {
		q := evalkit.QuestionOf(s)
		userID, ok := evalUsers[s.Context.User]
		if !ok {
			return q, fmt.Errorf("sample %s: unknown context.user %q", s.ID, s.Context.User)
		}
		fieldID, err := repository.ProfileRepo{}.FieldIDByCode(ctx, a.DB, tenantID, farmID, s.Context.Field)
		if err != nil {
			return q, fmt.Errorf("sample %s: %w", s.ID, err)
		}
		q.Scope = model.Scope{TenantID: tenantID, FarmID: farmID, UserID: userID, Roles: []model.Role{model.Role(s.Context.Role)}}
		q.FieldID = fieldID
		return q, nil
	}
}
