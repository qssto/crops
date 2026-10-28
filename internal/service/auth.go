package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// 最小鉴权（母文档 §4.6.2-1）：HS256 token 的 sub 为 user_id，按静态授权表解析出路径农场上的 Scope。

const minSecretBytes = 32

var (
	// ErrUnauthenticated 表示无 token、验签失败或已过期（→ 401）。
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden 表示路径 farm_id 不在用户授权内（→ 403）。
	ErrForbidden = errors.New("forbidden")
	// ErrWeakSecret 表示 HS256 密钥过短。
	ErrWeakSecret = fmt.Errorf("auth: jwt secret must be >= %d bytes", minSecretBytes)
)

// AuthService 签发与校验 token，并解析 Scope。
type AuthService struct {
	secret []byte
	grants map[uint64][]model.Grant
	clock  clock.Clock
}

// NewAuthService 构造 AuthService；grants 来自 configs/auth.yaml。
func NewAuthService(secret string, grants map[uint64][]model.Grant, c clock.Clock) (*AuthService, error) {
	if len(secret) < minSecretBytes {
		return nil, ErrWeakSecret
	}
	return &AuthService{secret: []byte(secret), grants: grants, clock: c}, nil
}

// Issue 为 userID 签发 token（dev 工具与测试使用）。
func (s *AuthService) Issue(userID uint64, ttl time.Duration) (string, error) {
	now := s.clock.Now()
	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatUint(userID, 10),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return tok, nil
}

// Authenticate 校验 token（只接受 HS256、必须带 exp），返回 user_id。
func (s *AuthService) Authenticate(token string) (uint64, error) {
	if token == "" {
		return 0, fmt.Errorf("%w: missing token", ErrUnauthenticated)
	}
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.clock.Now))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	uid, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil || uid == 0 {
		return 0, fmt.Errorf("%w: bad subject", ErrUnauthenticated)
	}
	return uid, nil
}

// ResolveScope 返回用户在路径农场上的 Scope；平台授权 (0,0) 不匹配任何农场路径。
func (s *AuthService) ResolveScope(userID, farmID uint64) (model.Scope, error) {
	if farmID == 0 {
		return model.Scope{}, fmt.Errorf("%w: farm_id 0", ErrForbidden)
	}
	for _, g := range s.grants[userID] {
		if g.FarmID == farmID {
			return model.Scope{TenantID: g.TenantID, FarmID: g.FarmID, UserID: userID,
				Roles: append([]model.Role(nil), g.Roles...)}, nil
		}
	}
	return model.Scope{}, fmt.Errorf("%w: user %d farm %d", ErrForbidden, userID, farmID)
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
