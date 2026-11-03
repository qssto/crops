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

// fakeAuditExporter 是内存版 AuditExporter。
type fakeAuditExporter struct {
	err      error
	gotScope model.Scope
	gotID    uint64
	gotReq   service.AuditExportRequest
}

func (f *fakeAuditExporter) Request(_ context.Context, scope model.Scope, req service.AuditExportRequest) (service.AuditExportStatus, error) {
	f.gotScope, f.gotReq = scope, req
	return service.AuditExportStatus{ExportID: 42, State: model.AuditExportPending}, f.err
}

func (f *fakeAuditExporter) Status(_ context.Context, scope model.Scope, id uint64) (service.AuditExportStatus, error) {
	f.gotScope, f.gotID = scope, id
	return service.AuditExportStatus{ExportID: id, State: model.AuditExportDone, URL: "https://x/y"}, f.err
}

// 权限红线：audit.export 只给 auditor / platform_admin；farm_manager / agronomist 拒绝（ROLE_INSUFFICIENT 审计），
// 他场用户 CROSS_FARM_DENIED（scope），他场导出 id → 404，过保留期 → 410。
func TestAuditExportHandlers(t *testing.T) {
	e := newTestEnv(t)
	const body = `{"from":"2026-10-01T00:00:00Z","to":"2026-10-08T00:00:00Z","request_id":"r-1"}`
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		uid        uint64
		err        error
		wantStatus int
		wantDenied string
	}{
		{name: "auditor post", method: http.MethodPost, path: "/api/v1/farms/1/audit/exports", body: body, uid: 3001,
			wantStatus: http.StatusAccepted},
		{name: "platform admin post", method: http.MethodPost, path: "/api/v1/farms/1/audit/exports", body: body, uid: 9001,
			wantStatus: http.StatusAccepted},
		{name: "farm manager denied", method: http.MethodPost, path: "/api/v1/farms/1/audit/exports", body: body, uid: 1001,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyRole},
		{name: "agronomist get denied", method: http.MethodGet, path: "/api/v1/farms/1/audit/exports/42", uid: 1002,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyRole},
		{name: "other farm user denied", method: http.MethodPost, path: "/api/v1/farms/1/audit/exports", body: body, uid: 2001,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyScope},
		{name: "bad body", method: http.MethodPost, path: "/api/v1/farms/1/audit/exports", body: `{`, uid: 3001,
			wantStatus: http.StatusBadRequest},
		{name: "span over max", method: http.MethodPost, path: "/api/v1/farms/1/audit/exports", body: body, uid: 3001,
			err: fmt.Errorf("x: %w", service.ErrBadInput), wantStatus: http.StatusBadRequest},
		{name: "auditor get done", method: http.MethodGet, path: "/api/v1/farms/1/audit/exports/42", uid: 3001,
			wantStatus: http.StatusOK},
		{name: "bad export id", method: http.MethodGet, path: "/api/v1/farms/1/audit/exports/x", uid: 3001,
			wantStatus: http.StatusBadRequest},
		{name: "other farm export 404", method: http.MethodGet, path: "/api/v1/farms/1/audit/exports/77", uid: 3001,
			err: fmt.Errorf("x: %w", model.ErrNotFound), wantStatus: http.StatusNotFound},
		{name: "expired 410", method: http.MethodGet, path: "/api/v1/farms/1/audit/exports/42", uid: 3001,
			err: fmt.Errorf("x: %w", model.ErrRawExpired), wantStatus: http.StatusGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			ex := &fakeAuditExporter{err: tt.err}
			router := NewRouter(e.guard, &fakeAsker{}, AuditExportRoutes(ex))
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", e.bearer(t, tt.uid, 1))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			codes := e.den.codes()
			if tt.wantDenied == "" && len(codes) != 0 || tt.wantDenied != "" && (len(codes) != 1 || codes[0] != tt.wantDenied) {
				t.Fatalf("denials = %+v, want %q", e.den.got, tt.wantDenied)
			}
			switch tt.name {
			case "auditor post":
				if ex.gotScope.FarmID != 1 || ex.gotReq.RequestID != "r-1" || !strings.Contains(rec.Body.String(), `"export_id":42`) {
					t.Fatalf("scope %+v req %+v body %s", ex.gotScope, ex.gotReq, rec.Body.String())
				}
			case "auditor get done":
				if ex.gotID != 42 || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("id %d headers %v", ex.gotID, rec.Header())
				}
			}
		})
	}
}
