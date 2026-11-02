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

type fakeOrderOps struct {
	err      error
	pending  bool
	gotScope model.Scope
	gotIn    service.ReconcileInput
	gotOrder uint64
}

func (f *fakeOrderOps) Cancel(_ context.Context, scope model.Scope, orderID uint64) (service.CancelResult, error) {
	f.gotScope, f.gotOrder = scope, orderID
	return service.CancelResult{OrderID: orderID, OrderStatus: model.OrderDispatched, Pending: f.pending}, f.err
}

func (f *fakeOrderOps) Reconcile(_ context.Context, scope model.Scope, in service.ReconcileInput) (service.ReconcileResult, error) {
	f.gotScope, f.gotIn = scope, in
	return service.ReconcileResult{OrderID: in.OrderID, AttemptID: 200, OrderStatus: model.OrderDone}, f.err
}

func (f *fakeOrderOps) PendingReconcile(_ context.Context, scope model.Scope) ([]model.DispatchAttempt, error) {
	f.gotScope = scope
	return nil, f.err
}

// 权限红线：对账只给 farm_manager / platform_admin，中止不给 auditor / farmer；拒绝写 ROLE_INSUFFICIENT 审计。
func TestOrderOpsRoutes(t *testing.T) {
	e := newTestEnv(t)
	const cancel, reconcile, pending = "/api/v1/farms/1/orders/100/cancel", "/api/v1/farms/1/orders/100/reconcile", "/api/v1/farms/1/reconcile/pending"
	okBody := `{"conclusion":"DONE","done_area_mu":12,"comment":"飞手确认"}`
	tests := []struct {
		name       string
		method     string
		path       string
		uid        uint64
		tenant     uint64
		body       string
		err        error
		pending    bool
		wantStatus int
		wantError  string
		wantDenied string // 期望的拒绝码
	}{
		{name: "manager reconcile ok", method: http.MethodPost, path: reconcile, uid: 1001, tenant: 1, body: okBody, wantStatus: http.StatusOK},
		{name: "platform admin reconcile ok", method: http.MethodPost, path: reconcile, uid: 9001, body: okBody, wantStatus: http.StatusOK},
		{name: "agronomist reconcile denied", method: http.MethodPost, path: reconcile, uid: 1002, tenant: 1, body: okBody,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyRole},
		{name: "operator reconcile denied", method: http.MethodPost, path: reconcile, uid: 1003, tenant: 1, body: okBody,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyRole},
		{name: "auditor cancel denied", method: http.MethodPost, path: cancel, uid: 3001, tenant: 1,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyRole},
		{name: "no token", method: http.MethodPost, path: cancel, wantStatus: http.StatusUnauthorized},
		{name: "other farm manager denied", method: http.MethodPost, path: "/api/v1/farms/1/orders/100/reconcile", uid: 2001, tenant: 1,
			body: okBody, wantStatus: http.StatusForbidden, wantDenied: service.DenyScope},
		{name: "operator cancel done", method: http.MethodPost, path: cancel, uid: 1003, tenant: 1, wantStatus: http.StatusOK},
		{name: "cancel pending device", method: http.MethodPost, path: cancel, uid: 1003, tenant: 1, pending: true, wantStatus: http.StatusAccepted},
		{name: "cancel terminal 409", method: http.MethodPost, path: cancel, uid: 1003, tenant: 1,
			err: fmt.Errorf("x: %w", service.ErrCancelState), wantStatus: http.StatusConflict, wantError: "CANCEL_NOT_ALLOWED"},
		{name: "cancel busy 409", method: http.MethodPost, path: cancel, uid: 1003, tenant: 1, err: service.ErrOrderBusy,
			wantStatus: http.StatusConflict, wantError: "ORDER_BUSY"},
		{name: "cancel other farm order 403", method: http.MethodPost, path: cancel, uid: 1003, tenant: 1, err: notInScope,
			wantStatus: http.StatusForbidden, wantDenied: service.DenyResource},
		{name: "cancel bad order id", method: http.MethodPost, path: "/api/v1/farms/1/orders/x/cancel", uid: 1003, tenant: 1,
			wantStatus: http.StatusBadRequest},
		{name: "reconcile not pending 409", method: http.MethodPost, path: reconcile, uid: 1001, tenant: 1, body: okBody,
			err: service.ErrReconcileState, wantStatus: http.StatusConflict, wantError: "RECONCILE_NOT_PENDING"},
		{name: "reconcile bad conclusion 400", method: http.MethodPost, path: reconcile, uid: 1001, tenant: 1, body: okBody,
			err: service.ErrBadInput, wantStatus: http.StatusBadRequest},
		{name: "reconcile bad body", method: http.MethodPost, path: reconcile, uid: 1001, tenant: 1, body: `{"conclusion":`,
			wantStatus: http.StatusBadRequest},
		{name: "reconcile internal 500", method: http.MethodPost, path: reconcile, uid: 1001, tenant: 1, body: okBody,
			err: errors.New("db down"), wantStatus: http.StatusInternalServerError},
		{name: "pending list for reader", method: http.MethodGet, path: pending, uid: 3001, tenant: 1, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			s := &fakeOrderOps{err: tt.err, pending: tt.pending}
			router := NewRouter(e.guard, &fakeAsker{}, OrderOpsRoutes(s))
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.uid != 0 {
				req.Header.Set("Authorization", e.bearer(t, tt.uid, tt.tenant))
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			codes := e.den.codes()
			if tt.wantDenied == "" && len(codes) != 0 || tt.wantDenied != "" && (len(codes) != 1 || codes[0] != tt.wantDenied) {
				t.Fatalf("denials = %+v, want %q", e.den.got, tt.wantDenied)
			}
			if tt.wantDenied == service.DenyResource && e.den.got[0].Target != "order:100" {
				t.Fatalf("denial target = %s", e.den.got[0].Target)
			}
			if tt.wantError != "" {
				var got errorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error != tt.wantError {
					t.Fatalf("body = %s, want %q", rec.Body.String(), tt.wantError)
				}
			}
			if tt.name == "manager reconcile ok" && (s.gotIn.OrderID != 100 || s.gotIn.Conclusion != model.AttemptDone ||
				s.gotIn.DoneAreaMu == nil || *s.gotIn.DoneAreaMu != 12 || s.gotScope.FarmID != 1 || s.gotScope.UserID != 1001) {
				t.Fatalf("reconcile input = %+v scope %+v", s.gotIn, s.gotScope)
			}
			if tt.name == "pending list for reader" && !strings.Contains(rec.Body.String(), `"attempts":[]`) {
				t.Fatalf("pending body = %s", rec.Body.String())
			}
		})
	}
}
