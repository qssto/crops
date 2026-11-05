package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

const testSecret = "0123456789abcdef0123456789abcdef"

var testNow = time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// testRoleStore 是内存版 service.RoleStore：绑定同种子 user_roles，另加审计员 3001 与租户 2 的农场 3。
type testRoleStore struct{ err error }

var testBindings = []model.UserRole{
	{TenantID: 1, FarmID: 1, UserID: 1001, Role: model.RoleFarmManager},
	{TenantID: 1, FarmID: 1, UserID: 1002, Role: model.RoleAgronomist},
	{TenantID: 1, FarmID: 1, UserID: 1003, Role: model.RoleOperator},
	{TenantID: 1, FarmID: 1, UserID: 1004, Role: model.RoleAgronomist},
	{TenantID: 1, FarmID: 1, UserID: 3001, Role: model.RoleAuditor},
	{TenantID: 0, FarmID: 0, UserID: 9001, Role: model.RolePlatformAdmin},
	{TenantID: 1, FarmID: 2, UserID: 2001, Role: model.RoleFarmManager},
}

var testFarms = map[uint64][]uint64{1: {1}, 2: {1}, 3: {2}}

func (s testRoleStore) ListRoles(_ context.Context, tenantID, farmID, userID uint64) ([]model.RoleBinding, error) {
	var out []model.RoleBinding
	for _, b := range testBindings {
		if b.UserID == userID && (b.TenantID == tenantID && b.FarmID == farmID || b.TenantID == 0 && b.FarmID == 0) {
			out = append(out, model.RoleBinding{TenantID: b.TenantID, FarmID: b.FarmID, Role: b.Role})
		}
	}
	return out, s.err
}

func (s testRoleStore) FarmHasRole(_ context.Context, tenantID, farmID uint64, role model.Role) (bool, error) {
	return slices.ContainsFunc(testBindings, func(b model.UserRole) bool {
		return b.TenantID == tenantID && b.FarmID == farmID && b.Role == role
	}), s.err
}

func (s testRoleStore) FarmTenants(_ context.Context, farmID uint64) ([]uint64, error) {
	return testFarms[farmID], s.err
}

// recDenier 记录 Deny 调用。
type recDenier struct {
	mu  sync.Mutex
	got []service.AccessDenial
}

func (r *recDenier) Deny(_ context.Context, d service.AccessDenial) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, d)
}

func (r *recDenier) codes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.got))
	for _, d := range r.got {
		out = append(out, d.Code)
	}
	return out
}

// testEnv 是控制器测试的鉴权依赖：真实 AuthService（内存 user_role）+ 仓库权限矩阵 + 记录型 Denier。
type testEnv struct {
	auth  *service.AuthService
	den   *recDenier
	guard Guard
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, testRoleStore{})
}

func newTestEnvWith(t *testing.T, store service.RoleStore) *testEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	auth, err := service.NewAuthService(testSecret, store, fixedClock{testNow})
	if err != nil {
		t.Fatal(err)
	}
	perms, err := service.LoadPermissionMatrix("../../policy/permission_matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	den := &recDenier{}
	return &testEnv{auth: auth, den: den, guard: Guard{Auth: auth, Perms: perms, Denials: den}}
}

// token 签发 (uid, tenant) 的 token。
func (e *testEnv) token(t *testing.T, uid, tenant uint64) string {
	t.Helper()
	tok, err := e.auth.Issue(uid, tenant, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *testEnv) bearer(t *testing.T, uid, tenant uint64) string {
	return "Bearer " + e.token(t, uid, tenant)
}

// allMounts 挂上全部业务路由（handler 依赖为空桩）。
func allMounts() []Mount {
	return []Mount{ApprovalRoutes(&fakeDecider{}), RunRoutes(&fakeRunDetailer{}, &fakeMetrics{}),
		EventRoutes(&fakeStreamer{}), AuditRoutes(&fakeRawReader{}), OrderOpsRoutes(&fakeOrderOps{}),
		ApprovalDetailRoutes(&fakeDetailer{}), RejectionRoutes(&fakeExporter{}), AuditExportRoutes(&fakeAuditExporter{}),
		AlertRoutes(&fakeAlertLister{}), FeedbackRoutes(&fakeFeedback{})}
}

// routeOps 与实际路由双向核对：每条 farms 路由都登记了所需操作，登记表里没有多余项。
func TestRouteOps_MatchRouter(t *testing.T) {
	e := newTestEnv(t)
	r := NewRouter(e.guard, &fakeAsker{}, allMounts()...)
	var routes []string
	for _, ri := range r.Routes() {
		if strings.HasPrefix(ri.Path, farmsPrefix) {
			routes = append(routes, ri.Method+" "+ri.Path)
		}
	}
	var declared []string
	for k, ops := range routeOps {
		declared = append(declared, k)
		if len(ops) == 0 {
			t.Errorf("route %s declares no operation", k)
		}
	}
	sort.Strings(routes)
	sort.Strings(declared)
	if !slices.Equal(routes, declared) {
		t.Fatalf("router routes:\n%s\nrouteOps:\n%s", strings.Join(routes, "\n"), strings.Join(declared, "\n"))
	}
}

// 未在 routeOps 登记的路由一律拒绝（默认拒绝）；RequireOps 按固定操作判定。
func TestAuthorize_DefaultDenyAndRequireOps(t *testing.T) {
	e := newTestEnv(t)
	unregistered := func(g *gin.RouterGroup) { g.GET("/unregistered", Ping) }
	r := NewRouter(e.guard, &fakeAsker{}, unregistered)
	admin := r.Group("/admin", func(c *gin.Context) {
		scope, err := e.auth.ResolvePlatformScope(c.Request.Context(), 9001)
		if err != nil {
			t.Error(err)
		}
		if c.Query("as") == "farm" {
			scope = model.Scope{UserID: 1001, Roles: []model.Role{model.RoleFarmManager}}
		}
		c.Request = c.Request.WithContext(service.WithScope(c.Request.Context(), scope))
	}, RequireOps(e.guard, service.OpFlagWrite))
	admin.GET("/flags", Ping)
	tests := []struct {
		name, path, auth string
		want             int
		wantCode         string
	}{
		{name: "unregistered route", path: "/api/v1/farms/1/unregistered", auth: e.bearer(t, 1001, 1),
			want: http.StatusForbidden, wantCode: service.DenyRole},
		{name: "unregistered route platform admin", path: "/api/v1/farms/1/unregistered", auth: e.bearer(t, 9001, 0),
			want: http.StatusForbidden, wantCode: service.DenyRole},
		{name: "flag.write platform admin", path: "/admin/flags", want: http.StatusOK},
		{name: "flag.write farm role", path: "/admin/flags?as=farm", want: http.StatusForbidden, wantCode: service.DenyRole},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.wantCode != "" && (!strings.Contains(rec.Body.String(), tt.wantCode) ||
				!slices.Equal(e.den.codes(), []string{tt.wantCode})) {
				t.Fatalf("body=%s denials=%v", rec.Body.String(), e.den.codes())
			}
		})
	}
}

// signClaims 用测试密钥签任意 claims（构造缺字段的 token）。
func signClaims(t *testing.T, c jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// abortScoped：归属未命中 → 403 并记越界；关联数据缺失（裸 ErrNotFound）→ 500 记日志、不记越界；
// 取不到 Guard（装配错误）时仍回 403，但必须记 Error 日志而不是静默跳过审计。
func TestAbortScoped(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newTestEnv(t)
	scope := model.Scope{TenantID: 1, FarmID: 1, UserID: 1001}
	tests := []struct {
		name       string
		err        error
		withGuard  bool
		wantStatus int
		wantDenies int
		wantLog    string
	}{
		{name: "not in scope", err: fmt.Errorf("x: %w", service.ErrNotInScope), withGuard: true,
			wantStatus: http.StatusForbidden, wantDenies: 1},
		{name: "related data missing", err: fmt.Errorf("x: %w", model.ErrNotFound), withGuard: true,
			wantStatus: http.StatusInternalServerError, wantLog: "related data missing"},
		{name: "other error", err: errors.New("db down"), withGuard: true,
			wantStatus: http.StatusInternalServerError, wantLog: "db down"},
		{name: "guard missing still 403 but logged", err: service.ErrNotInScope,
			wantStatus: http.StatusForbidden, wantLog: "guard missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got, logBuf = nil, bytes.Buffer{}
			r := gin.New()
			r.GET("/x", func(c *gin.Context) {
				if tt.withGuard {
					c.Set(guardKey, e.guard)
				}
				c.Request = c.Request.WithContext(service.WithScope(c.Request.Context(), scope))
				abortScoped(c, tt.err, "run:9")
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
			if rec.Code != tt.wantStatus || len(e.den.got) != tt.wantDenies {
				t.Fatalf("status = %d denials = %+v", rec.Code, e.den.got)
			}
			if tt.wantDenies == 1 && (e.den.got[0].Code != service.DenyResource || e.den.got[0].Target != "run:9") {
				t.Fatalf("denial = %+v", e.den.got[0])
			}
			if !strings.Contains(logBuf.String(), tt.wantLog) {
				t.Fatalf("log = %s, want %q", logBuf.String(), tt.wantLog)
			}
		})
	}
}
