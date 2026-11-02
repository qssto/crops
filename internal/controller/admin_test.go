package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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

// kbExpertStore 在 testRoleStore 之上加一个平台 kb_expert（9002）。
type kbExpertStore struct{ testRoleStore }

func (s kbExpertStore) ListRoles(ctx context.Context, tenantID, farmID, userID uint64) ([]model.RoleBinding, error) {
	if userID == 9002 {
		return []model.RoleBinding{{Role: model.RoleKBExpert}}, nil
	}
	return s.testRoleStore.ListRoles(ctx, tenantID, farmID, userID)
}

func TestAdminFlags(t *testing.T) {
	// 9002 是绑在 (0,0) 的 kb_expert：过平台 Scope，但权限矩阵不给 flag.write。
	e := newTestEnvWith(t, kbExpertStore{})
	bearer := func(uid uint64) string {
		tenant := uint64(1)
		if uid >= 9001 {
			tenant = 0
		}
		return e.bearer(t, uid, tenant)
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
		wantDeny   string // 非空时要求恰好一次该拒绝码的审计
	}{
		{name: "put ok", method: http.MethodPut, path: put, auth: bearer(9001), body: okBody, wantStatus: http.StatusOK},
		{name: "put global ok", method: http.MethodPut, path: "/api/v1/admin/flags/enable_advice/scopes/0/0", auth: bearer(9001),
			body: `{"value":{"force_off":true},"comment":"x"}`, wantStatus: http.StatusOK},
		{name: "no token 401", method: http.MethodPut, path: put, body: okBody, wantStatus: http.StatusUnauthorized},
		{name: "bad token 401", method: http.MethodPut, path: put, auth: "Bearer x.y.z", body: okBody,
			wantStatus: http.StatusUnauthorized},
		{name: "farm user no platform grant 403", method: http.MethodPut, path: put, auth: bearer(1001), body: okBody,
			wantStatus: http.StatusForbidden, wantError: "SCOPE_DENIED", wantDeny: service.DenyScope},
		{name: "platform kb_expert lacks flag.write 403", method: http.MethodPut, path: put, auth: bearer(9002), body: okBody,
			wantStatus: http.StatusForbidden, wantError: "ROLE_INSUFFICIENT", wantDeny: service.DenyRole},
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
			den := &recDenier{}
			g := e.guard
			g.Denials, g.Logger = den, slog.New(slog.NewTextHandler(io.Discard, nil))
			r := gin.New()
			MountAdmin(r, g, fa)
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
			if tt.wantDeny != "" && !slices.Equal(den.codes(), []string{tt.wantDeny}) {
				t.Fatalf("denials = %v, want [%s]", den.codes(), tt.wantDeny)
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
