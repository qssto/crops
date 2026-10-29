package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type fakeRunDetailer struct {
	err   error
	gotID uint64
}

func (f *fakeRunDetailer) Get(_ context.Context, _ model.Scope, runID uint64) (service.RunDetail, error) {
	f.gotID = runID
	if f.err != nil {
		return service.RunDetail{}, f.err
	}
	return service.RunDetail{Run: model.RunSummary{RunID: runID}, Steps: []service.StepDetail{}}, nil
}

type fakeMetrics struct {
	err      error
	from, to time.Time
}

func (f *fakeMetrics) Daily(_ context.Context, _ model.Scope, from, to time.Time) (service.MetricsReport, error) {
	f.from, f.to = from, to
	return service.MetricsReport{From: "x"}, f.err
}

func TestRunRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	grants := map[uint64][]model.Grant{1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleFarmManager}}}}
	auth, err := service.NewAuthService("0123456789abcdef0123456789abcdef", grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bearer := "Bearer " + tok
	tests := []struct {
		name       string
		path       string
		auth       string
		detailErr  error
		metricsErr error
		wantStatus int
	}{
		{name: "detail 200", path: "/api/v1/farms/1/runs/7", auth: bearer, wantStatus: http.StatusOK},
		{name: "detail no token 401", path: "/api/v1/farms/1/runs/7", wantStatus: http.StatusUnauthorized},
		{name: "detail other farm 403", path: "/api/v1/farms/2/runs/7", auth: bearer, wantStatus: http.StatusForbidden},
		{name: "detail bad id 400", path: "/api/v1/farms/1/runs/abc", auth: bearer, wantStatus: http.StatusBadRequest},
		{name: "detail id 0 400", path: "/api/v1/farms/1/runs/0", auth: bearer, wantStatus: http.StatusBadRequest},
		{name: "detail not found 404", path: "/api/v1/farms/1/runs/7", auth: bearer, detailErr: model.ErrNotFound,
			wantStatus: http.StatusNotFound},
		{name: "detail internal 500", path: "/api/v1/farms/1/runs/7", auth: bearer, detailErr: errors.New("db down"),
			wantStatus: http.StatusInternalServerError},
		{name: "metrics default range 200", path: "/api/v1/farms/1/metrics", auth: bearer, wantStatus: http.StatusOK},
		{name: "metrics range 200", path: "/api/v1/farms/1/metrics?from=2026-10-01&to=2026-10-29", auth: bearer,
			wantStatus: http.StatusOK},
		{name: "metrics bad from 400", path: "/api/v1/farms/1/metrics?from=10/01", auth: bearer,
			wantStatus: http.StatusBadRequest},
		{name: "metrics bad to 400", path: "/api/v1/farms/1/metrics?to=2026-13-01", auth: bearer,
			wantStatus: http.StatusBadRequest},
		{name: "metrics service bad input 400", path: "/api/v1/farms/1/metrics", auth: bearer,
			metricsErr: service.ErrBadInput, wantStatus: http.StatusBadRequest},
		{name: "metrics no token 401", path: "/api/v1/farms/1/metrics", wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, m := &fakeRunDetailer{err: tt.detailErr}, &fakeMetrics{err: tt.metricsErr}
			router := NewRouter(auth, &fakeAsker{}, RunRoutes(d, m))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.name == "detail 200" {
				var got map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got["run"] == nil || d.gotID != 7 {
					t.Fatalf("body = %s, id %d, err %v", rec.Body.String(), d.gotID, err)
				}
			}
			if tt.name == "metrics range 200" {
				want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				if !m.from.Equal(want) || m.to.Day() != 29 {
					t.Fatalf("from=%s to=%s", m.from, m.to)
				}
			}
		})
	}
}

func TestMountDashboard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	r := gin.New()
	MountDashboard(r, dir) // 文件缺失时挂载不报错
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/dashboard"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing dashboard: status %d", rec.Code)
	}
	if err := os.WriteFile(filepath.Join(dir, DashboardFile), []byte(`{"type":"page"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path       string
		wantStatus int
	}{
		{"/dashboard", http.StatusOK},
		{"/dashboard/" + DashboardFile, http.StatusOK},
		{"/dashboard/secret.txt", http.StatusNotFound},
		{"/dashboard/.hidden.json", http.StatusNotFound},
		{"/dashboard/missing.json", http.StatusNotFound},
		{"/dashboard/..%2Fx.json", http.StatusNotFound},
	}
	for _, tt := range tests {
		if rec := get(tt.path); rec.Code != tt.wantStatus {
			t.Errorf("GET %s = %d, want %d", tt.path, rec.Code, tt.wantStatus)
		}
	}
	if rec := get("/dashboard/" + DashboardFile); rec.Body.String() != `{"type":"page"}` {
		t.Errorf("dashboard.json body = %q", rec.Body.String())
	}
	// /steward 外壳：steward.json 缺失时 404，装好后返回加载 steward.json 的 HTML。
	if rec := get("/steward"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing steward: status %d", rec.Code)
	}
	if err := os.WriteFile(filepath.Join(dir, StewardFile), []byte(`{"type":"page"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := get("/steward")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/dashboard/"+StewardFile) {
		t.Fatalf("GET /steward = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/dashboard"); !strings.Contains(rec.Body.String(), "/dashboard/"+DashboardFile) {
		t.Errorf("dashboard shell must load %s", DashboardFile)
	}
}
