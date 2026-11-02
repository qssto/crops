package devicesim_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/adapter/devicesim"
	"agri-steward/internal/adapter/devicesim/sim"
	"agri-steward/internal/model"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

var allCaps = adapter.Capabilities{Cancel: true, IdemKey: true, ExternalQuery: true}

func start(t *testing.T) (*sim.Server, *devicesim.Client, string) {
	t.Helper()
	s, err := sim.New(fixedClock{t0}, sim.DeviceFile{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c, err := devicesim.New(devicesim.Config{BaseURL: ts.URL + "/", Timeout: 2 * time.Second, Caps: allCaps})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, ts.URL
}

func order(id uint64) model.WorkOrder {
	return model.WorkOrder{OrderID: id, TenantID: 1, FarmID: 1, RunID: 9, FieldID: 101, DeviceType: "uav",
		WorkType: model.WorkSpray, Payload: json.RawMessage(`{"product":"吡虫啉","area_mu":12}`),
		TimeWindow: model.TimeWindow{From: t0, To: t0.Add(4 * time.Hour)}}
}

func idem(id uint64) string { return "order.dispatch:" + strconv.FormatUint(id, 10) + ":7:1" }

func mu(v float64) *float64 { return &v }

// step 是一次调用后的期望：标准回执状态 / 失败阶段，以及按映射表得到的作业单处置。
type step struct {
	call   string // dispatch / query / query_ext / cancel
	status model.AttemptStatus
	kind   model.FailureKind
	effect model.ReceiptEffect
	to     model.WorkOrderStatus
}

// TestEndToEnd_ScriptedSequences：起 devicesim（httptest）跑 Dispatch → QueryStatus → Cancel，
// 每一步回执经 model.MapReceipt 得到的处置与《回执映射表》一致（P1 §6.3 验收 1 的自动化部分）。
func TestEndToEnd_ScriptedSequences(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		script *devicesim.Script
		steps  []step
	}{
		{name: "default success", steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptExecuting, effect: model.EffectTransition, to: model.OrderExecuting},
			{call: "query_ext", status: model.AttemptDone, effect: model.EffectTransition, to: model.OrderDone},
			{call: "query", status: model.AttemptDone, effect: model.EffectTransition, to: model.OrderDone},
			{call: "cancel", status: model.AttemptDone, effect: model.EffectTransition, to: model.OrderDone}, // 已终结：返回当前状态
		}},
		{name: "cancel while running", steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptExecuting, effect: model.EffectTransition, to: model.OrderExecuting},
			{call: "cancel", status: model.AttemptCanceled, effect: model.EffectTransition, to: model.OrderCanceled},
			{call: "query", status: model.AttemptCanceled, effect: model.EffectTransition, to: model.OrderCanceled},
		}},
		{name: "partial", script: &devicesim.Script{Steps: []devicesim.Step{{State: devicesim.JobRunning},
			{State: devicesim.JobPartial, DoneMu: mu(7.5)}}}, steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptExecuting, effect: model.EffectTransition, to: model.OrderExecuting},
			{call: "query", status: model.AttemptPartiallyDone, effect: model.EffectTransition, to: model.OrderPartiallyDone},
		}},
		{name: "fail before start", script: &devicesim.Script{Steps: []devicesim.Step{
			{State: devicesim.JobFailed, FailStage: devicesim.StageBeforeStart}}}, steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptFailed, kind: model.FailureNotExecuted, effect: model.EffectRetryOrFail,
				to: model.OrderFailed},
		}},
		{name: "fail in progress", script: &devicesim.Script{Steps: []devicesim.Step{{State: devicesim.JobRunning},
			{State: devicesim.JobFailed, FailStage: devicesim.StageInProgress}}}, steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptExecuting, effect: model.EffectTransition, to: model.OrderExecuting},
			{call: "query", status: model.AttemptFailed, kind: model.FailureInExecution, effect: model.EffectTransition,
				to: model.OrderFailed},
		}},
		{name: "fail unknown", script: &devicesim.Script{Steps: []devicesim.Step{
			{State: devicesim.JobFailed, FailStage: devicesim.StageUnknown}}}, steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptFailed, kind: model.FailureUnknown, effect: model.EffectReconcile},
		}},
		{name: "no receipt", script: &devicesim.Script{Steps: []devicesim.Step{}}, steps: []step{
			{call: "dispatch", status: model.AttemptAccepted, effect: model.EffectNone},
			{call: "query", status: model.AttemptAccepted, effect: model.EffectNone}, // 一直停在受理：由 L4 扫描置 NO_RECEIPT
			{call: "query", status: model.AttemptAccepted, effect: model.EffectNone},
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c, _ := start(t)
			o := order(uint64(100 + i))
			if tc.script != nil {
				if err := s.SetScript(o.OrderID, *tc.script); err != nil {
					t.Fatal(err)
				}
			}
			var job string
			for j, st := range tc.steps {
				var r model.DeviceReceipt
				var err error
				switch st.call {
				case "dispatch":
					r, err = c.Dispatch(ctx, o, idem(o.OrderID))
					job = r.DeviceJobID
				case "query":
					r, err = c.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: job})
				case "query_ext":
					r, err = c.QueryStatus(ctx, adapter.StatusRef{IdemKey: idem(o.OrderID)})
				case "cancel":
					r, err = c.Cancel(ctx, job)
				}
				if err != nil {
					t.Fatalf("step %d %s: %v", j, st.call, err)
				}
				if r.Status != st.status || r.FailureKind != st.kind || r.DeviceJobID != job || !r.DeviceTime.Equal(t0) {
					t.Fatalf("step %d %s = %+v, want %s/%s job=%s", j, st.call, r, st.status, st.kind, job)
				}
				rule, err := model.MapReceipt(r.Status, r.FailureKind, r.DoneAreaMu)
				if err != nil || rule.Effect != st.effect || rule.To != st.to {
					t.Fatalf("step %d mapping = %+v, %v; want %s → %q", j, rule, err, st.effect, st.to)
				}
			}
		})
	}
}

// TestDispatch_Idempotent：同一 idem_key 重复下发返回同一任务、不推进状态；同键不同作业单 → 409 ErrRejected。
func TestDispatch_Idempotent(t *testing.T) {
	ctx := context.Background()
	_, c, _ := start(t)
	o := order(1)
	first, err := c.Dispatch(ctx, o, idem(1))
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Dispatch(ctx, o, idem(1))
	if err != nil || again.DeviceJobID != first.DeviceJobID || again.Status != model.AttemptAccepted {
		t.Fatalf("retry = %+v, %v; want same job %s", again, err, first.DeviceJobID)
	}
	other, err := c.Dispatch(ctx, o, "order.dispatch:1:7:2") // 新 attempt：新键新任务
	if err != nil || other.DeviceJobID == first.DeviceJobID {
		t.Fatalf("new attempt = %+v, %v", other, err)
	}
	clash := order(2)
	if _, err := c.Dispatch(ctx, clash, idem(1)); !errors.Is(err, adapter.ErrRejected) ||
		!strings.Contains(err.Error(), devicesim.CodeIdemClash) {
		t.Fatalf("same key other order err = %v, want ErrRejected IDEM_CONFLICT", err)
	}
	if _, err := c.Dispatch(ctx, o, ""); !errors.Is(err, adapter.ErrRejected) {
		t.Fatalf("empty key err = %v", err)
	}
}

// TestClient_ErrorPaths：超时 / 5xx / 拒收 / 不合规回执 / 未知任务 / 能力未声明的错误分类。
func TestClient_ErrorPaths(t *testing.T) {
	ctx := context.Background()
	s, c, base := start(t)
	scripts := map[uint64]devicesim.Script{
		11: {Submit: devicesim.SubmitError},
		12: {Submit: devicesim.SubmitReject},
		13: {Submit: devicesim.SubmitBadReceipt},
		14: {DelayMS: 500, Steps: []devicesim.Step{}},
	}
	for id, sc := range scripts {
		if err := s.SetScript(id, sc); err != nil {
			t.Fatal(err)
		}
	}
	short, err := devicesim.New(devicesim.Config{BaseURL: base, Timeout: 50 * time.Millisecond, Caps: allCaps})
	if err != nil {
		t.Fatal(err)
	}
	noExt, err := devicesim.New(devicesim.Config{BaseURL: base, Caps: adapter.Capabilities{IdemKey: true}})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := devicesim.New(devicesim.Config{BaseURL: "http://127.0.0.1:1", Caps: allCaps})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		call func() (model.DeviceReceipt, error)
		want error
	}{
		{"5xx", func() (model.DeviceReceipt, error) { return c.Dispatch(ctx, order(11), idem(11)) }, adapter.ErrUnavailable},
		{"reject", func() (model.DeviceReceipt, error) { return c.Dispatch(ctx, order(12), idem(12)) }, adapter.ErrRejected},
		{"bad receipt", func() (model.DeviceReceipt, error) { return c.Dispatch(ctx, order(13), idem(13)) }, adapter.ErrBadReceipt},
		{"timeout", func() (model.DeviceReceipt, error) { return short.Dispatch(ctx, order(14), idem(14)) }, adapter.ErrTimeout},
		{"connection refused", func() (model.DeviceReceipt, error) { return dead.Dispatch(ctx, order(15), idem(15)) },
			adapter.ErrUnavailable},
		{"unknown job", func() (model.DeviceReceipt, error) {
			return c.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "sim-404"})
		}, adapter.ErrJobNotFound},
		{"unknown external no", func() (model.DeviceReceipt, error) {
			return c.QueryStatus(ctx, adapter.StatusRef{IdemKey: "nope"})
		}, adapter.ErrJobNotFound},
		{"empty ref", func() (model.DeviceReceipt, error) { return c.QueryStatus(ctx, adapter.StatusRef{}) }, adapter.ErrRejected},
		{"external query not declared", func() (model.DeviceReceipt, error) {
			return noExt.QueryStatus(ctx, adapter.StatusRef{IdemKey: idem(1)})
		}, adapter.ErrCapability},
		{"cancel not declared", func() (model.DeviceReceipt, error) { return noExt.Cancel(ctx, "sim-1") }, adapter.ErrCapability},
		{"cancel empty job", func() (model.DeviceReceipt, error) { return c.Cancel(ctx, "") }, adapter.ErrRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := tc.call()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if r != (model.DeviceReceipt{}) {
				t.Fatalf("receipt on error = %+v, want zero", r)
			}
		})
	}
	// 被拒收 / 5xx 的单未建任务：外部单号查不到。
	if _, err := c.QueryStatus(ctx, adapter.StatusRef{IdemKey: idem(11)}); !errors.Is(err, adapter.ErrJobNotFound) {
		t.Fatalf("5xx must not create a job: %v", err)
	}
}

// TestClient_BadPlatformBodies：平台返回 2xx 但回执自相矛盾 / 非 JSON 时判 ErrBadReceipt。
func TestClient_BadPlatformBodies(t *testing.T) {
	ctx := context.Background()
	bodies := map[string]string{
		"not json":            `<html>`,
		"unknown fail stage":  `{"job_id":"j","state":"failed","fail_stage":"mid_air"}`,
		"failed without kind": `{"job_id":"j","state":"failed"}`,
		"partial without mu":  `{"job_id":"j","state":"partial"}`,
		"done without job id": `{"state":"finished"}`,
		"stage on finished":   `{"job_id":"j","state":"finished","fail_stage":"in_progress"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()
			c, err := devicesim.New(devicesim.Config{BaseURL: ts.URL, Caps: allCaps})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "j"}); !errors.Is(err, adapter.ErrBadReceipt) {
				t.Fatalf("err = %v, want ErrBadReceipt", err)
			}
		})
	}
}

// TestDispatch_TimeoutAfterAccept：平台先受理建任务、再延迟响应 → Dispatch 判 ErrTimeout（结果未知），
// 随后按 idem_key 查询能查到同一任务，重下发同键也返回同一任务（对账路径可用）。
func TestDispatch_TimeoutAfterAccept(t *testing.T) {
	ctx := context.Background()
	s, c, base := start(t)
	o := order(21)
	if err := s.SetScript(o.OrderID, devicesim.Script{AcceptDelayMS: 300, Steps: []devicesim.Step{}}); err != nil {
		t.Fatal(err)
	}
	short, err := devicesim.New(devicesim.Config{BaseURL: base, Timeout: 50 * time.Millisecond, Caps: allCaps})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := short.Dispatch(ctx, o, idem(o.OrderID)); !errors.Is(err, adapter.ErrTimeout) || r != (model.DeviceReceipt{}) {
		t.Fatalf("dispatch = %+v, %v; want ErrTimeout", r, err)
	}
	byKey, err := c.QueryStatus(ctx, adapter.StatusRef{IdemKey: idem(o.OrderID)})
	if err != nil || byKey.DeviceJobID == "" || byKey.Status != model.AttemptAccepted {
		t.Fatalf("query by idem_key = %+v, %v; want the accepted job", byKey, err)
	}
	byID, err := c.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: byKey.DeviceJobID})
	if err != nil || byID.DeviceJobID != byKey.DeviceJobID {
		t.Fatalf("query by job id = %+v, %v", byID, err)
	}
	again, err := c.Dispatch(ctx, o, idem(o.OrderID))
	if err != nil || again.DeviceJobID != byKey.DeviceJobID {
		t.Fatalf("re-dispatch = %+v, %v; want job %s", again, err, byKey.DeviceJobID)
	}
	if err := s.SetScript(1, devicesim.Script{AcceptDelayMS: -1}); err == nil {
		t.Fatal("negative accept_delay_ms must be rejected")
	}
}

// TestClient_JobIdentityMismatch：平台返回的任务与请求的 idem_key / order_id / job_id 不一致 → ErrBadReceipt（防串单）。
func TestClient_JobIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	serve := func(body string) *devicesim.Client {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(ts.Close)
		c, err := devicesim.New(devicesim.Config{BaseURL: ts.URL, Caps: allCaps})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	job := func(id, ext string, orderID uint64) string {
		return `{"job_id":"` + id + `","external_no":"` + ext + `","order_id":` + strconv.FormatUint(orderID, 10) +
			`,"state":"accepted","updated_at":"2026-07-15T01:00:00Z"}`
	}
	o := order(5)
	cases := map[string]func() (model.DeviceReceipt, error){
		"dispatch other external_no": func() (model.DeviceReceipt, error) {
			return serve(job("sim-1", idem(6), 5)).Dispatch(ctx, o, idem(5))
		},
		"dispatch other order_id": func() (model.DeviceReceipt, error) {
			return serve(job("sim-1", idem(5), 6)).Dispatch(ctx, o, idem(5))
		},
		"query by key other external_no": func() (model.DeviceReceipt, error) {
			return serve(job("sim-1", idem(6), 6)).QueryStatus(ctx, adapter.StatusRef{IdemKey: idem(5)})
		},
		"query by id other job_id": func() (model.DeviceReceipt, error) {
			return serve(job("sim-2", idem(5), 5)).QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "sim-1"})
		},
		"cancel other job_id": func() (model.DeviceReceipt, error) {
			return serve(job("sim-2", idem(5), 5)).Cancel(ctx, "sim-1")
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if r, err := call(); !errors.Is(err, adapter.ErrBadReceipt) || r != (model.DeviceReceipt{}) {
				t.Fatalf("= %+v, %v; want ErrBadReceipt", r, err)
			}
		})
	}
	if r, err := serve(job("sim-1", idem(5), 5)).Dispatch(ctx, o, idem(5)); err != nil || r.DeviceJobID != "sim-1" {
		t.Fatalf("matching job = %+v, %v", r, err)
	}
}

// TestClient_BodyReadAndLimit：2xx 响应头后 body 读失败 → ErrTimeout（结果未知）；非 2xx 读失败 → ErrUnavailable；
// 2xx body 超过 1 MiB → ErrBadReceipt；非 2xx 超限仍按状态码归类。
func TestClient_BodyReadAndLimit(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat(" ", 1<<20) + `{}`
	cases := map[string]struct {
		h    http.HandlerFunc
		want error
	}{
		"2xx truncated body": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"job_id":`)) // 声明 100 字节只写 10 字节，服务端关连接 → 客户端读到 unexpected EOF
		}, adapter.ErrTimeout},
		"5xx truncated body": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":`))
		}, adapter.ErrUnavailable},
		"2xx oversized body": {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(big)) }, adapter.ErrBadReceipt},
		"4xx oversized body": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(big))
		}, adapter.ErrRejected},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(tc.h)
			defer ts.Close()
			c, err := devicesim.New(devicesim.Config{BaseURL: ts.URL, Caps: allCaps})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Dispatch(ctx, order(7), idem(7)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNew_Config(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "http://", "::"} {
		if _, err := devicesim.New(devicesim.Config{BaseURL: u}); err == nil {
			t.Fatalf("base_url %q must be rejected", u)
		}
	}
	c, err := devicesim.New(devicesim.Config{BaseURL: "http://127.0.0.1:8090", Caps: allCaps})
	if err != nil || c.Name() != devicesim.Name || c.Capabilities() != allCaps {
		t.Fatalf("New = %v, %v", c, err)
	}
	if err := adapter.CheckCapabilities(c.Capabilities()); err != nil {
		t.Fatalf("devicesim must pass the open rule: %v", err)
	}
	body, err := c.EncodeCommand(order(3), idem(3))
	if err != nil {
		t.Fatal(err)
	}
	var req devicesim.SubmitRequest
	if err := json.Unmarshal(body, &req); err != nil || req.IdemKey != idem(3) || req.OrderID != 3 || req.RunID != 9 ||
		req.WorkType != string(model.WorkSpray) || !req.From.Equal(t0) {
		t.Fatalf("EncodeCommand = %s, %v", body, err)
	}
}
