package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fakeAsker 记录收到的 Scope / 输入，按配置返回。
type fakeAsker struct {
	askErr   error
	runErr   error
	gotScope model.Scope
	gotIn    service.AskInput
}

func (f *fakeAsker) Ask(ctx context.Context, scope model.Scope, in service.AskInput) (uint64, error) {
	f.gotScope, f.gotIn = scope, in
	if f.askErr != nil {
		return 0, f.askErr
	}
	return 77, nil
}

func (f *fakeAsker) GetRun(_ context.Context, scope model.Scope, runID uint64) (service.RunView, error) {
	f.gotScope = scope
	if f.runErr != nil {
		return service.RunView{}, f.runErr
	}
	return service.RunView{Run: model.RunSummary{RunID: runID, Status: model.RunRunning}}, nil
}

func TestAskAndRunHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
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
	const okBody = `{"field_id":3,"text":"稻飞虱要不要打药","farm_id":2}`

	tests := []struct {
		name       string
		method     string
		path       string
		auth       string
		reqID      string
		body       string
		asker      *fakeAsker
		wantStatus int
		wantReqID  string
	}{
		{name: "ask 202", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer, reqID: "req-1",
			body: okBody, asker: &fakeAsker{}, wantStatus: http.StatusAccepted, wantReqID: "req-1"},
		{name: "ask no token 401", method: http.MethodPost, path: "/api/v1/farms/1/ask", body: okBody,
			asker: &fakeAsker{}, wantStatus: http.StatusUnauthorized},
		{name: "ask other farm 403", method: http.MethodPost, path: "/api/v1/farms/2/ask", auth: bearer, body: okBody,
			asker: &fakeAsker{}, wantStatus: http.StatusForbidden},
		{name: "ask bad json 400", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer, body: `{"field_id":`,
			asker: &fakeAsker{}, wantStatus: http.StatusBadRequest},
		{name: "ask bad request id 400", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer,
			reqID: "bad id\n", body: okBody, asker: &fakeAsker{}, wantStatus: http.StatusBadRequest},
		{name: "ask service bad input 400", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer, body: okBody,
			asker: &fakeAsker{askErr: service.ErrBadInput}, wantStatus: http.StatusBadRequest},
		{name: "ask field out of scope 403", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer, body: okBody,
			asker: &fakeAsker{askErr: service.ErrForbidden}, wantStatus: http.StatusForbidden},
		{name: "ask rate limited 429", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer, body: okBody,
			asker: &fakeAsker{askErr: service.ErrRateLimited}, wantStatus: http.StatusTooManyRequests},
		{name: "ask internal 500", method: http.MethodPost, path: "/api/v1/farms/1/ask", auth: bearer, body: okBody,
			asker: &fakeAsker{askErr: errors.New("db down")}, wantStatus: http.StatusInternalServerError},
		{name: "run 200", method: http.MethodGet, path: "/api/v1/farms/1/runs/77", auth: bearer,
			asker: &fakeAsker{}, wantStatus: http.StatusOK},
		{name: "run bad id 400", method: http.MethodGet, path: "/api/v1/farms/1/runs/x", auth: bearer,
			asker: &fakeAsker{}, wantStatus: http.StatusBadRequest},
		{name: "run not found 404", method: http.MethodGet, path: "/api/v1/farms/1/runs/78", auth: bearer,
			asker: &fakeAsker{runErr: model.ErrNotFound}, wantStatus: http.StatusNotFound},
		{name: "run no token 401", method: http.MethodGet, path: "/api/v1/farms/1/runs/77",
			asker: &fakeAsker{}, wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewRouter(auth, tt.asker)
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			if tt.reqID != "" {
				req.Header.Set(HeaderRequestID, tt.reqID)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantReqID != "" && rec.Header().Get(HeaderRequestID) != tt.wantReqID {
				t.Fatalf("X-Request-Id = %q, want %q", rec.Header().Get(HeaderRequestID), tt.wantReqID)
			}
			if tt.wantStatus == http.StatusAccepted {
				var got askResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.RunID != 77 {
					t.Fatalf("body = %s, err %v", rec.Body.String(), err)
				}
				// 农场只认路径 + Scope，请求体 farm_id 被忽略
				if tt.asker.gotScope.FarmID != 1 || tt.asker.gotIn.FieldID != 3 {
					t.Fatalf("scope=%+v in=%+v", tt.asker.gotScope, tt.asker.gotIn)
				}
			}
		})
	}
}

func TestRequestID_Generated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RequestID(), func(c *gin.Context) {
		c.String(http.StatusOK, service.RequestIDFrom(c.Request.Context()))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	got := rec.Header().Get(HeaderRequestID)
	if len(got) != 2*requestIDBytes || rec.Body.String() != got {
		t.Fatalf("header=%q body=%q", got, rec.Body.String())
	}
}
