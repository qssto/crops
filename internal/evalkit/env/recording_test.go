package env

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/adapter"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func TestRecordingAdapter(t *testing.T) {
	r := &RecordingAdapter{}
	if got := r.DeviceCalls(); got == nil || len(got) != 0 {
		t.Fatalf("fresh adapter calls = %#v, want empty non-nil", got)
	}
	ctx := context.Background()
	order := model.WorkOrder{OrderID: 3, TenantID: 1, FarmID: 2, FieldID: 201, Payload: []byte(`{"device_id":"uav-p100-21"}`)}
	res, err := r.DryRun(ctx, order)
	if err != nil || string(res.Commands) != "[]" {
		t.Fatalf("DryRun = %+v, %v", res, err)
	}
	rc, err := r.Dispatch(ctx, order, "k-3")
	if err != nil || rc.DeviceJobID != "recorded-3" || rc.Status != model.AttemptAccepted {
		t.Fatalf("Dispatch = %+v, %v", rc, err)
	}
	order.Payload[0] = 'X' // 记录须复制 payload
	got := r.DeviceCalls()
	want := []DeviceCall{{Method: MethodDryRun, OrderID: 3}, {Method: MethodDispatch, OrderID: 3, IdempotencyKey: "k-3"}}
	eq := func(a, b DeviceCall) bool {
		return a.Method == b.Method && a.OrderID == b.OrderID && a.IdempotencyKey == b.IdempotencyKey &&
			a.TenantID == 1 && a.FarmID == 2 && a.FieldID == 201 && string(a.Payload) == `{"device_id":"uav-p100-21"}`
	}
	if !slices.EqualFunc(got, want, eq) {
		t.Fatalf("calls = %+v, want %+v with order attribution", got, want)
	}
	if s := got[0].String() + "|" + got[1].String(); s != "DryRun order=3|Dispatch order=3 key=k-3" {
		t.Fatalf("String = %q", s)
	}
	got[0].OrderID = 99
	if r.DeviceCalls()[0].OrderID != 3 {
		t.Fatal("DeviceCalls must return a copy")
	}
}

// TestRecordingAdapter_ReceiptSequence：样本回执序列按调用次序返回；用完后各方法回默认；QueryStatus / Cancel 归属取下发的作业单。
func TestRecordingAdapter_ReceiptSequence(t *testing.T) {
	area := 3.5
	r, err := NewRecordingAdapter(&sample.DeviceFixture{Receipts: []sample.ReceiptFixture{
		{Status: "ACCEPTED", Code: "OK"}, {Status: "EXECUTING"}, {Status: "PARTIALLY_DONE", DoneAreaMu: &area}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	order := model.WorkOrder{OrderID: 5, TenantID: 1, FarmID: 2, FieldID: 201}
	want := []model.AttemptStatus{model.AttemptAccepted, model.AttemptExecuting, model.AttemptPartiallyDone,
		model.AttemptPartiallyDone, model.AttemptCanceled}
	var got []model.AttemptStatus
	steps := []func() (model.DeviceReceipt, error){
		func() (model.DeviceReceipt, error) { return r.Dispatch(ctx, order, "k-5") },
		func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "recorded-5"})
		},
		func() (model.DeviceReceipt, error) { return r.QueryStatus(ctx, adapter.StatusRef{IdemKey: "k-5"}) },
		func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "recorded-5"})
		},
		func() (model.DeviceReceipt, error) { return r.Cancel(ctx, "recorded-5") },
	}
	for i, step := range steps {
		rc, err := step()
		if err != nil || rc.DeviceJobID != "recorded-5" {
			t.Fatalf("step %d = %+v, %v", i, rc, err)
		}
		got = append(got, rc.Status)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	calls := r.DeviceCalls()
	if len(calls) != 5 || calls[4].String() != "Cancel order=5 job=recorded-5" || calls[2].FarmID != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if _, err := r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "nope"}); !errors.Is(err, adapter.ErrJobNotFound) {
		t.Fatalf("unknown job err = %v", err)
	}
	if _, err := NewRecordingAdapter(&sample.DeviceFixture{Receipts: []sample.ReceiptFixture{{Status: "FAILED"}}}); err == nil {
		t.Fatal("FAILED without failure_kind must be rejected")
	}
}

func TestLogCapture(t *testing.T) {
	c := NewLogCapture()
	if got := c.Logs(); got == nil || len(got) != 0 {
		t.Fatalf("fresh capture = %#v, want empty non-nil", got)
	}
	var out bytes.Buffer
	h := c.Tap(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	l := slog.New(h)
	l.Debug("below level")
	l.Info("plain", "k", 1)
	l.With("run_id", 7).WithGroup("g").Info("derived", "x", "y")

	logs := string(c.Logs())
	if logs != out.String() {
		t.Fatalf("capture differs from downstream output:\ncapture=%s\nout=%s", logs, out.String())
	}
	if strings.Contains(logs, "below level") {
		t.Fatalf("capture must follow downstream level: %s", logs)
	}
	for _, want := range []string{`"msg":"plain","k":1`, `"run_id":7,"g":{"x":"y"}`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("capture missing %s: %s", want, logs)
		}
	}
	lr, ok := h.(interface{ Logs() []byte })
	if !ok || string(lr.Logs()) != logs {
		t.Fatal("tapped handler must expose Logs for the runner")
	}
}
