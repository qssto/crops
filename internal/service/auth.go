package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// 鉴权（母文档 §4.6.2-1，P1 §6.2 流程 1、2）：HS256 token 带 user_id / tenant_id，
// 按 user_role 解析路径农场上的 Scope；每次请求查唯一索引、不缓存。

const minSecretBytes = 32

var (
	// ErrUnauthenticated 表示无 token、验签失败或已过期（→ 401）。
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden 表示路径 farm_id 不在用户授权内，或资源不在 Scope 内（→ 403，不暴露存在性）。
	ErrForbidden = errors.New("forbidden")
	// ErrNotInScope 是 ErrForbidden 的一种：按 Scope 查资源本身未命中（不存在或属于他场，→ 403 + RESOURCE_NOT_IN_SCOPE 审计）。
	// 只由 notInScope 在资源归属查询处产生；本农场资源存在但关联数据缺失属于完整性错误，不得用它。
	ErrNotInScope = fmt.Errorf("%w: resource not in scope", ErrForbidden)
	// ErrWeakSecret 表示 HS256 密钥过短。
	ErrWeakSecret = fmt.Errorf("auth: jwt secret must be >= %d bytes", minSecretBytes)
)

// RoleStore 是身份解析需要的 user_role / farm_profile 读能力，repository.UserRoleRepo 实现。
type RoleStore interface {
	// ListRoles 返回用户在 (tenant_id, farm_id) 与 (0,0) 上的绑定（P1 §6.2 定版 SQL）。
	ListRoles(ctx context.Context, tenantID, farmID, userID uint64) ([]model.RoleBinding, error)
	FarmHasRole(ctx context.Context, tenantID, farmID uint64, role model.Role) (bool, error)
	// FarmTenants 返回 farm_id 的归属租户（最多 2 个）。
	FarmTenants(ctx context.Context, farmID uint64) ([]uint64, error)
}

// Identity 是 token 中的身份 claims；平台身份的 tenant_id 为 0。
type Identity struct {
	UserID   uint64
	TenantID uint64
}

// tokenClaims 是 token 载荷；tenant_id 必填（指针区分缺失与 0）。
type tokenClaims struct {
	UserID   uint64  `json:"user_id"`
	TenantID *uint64 `json:"tenant_id"`
	jwt.RegisteredClaims
}

// AuthService 签发与校验 token，并按 user_role 解析 Scope。
type AuthService struct {
	secret []byte
	roles  RoleStore
	clock  clock.Clock
}

// NewAuthService 构造 AuthService；roles 为 repository.UserRoleRepo。
func NewAuthService(secret string, roles RoleStore, c clock.Clock) (*AuthService, error) {
	if len(secret) < minSecretBytes {
		return nil, ErrWeakSecret
	}
	if roles == nil || c == nil {
		return nil, errors.New("auth: role store and clock are required")
	}
	return &AuthService{secret: []byte(secret), roles: roles, clock: c}, nil
}

// Issue 为 (userID, tenantID) 签发 token（dev 工具与测试使用）；平台身份 tenantID 传 0。
func (s *AuthService) Issue(userID, tenantID uint64, ttl time.Duration) (string, error) {
	if userID == 0 {
		return "", errors.New("auth: user_id required")
	}
	now := s.clock.Now()
	claims := tokenClaims{UserID: userID, TenantID: &tenantID, RegisteredClaims: jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return tok, nil
}

// Identify 校验 token（只接受 HS256、必须带 exp、user_id 非 0、带 tenant_id），返回身份。
func (s *AuthService) Identify(token string) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("%w: missing token", ErrUnauthenticated)
	}
	var claims tokenClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.clock.Now))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	if claims.UserID == 0 || claims.TenantID == nil {
		return Identity{}, fmt.Errorf("%w: user_id / tenant_id claim required", ErrUnauthenticated)
	}
	return Identity{UserID: claims.UserID, TenantID: *claims.TenantID}, nil
}

// Authenticate 校验 token，返回 user_id（controller.Authenticator 冻结接缝）。
func (s *AuthService) Authenticate(token string) (uint64, error) {
	id, err := s.Identify(token)
	if err != nil {
		return 0, err
	}
	return id.UserID, nil
}

// ResolveScope 返回用户在路径农场上的 Scope：tenant_id 取 farm_profile 中的真实归属，
// 再查 (tenant_id, farm_id, user_id) 与 (0,0,user_id) 的绑定。有农场绑定，或在 (0,0) 有 platform_admin 才放行；
// Roles = 农场角色 ∪ 平台角色。农场不存在与无绑定一样返回 ErrForbidden（不暴露存在性）。
func (s *AuthService) ResolveScope(ctx context.Context, userID, farmID uint64) (model.Scope, error) {
	if userID == 0 || farmID == 0 {
		return model.Scope{}, fmt.Errorf("%w: user %d farm %d", ErrForbidden, userID, farmID)
	}
	tenants, err := s.roles.FarmTenants(ctx, farmID)
	if err != nil {
		return model.Scope{}, fmt.Errorf("auth: resolve scope: %w", err)
	}
	switch len(tenants) {
	case 0:
		return model.Scope{}, fmt.Errorf("%w: user %d farm %d", ErrForbidden, userID, farmID)
	case 1:
	default:
		return model.Scope{}, fmt.Errorf("auth: farm %d belongs to multiple tenants", farmID)
	}
	tenantID := tenants[0]
	if tenantID == 0 { // farm_profile 归属租户为 0 是数据错误：失败关闭（→ 500），不得落成平台 Scope
		return model.Scope{}, fmt.Errorf("auth: farm %d has tenant_id 0", farmID)
	}
	bindings, err := s.roles.ListRoles(ctx, tenantID, farmID, userID)
	if err != nil {
		return model.Scope{}, fmt.Errorf("auth: resolve scope: %w", err)
	}
	farmRoles := rolesAt(bindings, tenantID, farmID, false)
	platformRoles := rolesAt(bindings, 0, 0, true)
	if len(farmRoles) == 0 && !slices.Contains(platformRoles, model.RolePlatformAdmin) {
		return model.Scope{}, fmt.Errorf("%w: user %d farm %d", ErrForbidden, userID, farmID)
	}
	return model.Scope{TenantID: tenantID, FarmID: farmID, UserID: userID,
		Roles: sortRoles(append(farmRoles, platformRoles...))}, nil
}

// ResolvePlatformScope 返回用户的平台 Scope（tenant_id=farm_id=0），用于 /api/v1/admin/...；
// 没有 (0,0) 平台角色绑定返回 ErrForbidden。
func (s *AuthService) ResolvePlatformScope(ctx context.Context, userID uint64) (model.Scope, error) {
	if userID == 0 {
		return model.Scope{}, fmt.Errorf("%w: user 0", ErrForbidden)
	}
	bindings, err := s.roles.ListRoles(ctx, 0, 0, userID)
	if err != nil {
		return model.Scope{}, fmt.Errorf("auth: resolve platform scope: %w", err)
	}
	roles := rolesAt(bindings, 0, 0, true)
	if len(roles) == 0 {
		return model.Scope{}, fmt.Errorf("%w: user %d has no platform grant", ErrForbidden, userID)
	}
	return model.Scope{UserID: userID, Roles: sortRoles(roles)}, nil
}

// rolesAt 取 (tenantID, farmID) 上的已登记角色；platform 为 true 时只取平台角色，否则只取农场角色
// （绑定位置与角色类别不符的行一律忽略：农场角色挂在 (0,0) 不能放行任一农场）。
func rolesAt(bindings []model.RoleBinding, tenantID, farmID uint64, platform bool) []model.Role {
	var out []model.Role
	for _, b := range bindings {
		if b.TenantID != tenantID || b.FarmID != farmID || !b.Role.Valid() || b.Role.Platform() != platform {
			continue
		}
		if !slices.Contains(out, b.Role) {
			out = append(out, b.Role)
		}
	}
	return out
}

// sortRoles 按 Roles 的登记顺序排序（SQL 无 ORDER BY；审批取第一个匹配角色，须确定）。
func sortRoles(roles []model.Role) []model.Role {
	slices.SortFunc(roles, func(a, b model.Role) int { return slices.Index(Roles, a) - slices.Index(Roles, b) })
	return roles
}

// notInScope 把资源归属查询（按 Scope 查 run / ticket 等资源本身）的 model.ErrNotFound 标记为 ErrNotInScope；其余错误原样返回。
func notInScope(err error) error {
	if errors.Is(err, model.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotInScope, err)
	}
	return err
}

// FarmRoleChecker 判断农场是否有某角色的绑定（L0：enable_robot_dispatch 置开要求有 agronomist）。
type FarmRoleChecker interface {
	FarmHasRole(ctx context.Context, tenantID, farmID uint64, role model.Role) (bool, error)
}

// FarmHasRole 实现 FarmRoleChecker（查 user_role 的 idx_farm_role）。
func (s *AuthService) FarmHasRole(ctx context.Context, tenantID, farmID uint64, role model.Role) (bool, error) {
	ok, err := s.roles.FarmHasRole(ctx, tenantID, farmID, role)
	if err != nil {
		return false, fmt.Errorf("auth: farm has role: %w", err)
	}
	return ok, nil
}

type scopeKey struct{}

// WithScope 把 Scope 放入 ctx（controller 中间件调用）。
func WithScope(ctx context.Context, s model.Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom 从 ctx 取 Scope；service 内一律以它为准，忽略请求体中的 farm_id。
func ScopeFrom(ctx context.Context) (model.Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(model.Scope)
	return s, ok
}
