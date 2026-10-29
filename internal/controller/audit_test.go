package controller

import (
	"context"
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

type fakeRawReader struct {
	err            error
	gotRun, gotRaw uint64
}

func (f *fakeRawReader) ReadRaw(_ context.Context, _ model.Scope, runID, rawID uint64) (service.RawLink, error) {
	f.gotRun, f.gotRaw = runID, rawID
	if f.err != nil {
		return service.RawLink{}, f.err
	}
	return service.RawLink{RawID: rawID, URL: "https://minio.local/x?sig=1"}, nil
}

func TestRawHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	grants := map[uint64][]model.Grant{1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleAgronomist}}}}
	auth, err := service.NewAuthService("0123456789abcdef0123456789abcdef", grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		path       string
		err        error
		wantStatus int
	}{
		{name: "ok", path: "/api/v1/farms/1/runs/9/raw/101", wantStatus: http.StatusOK},
		{name: "denied", path: "/api/v1/farms/1/runs/9/raw/101",
			err: fmt.Errorf("%w: RUN_MISMATCH", service.ErrForbidden), wantStatus: http.StatusForbidden},
		{name: "bad raw id", path: "/api/v1/farms/1/runs/9/raw/x", wantStatus: http.StatusBadRequest},
		{name: "bad run id", path: "/api/v1/farms/1/runs/0/raw/101", wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRawReader{err: tt.err}
			router := NewRouter(auth, &fakeAsker{}, AuditRoutes(r))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusOK && (r.gotRun != 9 || r.gotRaw != 101 ||
				!strings.Contains(rec.Body.String(), `"url"`) || rec.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("got run=%d raw=%d body=%s", r.gotRun, r.gotRaw, rec.Body.String())
			}
		})
	}
}
