package adapter_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
)

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(10 * time.Millisecond)
	return c.t
}

type memRecorder struct{ traces []model.DeviceCallTrace }

func (m *memRecorder) RecordDeviceCall(_ context.Context, t model.DeviceCallTrace) {
	m.traces = append(m.traces, t)
}

type memPutter struct {
	puts []model.RawPut
	err  error
}

func (m *memPutter) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	if m.err != nil {
		return model.RawObject{}, m.err
	}
	m.puts = append(m.puts, p)
	return model.RawObject{ID: uint64(40 + len(m.puts)), Kind: p.Kind}, nil
}

// countingDevice 记录调用次数；Dispatch 回 ACCEPTED，Cancel 回 err（非空时）。
type countingDevice struct {
	stubDevice
	dispatches int
	err        error
}

func (c *countingDevice) Dispatch(context.Context, model.WorkOrder, string) (model.DeviceReceipt, error) {
	c.dispatches++
	return model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "job-1", Code: "OK"}, nil
}

func (c *countingDevice) Cancel(context.Context, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, c.err
}

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

func tracedOrder() model.WorkOrder {
	return model.WorkOrder{OrderID: 7, RunID: 9, TenantID: 1, FarmID: 2, FieldID: 3, DeviceType: "uav",
		WorkType: model.WorkSpray, Payload: []byte(`{"area_mu":5}`), TimeWindow: model.TimeWindow{From: t0}}
}

func TestTraced_Dispatch(t *testing.T) {
	ctx := context.Background()
	dev := &countingDevice{stubDevice: stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}}}
	rec, raw := &memRecorder{}, &memPutter{}
	tr, err := adapter.NewTraced("uav", dev, rec, raw, &stepClock{t: t0})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Capabilities() != dev.Capabilities() {
		t.Fatal("capabilities must pass through")
	}
	key := "order.dispatch:7:5:1"
	r, err := tr.Dispatch(ctx, tracedOrder(), key)
	if err != nil || r.DeviceJobID != "job-1" || dev.dispatches != 1 {
		t.Fatalf("Dispatch = %+v, %v (calls %d)", r, err, dev.dispatches)
	}
	if len(raw.puts) != 1 {
		t.Fatalf("puts = %d", len(raw.puts))
	}
	p := raw.puts[0]
	if p.Kind != model.RawDeviceCmd || p.RunID != 9 || p.TenantID != 1 || p.FarmID != 2 || !p.At.Equal(t0) ||
		p.Name != adapter.CommandRawName(key) || !strings.Contains(string(p.Body), `"idem_key":"order.dispatch:7:5:1"`) {
		t.Fatalf("put = %+v body=%s", p, p.Body)
	}
	if len(rec.traces) != 1 {
		t.Fatalf("traces = %d", len(rec.traces))
	}
	got := rec.traces[0]
	if got.Method != model.DeviceMethodDispatch || got.DeviceType != "uav" || got.OrderID != 7 || got.RunID != 9 ||
		got.IdemKey != key || got.DeviceJobID != "job-1" || got.Status != model.AttemptAccepted || got.Code != "OK" ||
		got.CommandRawID != 41 || got.Latency != 20*time.Millisecond || got.Error != "" || // stepClock：Archive 取一次 Now
		!strings.HasPrefix(got.ToolCallID, "dev-") || len(got.ToolCallID) != 28 {
		t.Fatalf("trace = %+v", got)
	}
	// 同一 order + idemKey 重复归档写同一 name（RawStore 按 uk_key 幂等）。
	if _, err := tr.Archive(ctx, tracedOrder(), key); err != nil || raw.puts[1].Name != p.Name {
		t.Fatalf("re-archive = %v %+v", err, raw.puts)
	}
}

func TestTraced_ArchiveFailureSkipsDevice(t *testing.T) {
	dev := &countingDevice{stubDevice: stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}}}
	rec := &memRecorder{}
	tr, err := adapter.NewTraced("uav", dev, rec, &memPutter{err: errors.New("minio down")}, &stepClock{t: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Dispatch(context.Background(), tracedOrder(), "k"); err == nil || dev.dispatches != 0 {
		t.Fatalf("err = %v, dispatches = %d; archive failure must not call the device", err, dev.dispatches)
	}
	if len(rec.traces) != 1 || !strings.Contains(rec.traces[0].Error, "minio down") || rec.traces[0].CommandRawID != 0 {
		t.Fatalf("traces = %+v", rec.traces)
	}
	if _, err := tr.Archive(context.Background(), tracedOrder(), ""); err == nil {
		t.Fatal("empty idem key must be rejected")
	}
}

func TestTraced_QueryCancelAttribution(t *testing.T) {
	dev := &countingDevice{stubDevice: stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}},
		err: adapter.ErrUnavailable}
	rec := &memRecorder{}
	tr, err := adapter.NewTraced("uav", dev, rec, &memPutter{}, &stepClock{t: t0})
	if err != nil {
		t.Fatal(err)
	}
	ctx := adapter.WithTraceOrder(context.Background(), adapter.TraceOrder{TenantID: 1, FarmID: 2, OrderID: 7, RunID: 9})
	if _, err := tr.QueryStatus(ctx, adapter.StatusRef{IdemKey: "k", DeviceJobID: "job-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Cancel(ctx, "job-1"); !errors.Is(err, adapter.ErrUnavailable) {
		t.Fatalf("Cancel err = %v", err)
	}
	if len(rec.traces) != 2 {
		t.Fatalf("traces = %+v", rec.traces)
	}
	q, c := rec.traces[0], rec.traces[1]
	if q.Method != model.DeviceMethodQueryStatus || q.OrderID != 7 || q.FarmID != 2 || q.IdemKey != "k" || q.DeviceJobID != "job-1" {
		t.Fatalf("query trace = %+v", q)
	}
	if c.Method != model.DeviceMethodCancel || c.OrderID != 7 || c.DeviceJobID != "job-1" || c.Error == "" || c.Status != "" {
		t.Fatalf("cancel trace = %+v", c)
	}
	if q.ToolCallID == c.ToolCallID {
		t.Fatal("tool_call_id must differ per call")
	}
}

// TestTraced_DispatchArchived：调用方已归档时不再归档，trace 的 command_raw_id 取传入值；
// command_raw_id 为 0 → ErrRejected 且不调用设备。Archive 的 created_at 取 clock.Now()，目录日期仍取作业窗口起始日。
func TestTraced_DispatchArchived(t *testing.T) {
	ctx := context.Background()
	dev := &countingDevice{stubDevice: stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}}}
	rec, raw := &memRecorder{}, &memPutter{}
	tr, err := adapter.NewTraced("uav", dev, rec, raw, &stepClock{t: t0.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	key := "order.dispatch:7:5:1"
	obj, err := tr.Archive(ctx, tracedOrder(), key)
	if err != nil || len(raw.puts) != 1 {
		t.Fatalf("Archive = %+v, %v", obj, err)
	}
	if p := raw.puts[0]; !p.At.Equal(t0) || !p.CreatedAt.Equal(t0.Add(48*time.Hour+10*time.Millisecond)) {
		t.Fatalf("put At = %s CreatedAt = %s; want At = window start, CreatedAt = clock now", p.At, p.CreatedAt)
	}
	r, err := tr.DispatchArchived(ctx, tracedOrder(), key, obj.ID)
	if err != nil || r.DeviceJobID != "job-1" || dev.dispatches != 1 || len(raw.puts) != 1 {
		t.Fatalf("DispatchArchived = %+v, %v (dispatches %d, puts %d)", r, err, dev.dispatches, len(raw.puts))
	}
	if len(rec.traces) != 1 || rec.traces[0].CommandRawID != obj.ID || rec.traces[0].Method != model.DeviceMethodDispatch {
		t.Fatalf("traces = %+v", rec.traces)
	}
	if _, err := tr.DispatchArchived(ctx, tracedOrder(), key, 0); !errors.Is(err, adapter.ErrRejected) || dev.dispatches != 1 {
		t.Fatalf("zero command_raw_id err = %v, dispatches = %d", err, dev.dispatches)
	}
	if len(rec.traces) != 2 || rec.traces[1].Error == "" {
		t.Fatalf("rejected dispatch must still be traced: %+v", rec.traces)
	}
}

// TestTraced_MissingTraceOrderWarns：QueryStatus / Cancel 缺 WithTraceOrder 时记一条 warn（trace 照写）。
func TestTraced_MissingTraceOrderWarns(t *testing.T) {
	dev := &countingDevice{stubDevice: stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}}}
	rec := &memRecorder{}
	tr, err := adapter.NewTraced("uav", dev, rec, &memPutter{}, &stepClock{t: t0})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tr.WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()
	if _, err := tr.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "job-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Cancel(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "without WithTraceOrder"); n != 2 || len(rec.traces) != 2 {
		t.Fatalf("warns = %d, traces = %d; log:\n%s", n, len(rec.traces), buf.String())
	}
	buf.Reset()
	with := adapter.WithTraceOrder(ctx, adapter.TraceOrder{TenantID: 1, FarmID: 2, OrderID: 7})
	if _, err := tr.QueryStatus(with, adapter.StatusRef{DeviceJobID: "job-1"}); err != nil || buf.Len() != 0 {
		t.Fatalf("with trace order: err = %v, log = %q", err, buf.String())
	}
}

func TestNewTraced_Config(t *testing.T) {
	var typedNil *ptrDevice
	if _, err := adapter.NewTraced("uav", typedNil, &memRecorder{}, &memPutter{}, &stepClock{t: t0}); !errors.Is(err, adapter.ErrTracedConfig) {
		t.Fatalf("typed nil adapter err = %v", err)
	}
	dev := stubDevice{}
	clk := &stepClock{t: t0}
	for name, f := range map[string]func() (*adapter.Traced, error){
		"type": func() (*adapter.Traced, error) { return adapter.NewTraced("", dev, &memRecorder{}, &memPutter{}, clk) },
		"adapter": func() (*adapter.Traced, error) {
			return adapter.NewTraced("uav", nil, &memRecorder{}, &memPutter{}, clk)
		},
		"recorder": func() (*adapter.Traced, error) { return adapter.NewTraced("uav", dev, nil, &memPutter{}, clk) },
		"raw":      func() (*adapter.Traced, error) { return adapter.NewTraced("uav", dev, &memRecorder{}, nil, clk) },
		"clock": func() (*adapter.Traced, error) {
			return adapter.NewTraced("uav", dev, &memRecorder{}, &memPutter{}, nil)
		},
	} {
		if _, err := f(); !errors.Is(err, adapter.ErrTracedConfig) {
			t.Fatalf("missing %s: err = %v", name, err)
		}
	}
	if n := adapter.CommandRawName("order.dispatch:1:2:1"); !strings.HasPrefix(n, "cmd-") || len(n) != 4+32+5 {
		t.Fatalf("CommandRawName = %q", n)
	}
}
