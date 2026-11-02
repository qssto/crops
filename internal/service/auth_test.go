package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"agri-steward/internal/model"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// memRoles 是内存版 RoleStore：bindings 模拟 user_role，farms 模拟 farm_profile（farm_id → tenant_id 列表）。
type memRoles struct {
	bindings []model.UserRole
	farms    map[uint64][]uint64
	err      error
}

func (m memRoles) ListRoles(_ context.Context, tenantID, farmID, userID uint64) ([]model.RoleBinding, error) {
	var out []model.RoleBinding
	for _, b := range m.bindings {
		if b.UserID == userID && (b.TenantID == tenantID && b.FarmID == farmID || b.TenantID == 0 && b.FarmID == 0) {
			out = append(out, model.RoleBinding{TenantID: b.TenantID, FarmID: b.FarmID, Role: b.Role})
		}
	}
	return out, m.err
}

func (m memRoles) FarmHasRole(_ context.Context, tenantID, farmID uint64, role model.Role) (bool, error) {
	return slices.ContainsFunc(m.bindings, func(b model.UserRole) bool {
		return b.TenantID == tenantID && b.FarmID == farmID && b.Role == role
	}), m.err
}

func (m memRoles) FarmTenants(_ context.Context, farmID uint64) ([]uint64, error) {
	return m.farms[farmID], m.err
}

func testRoles() memRoles {
	return memRoles{
		bindings: []model.UserRole{
			{TenantID: 1, FarmID: 1, UserID: 1001, Role: model.RoleFarmManager},
			{TenantID: 1, FarmID: 1, UserID: 1002, Role: model.RoleAgronomist},
			{TenantID: 1, FarmID: 1, UserID: 1002, Role: model.RoleFarmManager},
			{TenantID: 1, FarmID: 2, UserID: 2001, Role: model.RoleFarmManager},
			{TenantID: 0, FarmID: 0, UserID: 9001, Role: model.RolePlatformAdmin},
			{TenantID: 0, FarmID: 0, UserID: 9002, Role: model.RoleKBExpert},
			{TenantID: 0, FarmID: 0, UserID: 9003, Role: model.RoleAgronomist},    // 农场角色挂在 (0,0)：忽略
			{TenantID: 1, FarmID: 1, UserID: 9004, Role: model.RolePlatformAdmin}, // 平台角色挂在农场：忽略
			{TenantID: 1, FarmID: 1, UserID: 9005, Role: "boss"},                  // 未登记角色：忽略
		},
		farms: map[uint64][]uint64{1: {1}, 2: {1}, 3: {2}, 7: {1, 2}, 8: {0}},
	}
}

func newTestAuth(t *testing.T, now time.Time) *AuthService {
	t.Helper()
	a, err := NewAuthService(testSecret, testRoles(), fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestIdentify(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	a := newTestAuth(t, now)
	sign := func(c jwt.Claims) string {
		t.Helper()
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(testSecret))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	issue := func(a *AuthService, uid, tenant uint64, ttl time.Duration) string {
		t.Helper()
		s, err := a.Issue(uid, tenant, ttl)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	other, err := NewAuthService("ffffffffffffffffffffffffffffffff", testRoles(), fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"user_id": 1001, "tenant_id": 1,
		"exp": now.Add(time.Hour).Unix()}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	exp := now.Add(time.Hour).Unix()
	tests := []struct {
		name    string
		token   string
		want    Identity
		wantErr error
	}{
		{name: "ok", token: issue(a, 1001, 1, time.Hour), want: Identity{UserID: 1001, TenantID: 1}},
		{name: "platform tenant 0", token: issue(a, 9001, 0, time.Hour), want: Identity{UserID: 9001}},
		{name: "missing", token: "", wantErr: ErrUnauthenticated},
		{name: "garbage", token: "abc.def.ghi", wantErr: ErrUnauthenticated},
		{name: "wrong secret", token: issue(other, 1001, 1, time.Hour), wantErr: ErrUnauthenticated},
		{name: "expired", token: issue(a, 1001, 1, -time.Minute), wantErr: ErrUnauthenticated},
		{name: "alg none", token: none, wantErr: ErrUnauthenticated},
		{name: "no exp", token: sign(jwt.MapClaims{"user_id": 1001, "tenant_id": 1}), wantErr: ErrUnauthenticated},
		{name: "no tenant claim", token: sign(jwt.MapClaims{"user_id": 1001, "exp": exp}), wantErr: ErrUnauthenticated},
		{name: "no user claim", token: sign(jwt.MapClaims{"tenant_id": 1, "exp": exp}), wantErr: ErrUnauthenticated},
		{name: "P0 sub-only token", token: sign(jwt.MapClaims{"sub": "1001", "exp": exp}), wantErr: ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := a.Identify(tt.token)
			if !errors.Is(err, tt.wantErr) || id != tt.want {
				t.Fatalf("id=%+v err=%v, want %+v %v", id, err, tt.want, tt.wantErr)
			}
			uid, err := a.Authenticate(tt.token)
			if !errors.Is(err, tt.wantErr) || uid != tt.want.UserID {
				t.Fatalf("Authenticate uid=%d err=%v", uid, err)
			}
		})
	}
	if _, err := a.Issue(0, 1, time.Hour); err == nil {
		t.Fatal("Issue user 0 must fail")
	}
	// jwt 原始错误以 %w 保留在链上（便于按过期 / 验签区分排障）
	if _, err := a.Identify(issue(a, 1001, 1, -time.Minute)); !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expired err = %v, want wraps jwt.ErrTokenExpired", err)
	}
}

func TestResolveScope(t *testing.T) {
	a := newTestAuth(t, time.Now().UTC())
	fm, agro, pa := model.RoleFarmManager, model.RoleAgronomist, model.RolePlatformAdmin
	tests := []struct {
		name    string
		uid     uint64
		farm    uint64
		want    model.Scope
		wantErr error
	}{
		{name: "farm binding", uid: 1001, farm: 1, want: model.Scope{TenantID: 1, FarmID: 1, UserID: 1001, Roles: []model.Role{fm}}},
		{name: "multiple roles sorted", uid: 1002, farm: 1,
			want: model.Scope{TenantID: 1, FarmID: 1, UserID: 1002, Roles: []model.Role{fm, agro}}},
		{name: "farm 2 manager", uid: 2001, farm: 2, want: model.Scope{TenantID: 1, FarmID: 2, UserID: 2001, Roles: []model.Role{fm}}},
		{name: "other farm same tenant", uid: 1001, farm: 2, wantErr: ErrForbidden},
		{name: "farm 2 user on farm 1", uid: 2001, farm: 1, wantErr: ErrForbidden},
		{name: "platform admin any farm, tenant from farm_profile", uid: 9001, farm: 3,
			want: model.Scope{TenantID: 2, FarmID: 3, UserID: 9001, Roles: []model.Role{pa}}},
		{name: "kb_expert alone cannot enter farm", uid: 9002, farm: 1, wantErr: ErrForbidden},
		{name: "farm role at (0,0) ignored", uid: 9003, farm: 1, wantErr: ErrForbidden},
		{name: "platform role at farm ignored", uid: 9004, farm: 1, wantErr: ErrForbidden},
		{name: "unknown role ignored", uid: 9005, farm: 1, wantErr: ErrForbidden},
		{name: "unknown farm", uid: 9001, farm: 99, wantErr: ErrForbidden},
		{name: "farm 0", uid: 1001, farm: 0, wantErr: ErrForbidden},
		{name: "user 0", uid: 0, farm: 1, wantErr: ErrForbidden},
		{name: "unknown user", uid: 42, farm: 1, wantErr: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := a.ResolveScope(context.Background(), tt.uid, tt.farm)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (s.TenantID != tt.want.TenantID || s.FarmID != tt.want.FarmID ||
				s.UserID != tt.want.UserID || !slices.Equal(s.Roles, tt.want.Roles)) {
				t.Fatalf("scope = %+v, want %+v", s, tt.want)
			}
		})
	}
}

// 多租户同 farm_id 与存储错误不是 403：返回非 ErrForbidden 的错误（中间件记 500）。
func TestResolveScope_Errors(t *testing.T) {
	a := newTestAuth(t, time.Now().UTC())
	if _, err := a.ResolveScope(context.Background(), 1001, 7); err == nil || errors.Is(err, ErrForbidden) {
		t.Fatalf("ambiguous farm err = %v", err)
	}
	// 归属租户为 0 的农场失败关闭：platform_admin 也不得拿到 tenant_id=0 的农场 Scope
	for _, uid := range []uint64{9001, 1001} {
		if sc, err := a.ResolveScope(context.Background(), uid, 8); err == nil || errors.Is(err, ErrForbidden) {
			t.Fatalf("user %d tenant-0 farm: scope = %+v err = %v, want internal error", uid, sc, err)
		}
	}
	boom := errors.New("db down")
	bad, err := NewAuthService(testSecret, memRoles{err: boom}, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.ResolveScope(context.Background(), 1001, 1); !errors.Is(err, boom) || errors.Is(err, ErrForbidden) {
		t.Fatalf("store err = %v", err)
	}
	if _, err := bad.ResolvePlatformScope(context.Background(), 9001); !errors.Is(err, boom) {
		t.Fatalf("platform store err = %v", err)
	}
	if _, err := bad.FarmHasRole(context.Background(), 1, 1, model.RoleAgronomist); !errors.Is(err, boom) {
		t.Fatalf("FarmHasRole err = %v", err)
	}
}

func TestNewAuthService(t *testing.T) {
	if _, err := NewAuthService("short", testRoles(), fixedClock{}); !errors.Is(err, ErrWeakSecret) {
		t.Fatalf("weak secret err = %v", err)
	}
	if _, err := NewAuthService(testSecret, nil, fixedClock{}); err == nil {
		t.Fatal("nil role store must fail")
	}
	if _, err := NewAuthService(testSecret, testRoles(), nil); err == nil {
		t.Fatal("nil clock must fail")
	}
}

func TestScopeContext(t *testing.T) {
	if _, ok := ScopeFrom(context.Background()); ok {
		t.Fatal("empty ctx must have no scope")
	}
	ctx := WithScope(context.Background(), model.Scope{FarmID: 1})
	if s, ok := ScopeFrom(ctx); !ok || s.FarmID != 1 {
		t.Fatalf("scope = %+v %v", s, ok)
	}
}

func TestResolvePlatformScope(t *testing.T) {
	a := newTestAuth(t, time.Now().UTC())
	tests := []struct {
		name      string
		uid       uint64
		wantRoles []model.Role
		wantErr   error
	}{
		{name: "platform admin", uid: 9001, wantRoles: []model.Role{model.RolePlatformAdmin}},
		{name: "kb_expert", uid: 9002, wantRoles: []model.Role{model.RoleKBExpert}},
		{name: "farm user", uid: 1001, wantErr: ErrForbidden},
		{name: "farm role at (0,0) ignored", uid: 9003, wantErr: ErrForbidden},
		{name: "unknown user", uid: 42, wantErr: ErrForbidden},
		{name: "user 0", uid: 0, wantErr: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := a.ResolvePlatformScope(context.Background(), tt.uid)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (s.TenantID != 0 || s.FarmID != 0 || s.UserID != tt.uid || !slices.Equal(s.Roles, tt.wantRoles)) {
				t.Fatalf("scope = %+v", s)
			}
		})
	}
}

func TestFarmHasRole(t *testing.T) {
	a := newTestAuth(t, time.Now().UTC())
	tests := []struct {
		name         string
		tenant, farm uint64
		role         model.Role
		want         bool
	}{
		{name: "bound", tenant: 1, farm: 1, role: model.RoleAgronomist, want: true},
		{name: "role not bound", tenant: 1, farm: 1, role: model.RoleAuditor},
		{name: "other farm", tenant: 1, farm: 2, role: model.RoleAgronomist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.FarmHasRole(context.Background(), tt.tenant, tt.farm, tt.role)
			if err != nil || got != tt.want {
				t.Fatalf("got %v err %v, want %v", got, err, tt.want)
			}
		})
	}
}
