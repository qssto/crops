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

// fakeStreamer 记录入参；按配置在开始推送前 / 后返回错误，否则推 events 后 End。
type fakeStreamer struct {
	events   []service.StreamEvent
	errEarly error // 不写 sink 直接返回
	errLate  error // 推完 events 后返回（不发 End）
	gotRun   uint64
	gotLast  int64
}

func (f *fakeStreamer) Stream(_ context.Context, _ model.Scope, runID uint64, lastSeq int64, sink service.StreamSink) error {
	f.gotRun, f.gotLast = runID, lastSeq
	if f.errEarly != nil {
		return f.errEarly
	}
	for _, e := range f.events {
		if err := sink.Event(e); err != nil {
			return err
		}
	}
	if err := sink.Ping(); err != nil {
		return err
	}
	if f.errLate != nil {
		return f.errLate
	}
	return sink.End()
}

func TestEventsHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	grants := map[uint64][]model.Grant{1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleFarmManager}}}}
	auth, err := service.NewAuthService("0123456789abcdef0123456789abcdef", grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	step, call := "plan", uint64(11)
	ev := service.StreamEvent{Seq: 3, EventType: model.EventStepStarted, StepID: &step, ToolCallID: &call,
		ActorType: model.ActorAgent, Payload: json.RawMessage(`{"a":1}`), CreatedAt: now}
	const base = "/api/v1/farms/1/runs/5/events"
	tests := []struct {
		name       string
		path       string
		bearer     bool
		lastHeader string
		streamer   *fakeStreamer
		wantStatus int
		wantLast   int64
		wantBody   string // 非空时须完全相等
		wantError  string
	}{
		{name: "frames", path: base, bearer: true, streamer: &fakeStreamer{events: []service.StreamEvent{ev}},
			wantStatus: http.StatusOK, wantBody: "id: 3\nevent: STEP_STARTED\ndata: " +
				`{"seq":3,"event_type":"STEP_STARTED","step_id":"plan","tool_call_id":11,"actor_type":"agent",` +
				`"payload":{"a":1},"created_at":"2026-10-29T01:00:00Z"}` + "\n\n: ping\n\nevent: end\ndata: {}\n\n"},
		{name: "header cursor wins", path: base + "?last_event_id=2", bearer: true, lastHeader: "5",
			streamer: &fakeStreamer{}, wantStatus: http.StatusOK, wantLast: 5},
		{name: "query cursor", path: base + "?last_event_id=2", bearer: true, streamer: &fakeStreamer{},
			wantStatus: http.StatusOK, wantLast: 2},
		{name: "bad cursor", path: base, bearer: true, lastHeader: "x", streamer: &fakeStreamer{},
			wantStatus: http.StatusBadRequest, wantError: "bad last_event_id"},
		{name: "negative cursor", path: base + "?last_event_id=-1", bearer: true, streamer: &fakeStreamer{},
			wantStatus: http.StatusBadRequest},
		{name: "bad run id", path: "/api/v1/farms/1/runs/0/events", bearer: true, streamer: &fakeStreamer{},
			wantStatus: http.StatusBadRequest, wantError: "bad run_id"},
		{name: "run not found before stream", path: base, bearer: true, streamer: &fakeStreamer{errEarly: model.ErrNotFound},
			wantStatus: http.StatusNotFound},
		{name: "error after start only disconnects", path: base, bearer: true,
			streamer: &fakeStreamer{errLate: errors.New("redis gone")}, wantStatus: http.StatusOK, wantBody: ": ping\n\n"},
		{name: "access_token query on events route", path: base + "?access_token=" + tok, streamer: &fakeStreamer{},
			wantStatus: http.StatusOK},
		{name: "no token", path: base, streamer: &fakeStreamer{}, wantStatus: http.StatusUnauthorized},
		{name: "access_token other farm", path: "/api/v1/farms/2/runs/5/events?access_token=" + tok, streamer: &fakeStreamer{},
			wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewRouter(auth, &fakeAsker{}, EventRoutes(tt.streamer))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.bearer {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			if tt.lastHeader != "" {
				req.Header.Set(lastEventIDHeader, tt.lastHeader)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q\nwant %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantError != "" && !strings.Contains(rec.Body.String(), `"error":"`+tt.wantError+`"`) {
				t.Errorf("body = %s, want error %q", rec.Body.String(), tt.wantError)
			}
			if rec.Code == http.StatusOK {
				if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" || tt.streamer.gotRun != 5 ||
					tt.streamer.gotLast != tt.wantLast {
					t.Errorf("content-type %q run %d last %d", ct, tt.streamer.gotRun, tt.streamer.gotLast)
				}
			}
		})
	}
}

// ?access_token= 只在 SSE 路由生效：其他路由仍须 Authorization 头；有 Authorization 头时以头为准。
func TestFarmAuth_AccessTokenOnlyOnEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	grants := map[uint64][]model.Grant{1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleFarmManager}}}}
	auth, err := service.NewAuthService("0123456789abcdef0123456789abcdef", grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(auth, &fakeAsker{}, EventRoutes(&fakeStreamer{}), RunRoutes(&fakeRunDetailer{}, &fakeMetrics{}))
	for _, tc := range []struct {
		path, header string
		want         int
	}{
		{path: "/api/v1/farms/1/ping?access_token=" + tok, want: http.StatusUnauthorized},
		{path: "/api/v1/farms/1/runs/5?access_token=" + tok, want: http.StatusUnauthorized},
		{path: "/api/v1/farms/1/runs/5/events?access_token=" + tok, header: "Bearer bogus", want: http.StatusUnauthorized},
		{path: "/api/v1/farms/1/runs/5/events?access_token=bogus", want: http.StatusUnauthorized},
		{path: "/api/v1/farms/1/runs/5/events?access_token=" + tok, want: http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("GET %s (auth %q) = %d, want %d", strings.Split(tc.path, "?")[0], tc.header, rec.Code, tc.want)
		}
	}
}
