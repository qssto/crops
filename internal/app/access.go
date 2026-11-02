package app

import (
	"errors"
	"fmt"

	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1-L1-A 装配：身份解析（user_role）、权限矩阵、拒绝审计。

// ErrMissingPermissions 表示未注入权限矩阵（policy/permission_matrix.yaml，service.LoadPermissionMatrix）。
var ErrMissingPermissions = errors.New("app: deps.Permissions is required (service.LoadPermissionMatrix)")

// buildAccess 装配拒绝审计；须在 HTTP 路由与审批 / 原文读取服务之前。
func (a *App) buildAccess(deps Deps) error {
	den, err := service.NewDenialAuditor(repository.AuditRepo{DB: a.DB}, a.Clock, a.Logger, deps.DenialCounter)
	if err != nil {
		return fmt.Errorf("app: build denial auditor: %w", err)
	}
	a.Perms, a.Denials = deps.Permissions, den
	return nil
}

// NewAuth 构造按 user_role 解析 Scope 的 AuthService（同时是 L0 的 service.FarmRoleChecker）。
func (a *App) NewAuth(secret string) (*service.AuthService, error) {
	auth, err := service.NewAuthService(secret, repository.UserRoleRepo{DB: a.DB}, a.Clock)
	if err != nil {
		return nil, fmt.Errorf("app: build auth: %w", err)
	}
	return auth, nil
}

// guard 是 HTTP 鉴权中间件的依赖（L0 的 admin 路由组可复用：controller.RequireOps(guard, service.OpFlagWrite)）。
func (a *App) guard(auth *service.AuthService) controller.Guard {
	return controller.Guard{Auth: auth, Perms: a.Perms, Denials: a.Denials, Logger: a.Logger}
}
