package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestFarmAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	secret := "0123456789abcdef0123456789abcdef"
	grants := map[uint64][]model.Grant{
		1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleFarmManager}}},
		9001: {{TenantID: 0, FarmID: 0, Roles: []model.Role{model.RolePlatformAdmin}}},
	}
	auth, err := service.NewAuthService(secret, grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	forger, err := service.NewAuthService(strings.Repeat("x", 32), grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	issue := func(a *service.AuthService, uid uint64) string {
		tok, err := a.Issue(uid, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	router := NewRouter(auth)

	tests := []struct {
		name       string
		path       string
		header     string
		body       string
		wantStatus int
	}{
		{name: "ok", path: "/api/v1/farms/1/ping", header: "Bearer " + issue(auth, 1001), wantStatus: http.StatusOK},
		{name: "no token", path: "/api/v1/farms/1/ping", wantStatus: http.StatusUnauthorized},
		{name: "not bearer", path: "/api/v1/farms/1/ping", header: issue(auth, 1001), wantStatus: http.StatusUnauthorized},
		{name: "bad signature", path: "/api/v1/farms/1/ping", header: "Bearer " + issue(forger, 1001), wantStatus: http.StatusUnauthorized},
		{name: "farm not in scope", path: "/api/v1/farms/2/ping", header: "Bearer " + issue(auth, 1001), wantStatus: http.StatusForbidden},
		{name: "platform admin on farm path", path: "/api/v1/farms/1/ping", header: "Bearer " + issue(auth, 9001), wantStatus: http.StatusForbidden},
		{name: "body farm_id ignored", path: "/api/v1/farms/2/ping", header: "Bearer " + issue(auth, 1001),
			body: `{"farm_id":1}`, wantStatus: http.StatusForbidden},
		{name: "bad farm_id", path: "/api/v1/farms/abc/ping", header: "Bearer " + issue(auth, 1001), wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, strings.NewReader(tt.body))
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			var got model.Scope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.TenantID != 1 || got.FarmID != 1 || got.UserID != 1001 || !got.HasRole(model.RoleFarmManager) {
				t.Fatalf("scope = %+v", got)
			}
		})
	}
}
