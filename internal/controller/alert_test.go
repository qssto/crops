package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type fakeAlertLister struct {
	err      error
	gotScope model.Scope
	gotQuery service.AlertQuery
	calls    int
}

func (f *fakeAlertLister) List(_ context.Context, scope model.Scope, q service.AlertQuery) (service.AlertList, error) {
	f.calls++
	f.gotScope, f.gotQuery = scope, q
	if f.err != nil {
		return service.AlertList{}, f.err
	}
	return service.AlertList{Items: []service.AlertItem{{ID: 1, Alert: model.AlertAdviceNoCitation, RunID: 5,
		RunLink: "/api/v1/farms/1/runs/5"}}}, nil
}

// P1 L7-B 告警列表：读权限即可；他场用户拒绝并审计；参数非法 400（应失败用例）。
func TestAlertListHandler(t *testing.T) {
	e := newTestEnv(t)
	tests := []struct {
		name       string
		path       string
		uid        uint64
		err        error
		wantStatus int
		wantQuery  service.AlertQuery
	}{
		{name: "defaults", path: "/api/v1/farms/1/alerts", uid: 1003, wantStatus: http.StatusOK},
		{name: "filtered", path: "/api/v1/farms/1/alerts?name=advice_no_citation&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z&limit=10",
			uid: 1001, wantStatus: http.StatusOK, wantQuery: service.AlertQuery{Name: model.AlertAdviceNoCitation,
				From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Limit: 10}},
		{name: "other farm user", path: "/api/v1/farms/1/alerts", uid: 2001, wantStatus: http.StatusForbidden},
		{name: "bad from", path: "/api/v1/farms/1/alerts?from=yesterday", uid: 1001, wantStatus: http.StatusBadRequest},
		{name: "bad to", path: "/api/v1/farms/1/alerts?to=2026-10-02", uid: 1001, wantStatus: http.StatusBadRequest},
		{name: "bad limit", path: "/api/v1/farms/1/alerts?limit=-1", uid: 1001, wantStatus: http.StatusBadRequest},
		{name: "cursor passed through", path: "/api/v1/farms/1/alerts?cursor=1790000000000_42", uid: 1001,
			wantStatus: http.StatusOK, wantQuery: service.AlertQuery{Cursor: "1790000000000_42"}},
		{name: "bad cursor", path: "/api/v1/farms/1/alerts?cursor=zzz", uid: 1001,
			err: fmt.Errorf("x: %w", service.ErrBadInput), wantStatus: http.StatusBadRequest},
		{name: "service rejects", path: "/api/v1/farms/1/alerts?name=nope", uid: 1001,
			err: fmt.Errorf("x: %w", service.ErrBadInput), wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			l := &fakeAlertLister{err: tt.err}
			router := NewRouter(e.guard, &fakeAsker{}, AlertRoutes(l))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", e.bearer(t, tt.uid, 1))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			switch {
			case tt.name == "other farm user":
				if l.calls != 0 || !slices.Equal(e.den.codes(), []string{service.DenyScope}) {
					t.Fatalf("calls = %d denials = %+v", l.calls, e.den.got)
				}
			case tt.wantStatus == http.StatusOK:
				if l.gotScope.FarmID != 1 || l.gotQuery != tt.wantQuery {
					t.Fatalf("scope = %+v query = %+v, want %+v", l.gotScope, l.gotQuery, tt.wantQuery)
				}
				if !strings.Contains(rec.Body.String(), `"run_link":"/api/v1/farms/1/runs/5"`) {
					t.Fatalf("body = %s", rec.Body.String())
				}
			case tt.err == nil && l.calls != 0:
				t.Fatalf("service called on bad input")
			}
		})
	}
}
