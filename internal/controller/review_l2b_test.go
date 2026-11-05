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

type fakeReviewAdmin struct {
	err      error
	calls    int
	gotActor model.Scope
	gotID    uint64
	gotQuery service.ReviewListQuery
	gotIn    model.ReviewSubmit
}

func (f *fakeReviewAdmin) List(_ context.Context, actor model.Scope, q service.ReviewListQuery) (model.ReviewTaskPage, error) {
	f.calls++
	f.gotActor, f.gotQuery = actor, q
	if f.err != nil {
		return model.ReviewTaskPage{}, f.err
	}
	return model.ReviewTaskPage{Items: []model.ReviewTaskView{{ID: 1, Holdout: true}}, Total: 1, Page: 1, PageSize: 20}, nil
}

func (f *fakeReviewAdmin) Get(_ context.Context, actor model.Scope, id uint64) (model.ReviewTaskView, error) {
	f.calls++
	f.gotActor, f.gotID = actor, id
	if f.err != nil {
		return model.ReviewTaskView{}, f.err
	}
	return model.ReviewTaskView{ID: id, Holdout: true}, nil
}

func (f *fakeReviewAdmin) Submit(_ context.Context, actor model.Scope, id uint64, in model.ReviewSubmit) (model.ReviewSubmitResult, error) {
	f.calls++
	f.gotActor, f.gotID, f.gotIn = actor, id, in
	if f.err != nil {
		return model.ReviewSubmitResult{}, f.err
	}
	return model.ReviewSubmitResult{Task: model.ReviewTaskView{ID: id, Status: model.ReviewDone}}, nil
}

type reviewCase struct {
	name         string
	method, path string
	auth, body   string
	svcErr       error
	wantStatus   int
	wantError    string
}

func serveReview(t *testing.T, e *testEnv, s ReviewAdmin, tc reviewCase) *httptest.ResponseRecorder {
	t.Helper()
	g := e.guard
	g.Denials, g.Logger = &recDenier{}, slog.New(slog.NewTextHandler(io.Discard, nil))
	r := gin.New()
	MountReviewAdmin(r, g, s)
	req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
	if tc.auth != "" {
		req.Header.Set("Authorization", tc.auth)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != tc.wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
	}
	if tc.wantError != "" {
		var eb errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil || eb.Error != tc.wantError {
			t.Fatalf("body = %s, want error %q", rec.Body.String(), tc.wantError)
		}
	}
	return rec
}

func reviewBearer(t *testing.T, e *testEnv) func(uint64) string {
	return func(uid uint64) string {
		tenant := uint64(1)
		if uid >= 9001 {
			tenant = 0
		}
		return e.bearer(t, uid, tenant)
	}
}

const (
	reviewListPath   = "/api/v1/admin/review-tasks"
	reviewGetPath    = "/api/v1/admin/review-tasks/7"
	reviewSubmitPath = "/api/v1/admin/review-tasks/7/submit"
	reviewOKBody     = `{"score":2,"reason":"ok"}`
)

// 红线：kb_expert 以外一律 403 且不进 service（平台 admin → ROLE_INSUFFICIENT，农场用户 → SCOPE_DENIED）。
func TestReviewAdmin_NonExpertDenied(t *testing.T) {
	e := newTestEnvWith(t, kbExpertStore{})
	bearer := reviewBearer(t, e)
	var cases []reviewCase
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, reviewListPath}, {http.MethodGet, reviewGetPath}, {http.MethodPost, reviewSubmitPath},
	} {
		cases = append(cases,
			reviewCase{"platform_admin " + ep.path, ep.method, ep.path, bearer(9001), reviewOKBody, nil, http.StatusForbidden, "ROLE_INSUFFICIENT"},
			reviewCase{"no token " + ep.path, ep.method, ep.path, "", reviewOKBody, nil, http.StatusUnauthorized, ""})
		for _, uid := range []uint64{1001, 1002, 1003, 3001} {
			cases = append(cases, reviewCase{fmt.Sprintf("farm user %d %s", uid, ep.path), ep.method, ep.path, bearer(uid),
				reviewOKBody, nil, http.StatusForbidden, "SCOPE_DENIED"})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeReviewAdmin{}
			serveReview(t, e, fa, tc)
			if fa.calls != 0 {
				t.Fatal("denied request reached service")
			}
		})
	}
}

// 应失败：冲突未回 409、非法输入未回 400、连接关闭未回 503。
func TestReviewAdmin_Expert(t *testing.T) {
	e := newTestEnvWith(t, kbExpertStore{})
	me := reviewBearer(t, e)(9002)
	wrap := func(e error) error { return fmt.Errorf("svc: %w", e) }
	cases := []reviewCase{
		{"list ok", http.MethodGet, reviewListPath + "?task_type=HUMAN_QUEUE&status=OPEN&page=2&page_size=5", me, "", nil, http.StatusOK, ""},
		{"list bad page", http.MethodGet, reviewListPath + "?page=x", me, "", nil, http.StatusBadRequest, ""},
		{"list bad page_size", http.MethodGet, reviewListPath + "?page_size=1.5", me, "", nil, http.StatusBadRequest, ""},
		{"list bad filter", http.MethodGet, reviewListPath + "?status=LOST", me, "", wrap(service.ErrBadInput), http.StatusBadRequest, ""},
		{"get ok", http.MethodGet, reviewGetPath, me, "", nil, http.StatusOK, ""},
		{"get bad id", http.MethodGet, "/api/v1/admin/review-tasks/x", me, "", nil, http.StatusBadRequest, ""},
		{"get zero id", http.MethodGet, "/api/v1/admin/review-tasks/0", me, "", nil, http.StatusBadRequest, ""},
		{"get not found", http.MethodGet, reviewGetPath, me, "", wrap(model.ErrNotFound), http.StatusNotFound, ""},
		{"submit ok", http.MethodPost, reviewSubmitPath, me, reviewOKBody, nil, http.StatusOK, ""},
		{"submit conflict 409", http.MethodPost, reviewSubmitPath, me, reviewOKBody, wrap(service.ErrReviewConflict), http.StatusConflict, "REVIEW_CONFLICT"},
		{"submit invalid 400", http.MethodPost, reviewSubmitPath, me, reviewOKBody,
			fmt.Errorf("svc: %w: %w", service.ErrBadInput, model.ErrReviewSubmit), http.StatusBadRequest, "REVIEW_SUBMIT_INVALID"},
		{"submit pii 400", http.MethodPost, reviewSubmitPath, me, reviewOKBody,
			fmt.Errorf("svc: %w: %w", service.ErrBadInput, service.ErrReviewPII), http.StatusBadRequest, "REVIEW_PII"},
		{"submit bad json", http.MethodPost, reviewSubmitPath, me, `{"score":`, nil, http.StatusBadRequest, ""},
		{"submit unknown field", http.MethodPost, reviewSubmitPath, me, `{"score":2,"judge_score":3}`, nil, http.StatusBadRequest, ""},
		{"submit trailing", http.MethodPost, reviewSubmitPath, me, `{"score":2}{}`, nil, http.StatusBadRequest, ""},
		{"submit too large", http.MethodPost, reviewSubmitPath, me,
			`{"decision":"MERGE","expect":"` + strings.Repeat("a", maxReviewBody) + `"}`, nil, http.StatusBadRequest, ""},
		{"disabled 503", http.MethodGet, reviewListPath, me, "", wrap(service.ErrReviewUnavailable), http.StatusServiceUnavailable,
			"EVAL_REVIEW_DISABLED"},
		{"internal 500", http.MethodGet, reviewGetPath, me, "", errors.New("db down"), http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeReviewAdmin{err: tc.svcErr}
			rec := serveReview(t, e, fa, tc)
			switch tc.name {
			case "list ok":
				if q := fa.gotQuery; q.TaskType != model.ReviewHumanQueue || q.Status != model.ReviewOpen || q.Page != 2 || q.PageSize != 5 ||
					fa.gotActor.UserID != 9002 || fa.gotActor.TenantID != 0 {
					t.Fatalf("query %+v actor %+v", q, fa.gotActor)
				}
			case "submit ok":
				if fa.gotID != 7 || fa.gotIn.Score == nil || *fa.gotIn.Score != 2 || fa.gotIn.Reason != "ok" ||
					!strings.Contains(rec.Body.String(), `"replayed":false`) {
					t.Fatalf("id %d in %+v body %s", fa.gotID, fa.gotIn, rec.Body.String())
				}
			}
		})
	}
}

// 红线：真实的 DisabledReviewService 挂在路由上：kb_expert → 503 EVAL_REVIEW_DISABLED，其余仍 403，不 panic。
func TestReviewAdmin_DisabledService(t *testing.T) {
	e := newTestEnvWith(t, kbExpertStore{})
	bearer := reviewBearer(t, e)
	svc := service.DisabledReviewService()
	for _, tc := range []reviewCase{
		{"list", http.MethodGet, reviewListPath, bearer(9002), "", nil, http.StatusServiceUnavailable, "EVAL_REVIEW_DISABLED"},
		{"get", http.MethodGet, reviewGetPath, bearer(9002), "", nil, http.StatusServiceUnavailable, "EVAL_REVIEW_DISABLED"},
		{"submit", http.MethodPost, reviewSubmitPath, bearer(9002), reviewOKBody, nil, http.StatusServiceUnavailable, "EVAL_REVIEW_DISABLED"},
		{"admin still 403", http.MethodGet, reviewListPath, bearer(9001), "", nil, http.StatusForbidden, "ROLE_INSUFFICIENT"},
		{"farm still 403", http.MethodGet, reviewListPath, bearer(1001), "", nil, http.StatusForbidden, "SCOPE_DENIED"},
	} {
		t.Run(tc.name, func(t *testing.T) { serveReview(t, e, svc, tc) })
	}
}
