package controller

import (
	"context"
	"encoding/csv"
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

type fakeDetailer struct {
	err    error
	gotID  uint64
	detail service.ApprovalDetail
}

func (f *fakeDetailer) Detail(_ context.Context, _ model.Scope, ticketID uint64) (service.ApprovalDetail, error) {
	f.gotID = ticketID
	return f.detail, f.err
}

type fakeExporter struct {
	err      error
	rows     []model.RejectionReason
	gotScope model.Scope
	from, to time.Time
}

func (f *fakeExporter) Export(_ context.Context, scope model.Scope, from, to time.Time) ([]model.RejectionReason, error) {
	f.gotScope, f.from, f.to = scope, from, to
	return f.rows, f.err
}

// 待审批详情：读权限即可看；票据不在本农场 → 403 + RESOURCE_NOT_IN_SCOPE 审计。
func TestApprovalDetailHandler(t *testing.T) {
	e := newTestEnv(t)
	tests := []struct {
		name       string
		path       string
		uid        uint64
		err        error
		wantStatus int
	}{
		{name: "ok for agronomist", path: "/api/v1/farms/1/approvals/7", uid: 1002, wantStatus: http.StatusOK},
		{name: "ok for operator (read)", path: "/api/v1/farms/1/approvals/7", uid: 1003, wantStatus: http.StatusOK},
		{name: "bad ticket id", path: "/api/v1/farms/1/approvals/x", uid: 1002, wantStatus: http.StatusBadRequest},
		{name: "not in scope", path: "/api/v1/farms/1/approvals/7", uid: 1002, err: notInScope, wantStatus: http.StatusForbidden},
		{name: "other farm user", path: "/api/v1/farms/1/approvals/7", uid: 2001, wantStatus: http.StatusForbidden},
		{name: "snapshot corrupt 500", path: "/api/v1/farms/1/approvals/7", uid: 1002,
			err: fmt.Errorf("x: %w", service.ErrSnapshotCorrupt), wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			d := &fakeDetailer{err: tt.err, detail: service.ApprovalDetail{RiskReasons: []string{model.RiskReasonPesticide}}}
			router := NewRouter(e.guard, &fakeAsker{}, ApprovalDetailRoutes(d))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", e.bearer(t, tt.uid, 1))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			switch tt.name {
			case "ok for agronomist":
				if d.gotID != 7 || !strings.Contains(rec.Body.String(), `"risk_reasons":["PESTICIDE"]`) {
					t.Fatalf("id = %d body = %s", d.gotID, rec.Body.String())
				}
			case "not in scope":
				assertResourceDenied(t, e, rec, "ticket:7")
			case "other farm user":
				if !slices.Equal(e.den.codes(), []string{service.DenyScope}) {
					t.Fatalf("denials = %+v", e.den.got)
				}
			}
		})
	}
}

// 权限红线：导出只给 farm_manager / agronomist / auditor；operator / farmer、他场用户拒绝并审计；时间窗缺失 / 非法 400。
func TestRejectionExportHandler(t *testing.T) {
	e := newTestEnv(t)
	const q = "/api/v1/farms/1/rejections?from=2026-10-01T00:00:00Z&to=2026-10-31T00:00:00Z"
	tests := []struct {
		name       string
		path       string
		uid        uint64
		err        error
		wantStatus int
		wantDenied string
	}{
		{name: "farm manager ok", path: q, uid: 1001, wantStatus: http.StatusOK},
		{name: "agronomist ok", path: q, uid: 1002, wantStatus: http.StatusOK},
		{name: "auditor ok", path: q, uid: 3001, wantStatus: http.StatusOK},
		{name: "operator denied", path: q, uid: 1003, wantStatus: http.StatusForbidden, wantDenied: service.DenyRole},
		{name: "other farm manager denied", path: q, uid: 2001, wantStatus: http.StatusForbidden, wantDenied: service.DenyScope},
		{name: "missing from", path: "/api/v1/farms/1/rejections?to=2026-10-31T00:00:00Z", uid: 1001, wantStatus: http.StatusBadRequest},
		{name: "missing to", path: "/api/v1/farms/1/rejections?from=2026-10-01T00:00:00Z", uid: 1001, wantStatus: http.StatusBadRequest},
		{name: "bad time", path: "/api/v1/farms/1/rejections?from=2026-10-01&to=2026-10-31", uid: 1001, wantStatus: http.StatusBadRequest},
		{name: "span over max", path: q, uid: 1001, err: fmt.Errorf("x: %w", service.ErrBadInput), wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.den.got = nil
			ex := &fakeExporter{err: tt.err}
			router := NewRouter(e.guard, &fakeAsker{}, RejectionRoutes(ex))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
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
			if tt.wantStatus == http.StatusOK && (ex.gotScope.FarmID != 1 ||
				!ex.from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !ex.to.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC))) {
				t.Fatalf("export scope %+v from %v to %v", ex.gotScope, ex.from, ex.to)
			}
		})
	}
}

// CSV：英文列名、BOM、reason_text 引号 / 换行转义、= + - @ 开头加 '（防公式注入）、空文本为空串。
func TestRejectionExportCSV(t *testing.T) {
	e := newTestEnv(t)
	text := func(s string) *string { return &s }
	at := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	ex := &fakeExporter{rows: []model.RejectionReason{
		{ID: 1, TicketID: 7, OrderID: 9, AdviceID: 4, RunID: 5, ReasonCode: model.RejectOther, ReasonText: text("=HYPERLINK(\"x\")"),
			OperatorID: 1002, OperatorRole: model.RoleAgronomist, CreatedAt: at},
		{ID: 2, TicketID: 8, ReasonCode: model.RejectDosage, ReasonText: text("用量偏高,\n需复核"), OperatorRole: model.RoleAgronomist, CreatedAt: at},
		{ID: 3, ReasonCode: model.RejectTiming, ReasonText: text("+1"), CreatedAt: at},
		{ID: 4, ReasonCode: model.RejectProduct, ReasonText: text("-2"), CreatedAt: at},
		{ID: 5, ReasonCode: model.RejectEvidence, ReasonText: text("@cmd"), CreatedAt: at},
		{ID: 6, ReasonCode: model.RejectDosage, CreatedAt: at},
	}}
	router := NewRouter(e.guard, &fakeAsker{}, RejectionRoutes(ex))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/farms/1/rejections?from=2026-10-01T00:00:00Z&to=2026-10-31T00:00:00Z", nil)
	req.Header.Set("Authorization", e.bearer(t, 3001, 1))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("status = %d type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body, ok := strings.CutPrefix(rec.Body.String(), csvBOM)
	if !ok {
		t.Fatal("missing UTF-8 BOM")
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recs[0], rejectionCSVHeader) || len(recs) != 7 {
		t.Fatalf("header %v rows %d", recs[0], len(recs))
	}
	want := []string{"1", "7", "9", "4", "5", "OTHER", `'=HYPERLINK("x")`, "1002", "agronomist", "2026-10-02T03:04:05Z"}
	if !slices.Equal(recs[1], want) {
		t.Fatalf("row 1 = %q, want %q", recs[1], want)
	}
	var texts []string
	for _, r := range recs[2:] {
		texts = append(texts, r[6])
	}
	if wantTexts := []string{"用量偏高,\n需复核", "'+1", "'-2", "'@cmd", ""}; !slices.Equal(texts, wantTexts) {
		t.Fatalf("texts = %q, want %q", texts, wantTexts)
	}
	if !strings.Contains(body, "\"用量偏高,\n需复核\"") {
		t.Fatalf("comma/newline text not quoted: %s", body)
	}
}
