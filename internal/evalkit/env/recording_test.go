package env

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/model"
)

func TestRecordingAdapter(t *testing.T) {
	r := &RecordingAdapter{}
	if got := r.DeviceCalls(); got == nil || len(got) != 0 {
		t.Fatalf("fresh adapter calls = %#v, want empty non-nil", got)
	}
	ctx := context.Background()
	res, err := r.DryRun(ctx, model.WorkOrder{OrderID: 3})
	if err != nil || string(res.Commands) != "[]" {
		t.Fatalf("DryRun = %+v, %v", res, err)
	}
	task, err := r.Dispatch(ctx, model.WorkOrder{OrderID: 3}, "k-3")
	if err != nil || task != "recorded-3" {
		t.Fatalf("Dispatch = %q, %v", task, err)
	}
	got := r.DeviceCalls()
	want := []DeviceCall{{Method: MethodDryRun, OrderID: 3}, {Method: MethodDispatch, OrderID: 3, IdempotencyKey: "k-3"}}
	if !slices.Equal(got, want) {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
	if s := got[0].String() + "|" + got[1].String(); s != "DryRun order=3|Dispatch order=3 key=k-3" {
		t.Fatalf("String = %q", s)
	}
	got[0].OrderID = 99
	if r.DeviceCalls()[0].OrderID != 3 {
		t.Fatal("DeviceCalls must return a copy")
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
