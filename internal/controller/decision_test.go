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

// fakeDecider 记录收到的输入，按配置返回。
type fakeDecider struct {
	err   error
	gotIn service.DecideInput
}

func (f *fakeDecider) Pending(context.Context, model.Scope) ([]service.PendingTicket, error) {
	return []service.PendingTicket{{ID: 7, OrderID: 3, RiskLevel: "LOW"}}, f.err
}

func (f *fakeDecider) Decide(_ context.Context, _ model.Scope, in service.DecideInput) (service.DecideResult, error) {
	f.gotIn = in
	if f.err != nil {
		return service.DecideResult{}, f.err
	}
	return service.DecideResult{DecisionID: 1, TicketID: in.TicketID, Status: model.TicketApproved}, nil
}

func TestDecideHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	grants := map[uint64][]model.Grant{1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleAgronomist}}}}
	auth, err := service.NewAuthService("0123456789abcdef0123456789abcdef", grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/farms/1/approvals/7/decide"
	body := `{"decision":"APPROVE","snapshot_digest":"` + strings.Repeat("a", 64) + `"}`
	tests := []struct {
		name       string
		path       string
		reqID      string
		err        error
		wantStatus int
		wantError  string
	}{
		{name: "ok", path: path, reqID: "r-1", wantStatus: http.StatusOK},
		{name: "missing request id", path: path, wantStatus: http.StatusBadRequest},
		{name: "bad ticket id", path: "/api/v1/farms/1/approvals/x/decide", reqID: "r-1", wantStatus: http.StatusBadRequest},
		{name: "closed", path: path, reqID: "r-1", err: fmt.Errorf("wrap: %w", service.ErrTicketClosed),
			wantStatus: http.StatusConflict, wantError: "TICKET_CLOSED"},
		{name: "expired", path: path, reqID: "r-1", err: service.ErrTicketExpired, wantStatus: http.StatusConflict, wantError: "TICKET_EXPIRED"},
		{name: "stale", path: path, reqID: "r-1", err: service.ErrSnapshotStale, wantStatus: http.StatusConflict, wantError: "SNAPSHOT_STALE"},
		{name: "in flight", path: path, reqID: "r-1", err: service.ErrRequestInFlight, wantStatus: http.StatusConflict, wantError: "REQUEST_IN_FLIGHT"},
		{name: "request id reused", path: path, reqID: "r-1", err: service.ErrRequestConflict, wantStatus: http.StatusConflict, wantError: "REQUEST_ID_CONFLICT"},
		{name: "busy", path: path, reqID: "r-1", err: service.ErrOrderBusy, wantStatus: http.StatusConflict, wantError: "ORDER_BUSY"},
		{name: "role", path: path, reqID: "r-1", err: service.ErrRoleInsufficient, wantStatus: http.StatusForbidden, wantError: "ROLE_INSUFFICIENT"},
		{name: "same approver", path: path, reqID: "r-1", err: service.ErrSameApprover, wantStatus: http.StatusForbidden, wantError: "SAME_APPROVER"},
		{name: "bad input", path: path, reqID: "r-1", err: service.ErrBadInput, wantStatus: http.StatusBadRequest},
		{name: "not found", path: path, reqID: "r-1", err: model.ErrNotFound, wantStatus: http.StatusNotFound},
		{name: "internal", path: path, reqID: "r-1", err: errors.New("db down"), wantStatus: http.StatusInternalServerError,
			wantError: http.StatusText(http.StatusInternalServerError)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDecider{err: tt.err}
			router := NewRouter(auth, &fakeAsker{}, ApprovalRoutes(d))
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tok)
			if tt.reqID != "" {
				req.Header.Set(HeaderRequestID, tt.reqID)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantError != "" {
				var got errorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error != tt.wantError {
					t.Fatalf("body = %s, want error %q (%v)", rec.Body.String(), tt.wantError, err)
				}
			}
			if tt.name == "ok" && (d.gotIn.TicketID != 7 || d.gotIn.RequestID != "r-1" || d.gotIn.Decision != "APPROVE") {
				t.Fatalf("input = %+v", d.gotIn)
			}
		})
	}

	// 待审批列表：status 缺省或 OPEN 返回列表，其他取值 400
	router := NewRouter(auth, &fakeAsker{}, ApprovalRoutes(&fakeDecider{}))
	for _, tc := range []struct {
		query string
		want  int
	}{{"", http.StatusOK}, {"?status=OPEN", http.StatusOK}, {"?status=APPROVED", http.StatusBadRequest}} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/farms/1/approvals"+tc.query, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want || (tc.want == http.StatusOK && !strings.Contains(rec.Body.String(), `"ticket_id":7`)) {
			t.Fatalf("pending%s = %d %s", tc.query, rec.Code, rec.Body.String())
		}
	}
}
