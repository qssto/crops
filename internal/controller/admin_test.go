package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fakeFlagAdmin 记录收到的 actor / 入参，按配置返回。
type fakeFlagAdmin struct {
	err      error
	gotActor model.Scope
	gotIn    service.FlagSetInput
	gotKey   model.FlagKey
}

func (f *fakeFlagAdmin) Set(_ context.Context, actor model.Scope, in service.FlagSetInput) (service.FlagSetResult, error) {
	f.gotActor, f.gotIn = actor, in
	if f.err != nil {
		return service.FlagSetResult{}, f.err
	}
	return service.FlagSetResult{FlagKey: in.Key, TenantID: in.TenantID, FarmID: in.FarmID, Value: in.Value, RowVer: 1}, nil
}

func (f *fakeFlagAdmin) List(_ context.Context, actor model.Scope, key model.FlagKey) ([]model.FlagSetting, error) {
	f.gotActor, f.gotKey = actor, key
	return nil, f.err
}

func TestAdminFlags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	grants := map[uint64][]model.Grant{
		1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleFarmManager}}},
		9001: {{TenantID: 0, FarmID: 0, Roles: []model.Role{model.RolePlatformAdmin}}},
	}
	auth, err := service.NewAuthService("0123456789abcdef0123456789abcdef", grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(uid uint64) string {
		tok, err := auth.Issue(uid, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + tok
	}
	const (
		put    = "/api/v1/admin/flags/enable_advice/scopes/1/1"
		okBody = `{"value":{"on":true},"comment":"灰度第一档","expect_row_ver":0}`
	)
	wrap := func(e error) error { return fmt.Errorf("svc: %w", e) }
	tests := []struct {
		name       string
		method     string
		path       string
		auth       string
		body       string
		svcErr     error
		wantStatus int
		wantError  string
	}{
		{name: "put ok", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody, wantStatus: http.StatusOK},
		{name: "put global ok", method: http.MethodPut, path: "/api/v1/admin/flags/enable_advice/scopes/0/0", auth: bearer(9001),
			body: `{"value":{"force_off":true},"comment":"x"}`, wantStatus: http.StatusOK},
		{name: "no token 401", method: http.MethodPut, path: put, body: okBody, wantStatus: http.StatusUnauthorized},
		{name: "bad token 401", method: http.MethodPut, path: put, auth: "Bearer x.y.z", body: okBody,
			wantStatus: http.StatusUnauthorized},
		{name: "farm user no platform grant 403", method: http.MethodPut, path: put, auth: bearer(1001), body: okBody,
			wantStatus: http.StatusForbidden},
		{name: "bad tenant_id 400", method: http.MethodPut, path: "/api/v1/admin/flags/enable_advice/scopes/x/1",
			auth: bearer(9001), body: okBody, wantStatus: http.StatusBadRequest},
		{name: "bad farm_id 400", method: http.MethodPut, path: "/api/v1/admin/flags/enable_advice/scopes/1/-1",
			auth: bearer(9001), body: okBody, wantStatus: http.StatusBadRequest},
		{name: "bad body 400", method: http.MethodPut, path: put, auth: bearer(9001), body: `{"value":`,
			wantStatus: http.StatusBadRequest},
		{name: "role insufficient 403", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrRoleInsufficient), wantStatus: http.StatusForbidden, wantError: "ROLE_INSUFFICIENT"},
		{name: "unknown flag 404", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrFlagUnknown), wantStatus: http.StatusNotFound, wantError: "FLAG_UNKNOWN"},
		{name: "bad value 400", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrFlagValue), wantStatus: http.StatusBadRequest, wantError: "FLAG_VALUE_INVALID"},
		{name: "bad scope 400", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrFlagScope), wantStatus: http.StatusBadRequest, wantError: "FLAG_SCOPE_INVALID"},
		{name: "comment missing 400", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrBadInput), wantStatus: http.StatusBadRequest},
		{name: "row_ver conflict 409", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrFlagConflict), wantStatus: http.StatusConflict, wantError: "FLAG_CONFLICT"},
		{name: "precondition 409", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: wrap(service.ErrFlagPrecondition), wantStatus: http.StatusConflict, wantError: "FLAG_PRECONDITION_FAILED"},
		{name: "internal 500", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody,
			svcErr: errors.New("db down"), wantStatus: http.StatusInternalServerError, wantError: "Internal Server Error"},
		{name: "list ok", method: http.MethodGet, path: "/api/v1/admin/flags/enable_advice", auth: bearer(9001),
			wantStatus: http.StatusOK},
		{name: "list farm user 403", method: http.MethodGet, path: "/api/v1/admin/flags/enable_advice", auth: bearer(1001),
			wantStatus: http.StatusForbidden},
		{name: "list unknown 404", method: http.MethodGet, path: "/api/v1/admin/flags/enable_magic", auth: bearer(9001),
			svcErr: wrap(service.ErrFlagUnknown), wantStatus: http.StatusNotFound, wantError: "FLAG_UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeFlagAdmin{err: tt.svcErr}
			r := gin.New()
			MountAdmin(r, auth, fa)
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantError != "" {
				var eb errorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil || eb.Error != tt.wantError {
					t.Fatalf("body = %s, want error %q", rec.Body.String(), tt.wantError)
				}
			}
			if rec.Code < http.StatusBadRequest && (fa.gotActor.UserID != 9001 || fa.gotActor.TenantID != 0 || fa.gotActor.FarmID != 0) {
				t.Fatalf("actor = %+v", fa.gotActor)
			}
			if tt.name == "put ok" && (fa.gotIn.Key != model.FlagEnableAdvice || fa.gotIn.TenantID != 1 || fa.gotIn.FarmID != 1 ||
				string(fa.gotIn.Value) != `{"on":true}` || fa.gotIn.Comment != "灰度第一档" || fa.gotIn.ExpectRowVer == nil ||
				*fa.gotIn.ExpectRowVer != 0) {
				t.Fatalf("in = %+v", fa.gotIn)
			}
			if tt.name == "put global ok" && (fa.gotIn.TenantID != 0 || fa.gotIn.FarmID != 0 || fa.gotIn.ExpectRowVer != nil) {
				t.Fatalf("in = %+v", fa.gotIn)
			}
			if tt.name == "list ok" && (fa.gotKey != model.FlagEnableAdvice ||
				!strings.Contains(rec.Body.String(), `"settings":[]`)) {
				t.Fatalf("key = %s body = %s", fa.gotKey, rec.Body.String())
			}
		})
	}
}
