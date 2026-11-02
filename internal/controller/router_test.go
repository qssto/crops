package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

func TestFarmAuth(t *testing.T) {
	e := newTestEnv(t)
	forger, err := service.NewAuthService(strings.Repeat("x", 32), testRoleStore{}, fixedClock{testNow})
	if err != nil {
		t.Fatal(err)
	}
	forged, err := forger.Issue(1001, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	exp := testNow.Add(time.Hour).Unix()
	router := NewRouter(e.guard, &fakeAsker{})

	tests := []struct {
		name       string
		path       string
		header     string
		body       string
		wantStatus int
		wantScope  model.Scope // 200 时校验 tenant / farm / user
		wantDeny   bool        // 403 SCOPE_DENIED 并记一次拒绝
		wantOwner  uint64      // 拒绝审计的 owner_tenant_id（只在租户不符时填）
	}{
		{name: "ok", path: "/api/v1/farms/1/ping", header: e.bearer(t, 1001, 1), wantStatus: http.StatusOK,
			wantScope: model.Scope{TenantID: 1, FarmID: 1, UserID: 1001}},
		{name: "no token", path: "/api/v1/farms/1/ping", wantStatus: http.StatusUnauthorized},
		{name: "not bearer", path: "/api/v1/farms/1/ping", header: e.token(t, 1001, 1), wantStatus: http.StatusUnauthorized},
		{name: "bad signature", path: "/api/v1/farms/1/ping", header: "Bearer " + forged, wantStatus: http.StatusUnauthorized},
		{name: "missing tenant claim", path: "/api/v1/farms/1/ping",
			header: "Bearer " + signClaims(t, jwt.MapClaims{"user_id": 1001, "exp": exp}), wantStatus: http.StatusUnauthorized},
		{name: "farm not in scope", path: "/api/v1/farms/2/ping", header: e.bearer(t, 1001, 1),
			wantStatus: http.StatusForbidden, wantDeny: true},
		{name: "tenant mismatch", path: "/api/v1/farms/1/ping", header: e.bearer(t, 1001, 2),
			wantStatus: http.StatusForbidden, wantDeny: true, wantOwner: 1},
		{name: "unknown farm", path: "/api/v1/farms/99/ping", header: e.bearer(t, 1001, 1),
			wantStatus: http.StatusForbidden, wantDeny: true},
		{name: "platform admin any farm", path: "/api/v1/farms/3/ping", header: e.bearer(t, 9001, 0),
			wantStatus: http.StatusOK, wantScope: model.Scope{TenantID: 2, FarmID: 3, UserID: 9001}},
		{name: "body farm_id ignored", path: "/api/v1/farms/2/ping", header: e.bearer(t, 1001, 1),
			body: `{"farm_id":1}`, wantStatus: http.StatusForbidden, wantDeny: true},
		{name: "bad farm_id", path: "/api/v1/farms/abc/ping", header: e.bearer(t, 1001, 1), wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			req := httptest.NewRequest(http.MethodGet, tt.path, strings.NewReader(tt.body))
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantDeny {
				if !strings.Contains(rec.Body.String(), service.DenyScope) || len(e.den.got) != 1 {
					t.Fatalf("body=%s denials=%+v", rec.Body.String(), e.den.got)
				}
				d, seg := e.den.got[0], strings.Split(tt.path, "/")[4]
				farmID, err := strconv.ParseUint(seg, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				// 平台级事件：Scope 未建立，审计落在 (0,0)
				if d.Code != service.DenyScope || d.Scope.TenantID != 0 || d.Scope.FarmID != 0 || d.UserID != 1001 ||
					d.FarmID != farmID || d.Target != "farm:"+seg || d.OwnerTenantID != tt.wantOwner {
					t.Fatalf("denial = %+v", d)
				}
				return
			}
			if len(e.den.got) != 0 {
				t.Fatalf("unexpected denials %+v", e.den.got)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			var got model.Scope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			w := tt.wantScope
			if got.TenantID != w.TenantID || got.FarmID != w.FarmID || got.UserID != w.UserID || len(got.Roles) == 0 {
				t.Fatalf("scope = %+v", got)
			}
		})
	}
}

// 解析 Scope 时存储出错 → 500，不当作越权。
func TestFarmAuth_StoreError(t *testing.T) {
	e := newTestEnvWith(t, testRoleStore{err: errors.New("db down")})
	router := NewRouter(e.guard, &fakeAsker{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/farms/1/ping", nil)
	req.Header.Set("Authorization", e.bearer(t, 1001, 1))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || len(e.den.got) != 0 {
		t.Fatalf("status = %d denials=%+v", rec.Code, e.den.got)
	}
}

// 角色未授予路由所需操作 → 403 ROLE_INSUFFICIENT，拒绝记录带操作名与当前 Scope。
func TestAuthorize_RoleInsufficient(t *testing.T) {
	e := newTestEnv(t)
	router := NewRouter(e.guard, &fakeAsker{}, allMounts()...)
	tests := []struct {
		name, method, path string
		uid                uint64
		wantStatus         int
		wantOp             service.Operation
	}{
		{name: "operator raw.read", method: http.MethodGet, path: "/api/v1/farms/1/runs/9/raw/101", uid: 1003,
			wantStatus: http.StatusForbidden, wantOp: service.OpRawRead},
		{name: "auditor ask", method: http.MethodPost, path: "/api/v1/farms/1/ask", uid: 3001,
			wantStatus: http.StatusForbidden, wantOp: service.OpAsk},
		{name: "operator decide", method: http.MethodPost, path: "/api/v1/farms/1/approvals/7/decide", uid: 1003,
			wantStatus: http.StatusForbidden, wantOp: service.OpApprovalDecidePesticide + "," + service.OpApprovalDecideOther},
		{name: "auditor raw.read ok", method: http.MethodGet, path: "/api/v1/farms/1/runs/9/raw/101", uid: 3001,
			wantStatus: http.StatusOK},
		{name: "operator advice.read ok", method: http.MethodGet, path: "/api/v1/farms/1/approvals", uid: 1003,
			wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{"decision":"APPROVE"}`))
			req.Header.Set("Authorization", e.bearer(t, tt.uid, 1))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantOp == "" {
				if len(e.den.got) != 0 {
					t.Fatalf("unexpected denials %+v", e.den.got)
				}
				return
			}
			if !strings.Contains(rec.Body.String(), service.DenyRole) || len(e.den.got) != 1 {
				t.Fatalf("body=%s denials=%+v", rec.Body.String(), e.den.got)
			}
			d := e.den.got[0]
			if d.Code != service.DenyRole || d.Op != tt.wantOp || d.Scope.TenantID != 1 || d.Scope.FarmID != 1 ||
				d.UserID != tt.uid || !strings.HasPrefix(d.Route, tt.method+" "+farmsPrefix) {
				t.Fatalf("denial = %+v", d)
			}
		})
	}
}

// notInScope 模拟 service 归属查询未命中（ErrNotInScope 包着仓储的 ErrNotFound）。
var notInScope = fmt.Errorf("service: x: %w: %w", service.ErrNotInScope, model.ErrNotFound)

// assertIntegrity500 校验关联数据缺失（裸 ErrNotFound）：500、不写越界审计。
func assertIntegrity500(t *testing.T, e *testEnv, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError || len(e.den.got) != 0 {
		t.Fatalf("status = %d denials = %+v, want 500 and no denial", rec.Code, e.den.got)
	}
}

// assertResourceDenied 校验资源越界：403、不暴露存在性、记一次 RESOURCE_NOT_IN_SCOPE（target 如 run:7）。
func assertResourceDenied(t *testing.T, e *testEnv, rec *httptest.ResponseRecorder, target string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(e.den.codes(), []string{service.DenyResource}) || e.den.got[0].Target != target ||
		e.den.got[0].Scope.FarmID == 0 {
		t.Fatalf("denials = %+v, want one %s on %s", e.den.got, service.DenyResource, target)
	}
}
