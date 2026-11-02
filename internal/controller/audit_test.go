package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	e := newTestEnv(t)
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
			e.den.got = nil
			r := &fakeRawReader{err: tt.err}
			router := NewRouter(e.guard, &fakeAsker{}, AuditRoutes(r))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", e.bearer(t, 1002, 1))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.err != nil {
				assertResourceDenied(t, e, rec, "raw:101")
			}
			if tt.wantStatus == http.StatusOK && (r.gotRun != 9 || r.gotRaw != 101 ||
				!strings.Contains(rec.Body.String(), `"url"`) || rec.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("got run=%d raw=%d body=%s", r.gotRun, r.gotRaw, rec.Body.String())
			}
		})
	}
}
