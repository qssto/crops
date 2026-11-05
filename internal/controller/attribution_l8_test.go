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
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type fakeIssueAdmin struct {
	err      error
	gotActor model.Scope
	gotID    uint64
	gotPatch model.AttributionPatch
	calls    int
}

func (f *fakeIssueAdmin) Patch(_ context.Context, actor model.Scope, id uint64, p model.AttributionPatch) (model.AttributionIssue, error) {
	f.calls++
	f.gotActor, f.gotID, f.gotPatch = actor, id, p
	if f.err != nil {
		return model.AttributionIssue{}, f.err
	}
	return model.AttributionIssue{ID: id, Status: *p.Status, RowVer: *p.RowVer + 1}, nil
}

// 应失败：row_ver 冲突未回 409、农场用户 / 无 attribution.write 的平台角色能改。
func TestAttributionIssuePatch(t *testing.T) {
	e := newTestEnvWith(t, kbExpertStore{})
	bearer := func(uid uint64) string {
		tenant := uint64(1)
		if uid >= 9001 {
			tenant = 0
		}
		return e.bearer(t, uid, tenant)
	}
	const (
		path   = "/api/v1/admin/attribution-issues/7"
		okBody = `{"status":"RESOLVED","disposition":"ADD_KB","resolution_ref":"kb PR #1","row_ver":3}`
	)
	wrap := func(e error) error { return fmt.Errorf("svc: %w", e) }
	tests := []struct {
		name       string
		path, auth string
		body       string
		svcErr     error
		wantStatus int
		wantError  string
	}{
		{"row_ver conflict 409", path, bearer(9001), okBody, wrap(service.ErrIssueConflict), http.StatusConflict, "ISSUE_ROW_VER_CONFLICT"},
		{"ok", path, bearer(9001), okBody, nil, http.StatusOK, ""},
		{"no token 401", path, "", okBody, nil, http.StatusUnauthorized, ""},
		{"farm user 403", path, bearer(1001), okBody, nil, http.StatusForbidden, "SCOPE_DENIED"},
		{"platform kb_expert lacks attribution.write 403", path, bearer(9002), okBody, nil, http.StatusForbidden, "ROLE_INSUFFICIENT"},
		{"bad id 400", "/api/v1/admin/attribution-issues/x", bearer(9001), okBody, nil, http.StatusBadRequest, ""},
		{"zero id 400", "/api/v1/admin/attribution-issues/0", bearer(9001), okBody, nil, http.StatusBadRequest, ""},
		{"bad body 400", path, bearer(9001), `{"row_ver":`, nil, http.StatusBadRequest, ""},
		{"invalid patch 400", path, bearer(9001), okBody, wrap(model.ErrAttributionPatch), http.StatusBadRequest, "ATTRIBUTION_PATCH_INVALID"},
		{"not found 404", path, bearer(9001), okBody, wrap(model.ErrNotFound), http.StatusNotFound, ""},
		{"internal 500", path, bearer(9001), okBody, errors.New("db down"), http.StatusInternalServerError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeIssueAdmin{err: tt.svcErr}
			g := e.guard
			g.Denials, g.Logger = &recDenier{}, slog.New(slog.NewTextHandler(io.Discard, nil))
			r := gin.New()
			MountAttributionAdmin(r, g, fa)
			req := httptest.NewRequest(http.MethodPatch, tt.path, strings.NewReader(tt.body))
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
			if (tt.wantStatus == http.StatusUnauthorized || tt.wantStatus == http.StatusForbidden) && fa.calls != 0 {
				t.Fatalf("denied request reached service")
			}
			if tt.name == "ok" && (fa.gotActor.UserID != 9001 || fa.gotActor.TenantID != 0 || fa.gotID != 7 ||
				*fa.gotPatch.Status != model.IssueResolved || *fa.gotPatch.RowVer != 3 || *fa.gotPatch.ResolutionRef != "kb PR #1" ||
				fa.gotPatch.OwnerID != nil || !strings.Contains(rec.Body.String(), `"row_ver":4`)) {
				t.Fatalf("actor %+v id %d patch %+v body %s", fa.gotActor, fa.gotID, fa.gotPatch, rec.Body.String())
			}
		})
	}
}
