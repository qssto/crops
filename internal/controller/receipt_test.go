package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type fakeReceipts struct {
	res service.WebhookResult
	err error
	got service.WebhookInput
}

func (f *fakeReceipts) Webhook(_ context.Context, in service.WebhookInput) (service.WebhookResult, error) {
	f.got = in
	return f.res, f.err
}

func TestReceiptHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const path = "/api/v1/farms/10/orders/100/receipts"
	tests := []struct {
		name       string
		path       string
		body       string
		res        service.WebhookResult
		err        error
		wantStatus int
	}{
		{name: "processed", path: path, body: `{}`, res: service.WebhookResult{Status: service.WebhookProcessed, Outcome: model.OutcomeApplied},
			wantStatus: http.StatusOK},
		{name: "replayed", path: path, body: `{}`, res: service.WebhookResult{Status: service.WebhookReplayed}, wantStatus: http.StatusOK},
		{name: "unmatched", path: path, body: `{}`, res: service.WebhookResult{Status: service.WebhookUnmatched}, wantStatus: http.StatusAccepted},
		{name: "bad signature", path: path, body: `{}`, err: fmt.Errorf("x: %w", service.ErrReceiptAuth), wantStatus: http.StatusUnauthorized},
		{name: "bad receipt", path: path, body: `{}`, err: service.ErrBadInput, wantStatus: http.StatusBadRequest},
		{name: "cross farm", path: path, body: `{}`, err: service.ErrForbidden, wantStatus: http.StatusForbidden},
		{name: "unknown job", path: path, body: `{}`, err: service.ErrReceiptUnknownJob, wantStatus: http.StatusServiceUnavailable},
		{name: "order busy", path: path, body: `{}`, err: service.ErrOrderBusy, wantStatus: http.StatusServiceUnavailable},
		{name: "internal", path: path, body: `{}`, err: errors.New("db down"), wantStatus: http.StatusInternalServerError},
		{name: "bad order id", path: "/api/v1/farms/10/orders/x/receipts", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "body too large", path: path, body: strings.Repeat("a", maxReceiptBody+1), wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeReceipts{res: tt.res, err: tt.err}
			r := gin.New()
			ReceiptRoutes(s)(r)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set(HeaderSignature, "sig")
			req.Header.Set(HeaderTimestamp, "123")
			req.Header.Set(HeaderRequestID, "req-9")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Fatal("503 must carry Retry-After")
			}
			if tt.name == "processed" && (s.got.FarmID != 10 || s.got.OrderID != 100 || s.got.Signature != "sig" ||
				s.got.Timestamp != "123" || string(s.got.Body) != `{}` || s.got.RequestID != "req-9" || s.got.Route != "POST "+ReceiptPath) {
				t.Fatalf("input = %+v", s.got)
			}
			if rec.Code == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "db down") {
				t.Fatal("internal error leaked")
			}
		})
	}
}
