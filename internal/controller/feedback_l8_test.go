package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

type fakeFeedback struct {
	err   error
	res   service.FeedbackResult
	got   service.FeedbackInput
	calls int
}

func (f *fakeFeedback) Submit(_ context.Context, _ model.Scope, in service.FeedbackInput) (service.FeedbackResult, error) {
	f.got = in
	f.calls++
	return f.res, f.err
}

// P2-L8 反馈接口：持 advice.feedback 的角色可提交；他场建议卡 → 404 + RESOURCE_NOT_IN_SCOPE；非本农场用户 → 403 SCOPE_DENIED；
// 入参非法 400；request_id 重放 200、他场 request_id 409。
func TestFeedbackHandler(t *testing.T) {
	e := newTestEnv(t)
	const body = `{"verdict":"DISLIKE","category":"DOSAGE","comment":"量大","request_id":"fb-1"}`
	tests := []struct {
		name       string
		path, body string
		uid        uint64
		err        error
		replayed   bool
		wantStatus int
	}{
		{name: "created", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 1003, wantStatus: http.StatusCreated},
		{name: "replayed", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 1003, replayed: true, wantStatus: http.StatusOK},
		{name: "auditor denied", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 3001, wantStatus: http.StatusForbidden},
		{name: "bad advice id", path: "/api/v1/farms/1/advices/x/feedback", body: body, uid: 1003, wantStatus: http.StatusBadRequest},
		{name: "bad body", path: "/api/v1/farms/1/advices/7/feedback", body: `{`, uid: 1003, wantStatus: http.StatusBadRequest},
		{name: "invalid", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 1003,
			err: fmt.Errorf("%w: %w", service.ErrBadInput, service.ErrFeedbackInvalid), wantStatus: http.StatusBadRequest},
		{name: "cross farm advice", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 1003,
			err: fmt.Errorf("x: %w", model.ErrNotFound), wantStatus: http.StatusNotFound},
		{name: "request id conflict", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 1003,
			err: model.ErrFeedbackRequestConflict, wantStatus: http.StatusConflict},
		{name: "other farm user", path: "/api/v1/farms/1/advices/7/feedback", body: body, uid: 2001, wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			f := &fakeFeedback{err: tt.err, res: service.FeedbackResult{Replayed: tt.replayed}}
			router := NewRouter(e.guard, &fakeAsker{}, FeedbackRoutes(f))
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", e.bearer(t, tt.uid, 1))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			switch tt.name {
			case "created":
				if f.got.AdviceID != 7 || f.got.Verdict != model.FeedbackDislike || f.got.Category != model.RejectDosage ||
					f.got.RequestID != "fb-1" {
					t.Fatalf("input = %+v", f.got)
				}
			case "cross farm advice":
				if !slices.Equal(e.den.codes(), []string{service.DenyResource}) || e.den.got[0].Target != "advice:7" {
					t.Fatalf("denials = %+v", e.den.got)
				}
			case "other farm user":
				if f.calls != 0 || !slices.Equal(e.den.codes(), []string{service.DenyScope}) {
					t.Fatalf("calls = %d denials = %+v", f.calls, e.den.got)
				}
			}
		})
	}
}

// 应失败（默认拒绝）：矩阵未给 advice.feedback 的角色即使有 advice.read 也 403 ROLE_INSUFFICIENT。
func TestFeedbackHandler_DenyUndeclared(t *testing.T) {
	e := newTestEnv(t)
	perms, err := service.ParsePermissionMatrix([]byte("version: 1\nroles:\n  operator: [ask, advice.read]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e.guard.Perms = perms
	f := &fakeFeedback{}
	router := NewRouter(e.guard, &fakeAsker{}, FeedbackRoutes(f))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/farms/1/advices/7/feedback",
		strings.NewReader(`{"verdict":"ADOPT","request_id":"fb-1"}`))
	req.Header.Set("Authorization", e.bearer(t, 1003, 1))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || f.calls != 0 || !slices.Equal(e.den.codes(), []string{service.DenyRole}) {
		t.Fatalf("status = %d calls = %d denials = %+v", rec.Code, f.calls, e.den.got)
	}
}
