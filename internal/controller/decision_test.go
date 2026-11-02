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
	e := newTestEnv(t)
	bearer := e.bearer(t, 1002, 1)
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
		{name: "role denied by matrix", path: path, reqID: "r-1", err: fmt.Errorf("service: decide: %w",
			&service.RoleDeniedError{Op: service.OpApprovalDecidePesticide}), wantStatus: http.StatusForbidden, wantError: "ROLE_INSUFFICIENT"},
		{name: "same approver", path: path, reqID: "r-1", err: service.ErrSameApprover, wantStatus: http.StatusForbidden, wantError: "SAME_APPROVER"},
		// P1 L5 应失败：驳回缺原因 → 400 REJECT_REASON_INVALID（不是泛化的 bad input）。
		{name: "reject reason invalid", path: path, reqID: "r-1", err: fmt.Errorf("deny: %w: %w", service.ErrBadInput,
			service.ErrRejectReasonInvalid), wantStatus: http.StatusBadRequest, wantError: "REJECT_REASON_INVALID"},
		{name: "bad input", path: path, reqID: "r-1", err: service.ErrBadInput, wantStatus: http.StatusBadRequest},
		{name: "not found", path: path, reqID: "r-1", err: notInScope, wantStatus: http.StatusForbidden},
		{name: "related data missing", path: path, reqID: "r-1", err: fmt.Errorf("replay: %w", model.ErrNotFound),
			wantStatus: http.StatusInternalServerError},
		{name: "internal", path: path, reqID: "r-1", err: errors.New("db down"), wantStatus: http.StatusInternalServerError,
			wantError: http.StatusText(http.StatusInternalServerError)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			d := &fakeDecider{err: tt.err}
			router := NewRouter(e.guard, &fakeAsker{}, ApprovalRoutes(d))
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(body))
			req.Header.Set("Authorization", bearer)
			if tt.reqID != "" {
				req.Header.Set(HeaderRequestID, tt.reqID)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			switch tt.name {
			case "not found":
				assertResourceDenied(t, e, rec, "ticket:7")
			case "related data missing":
				assertIntegrity500(t, e, rec)
			case "role denied by matrix": // §6.2 流程 5：审批角色拒绝也写 CROSS_FARM_DENIED，op 为实际审批类别
				if len(e.den.got) != 1 || e.den.got[0].Code != service.DenyRole ||
					e.den.got[0].Op != service.OpApprovalDecidePesticide || e.den.got[0].Target != "ticket:7" ||
					e.den.got[0].Scope.FarmID != 1 {
					t.Fatalf("denials = %+v", e.den.got)
				}
			case "same approver", "stale":
				if len(e.den.got) != 0 {
					t.Fatalf("business denial must not write CROSS_FARM_DENIED: %+v", e.den.got)
				}
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

	// P1 L5：驳回原因原样透传到 DecideInput。
	d := &fakeDecider{}
	rr := NewRouter(e.guard, &fakeAsker{}, ApprovalRoutes(d))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"decision":"REJECT","snapshot_digest":"`+
		strings.Repeat("a", 64)+`","reason_code":"OTHER","reason_text":"药剂需轮换"}`))
	req.Header.Set("Authorization", bearer)
	req.Header.Set(HeaderRequestID, "r-2")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || d.gotIn.ReasonCode != model.RejectOther || d.gotIn.ReasonText != "药剂需轮换" {
		t.Fatalf("reject = %d %s, input %+v", rec.Code, rec.Body.String(), d.gotIn)
	}

	// 待审批列表：status 缺省或 OPEN 返回列表，其他取值 400
	router := NewRouter(e.guard, &fakeAsker{}, ApprovalRoutes(&fakeDecider{}))
	for _, tc := range []struct {
		query string
		want  int
	}{{"", http.StatusOK}, {"?status=OPEN", http.StatusOK}, {"?status=APPROVED", http.StatusBadRequest}} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/farms/1/approvals"+tc.query, nil)
		req.Header.Set("Authorization", bearer)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want || (tc.want == http.StatusOK && !strings.Contains(rec.Body.String(), `"ticket_id":7`)) {
			t.Fatalf("pending%s = %d %s", tc.query, rec.Code, rec.Body.String())
		}
	}
}
