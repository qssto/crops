package env

import (
	"context"
	"errors"
	"testing"

	"agri-steward/internal/adapter"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// TestRecordingAdapter_JobPerIdemKey：同一作业单的新 idem_key（NOT_EXECUTED 重试）分配新任务号，
// 重投同一 idem_key 复用原任务号；按新旧任务号都能查到并归属同一作业单。
func TestRecordingAdapter_JobPerIdemKey(t *testing.T) {
	ctx := context.Background()
	r := &RecordingAdapter{}
	order := model.WorkOrder{OrderID: 7}
	steps := []struct {
		idem, wantJob string
	}{
		{"order.dispatch:7:1:1", "recorded-7"},
		{"order.dispatch:7:1:1", "recorded-7"},
		{"order.dispatch:7:1:2", "recorded-7-2"},
	}
	for i, s := range steps {
		rc, err := r.Dispatch(ctx, order, s.idem)
		if err != nil || rc.DeviceJobID != s.wantJob {
			t.Fatalf("dispatch #%d job = %q, %v; want %q", i, rc.DeviceJobID, err, s.wantJob)
		}
	}
	for _, job := range []string{"recorded-7", "recorded-7-2"} {
		if rc, err := r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: job}); err != nil || rc.DeviceJobID != job {
			t.Errorf("QueryStatus(%s) = %+v, %v", job, rc, err)
		}
	}
	if calls := r.DeviceCalls(); len(calls) != 5 || calls[4].OrderID != 7 {
		t.Errorf("calls = %v", calls)
	}
}

// TestRecordingAdapter_Scripts（P1-L4-B）：按方法、按作业单（首次出现次序）的脚本优先于全局序列；
// 错误步返回 adapter 错误；Dispatch 错误步 accepted=true 时仍可按 idem_key 查到；脚本用完回落到全局序列 / 默认值。
func TestRecordingAdapter_Scripts(t *testing.T) {
	area := 4.0
	rcpt := func(s string) sample.ScriptStep {
		return sample.ScriptStep{ReceiptFixture: sample.ReceiptFixture{Status: s}}
	}
	r, err := NewRecordingAdapter(&sample.DeviceFixture{
		Receipts: []sample.ReceiptFixture{{Status: "EXECUTING"}},
		Scripts: []sample.DeviceScript{
			{Method: "Dispatch", Order: 1, Steps: []sample.ScriptStep{{Error: sample.DeviceErrTimeout, Accepted: true}}},
			{Method: "Dispatch", Order: 2, Steps: []sample.ScriptStep{{Error: sample.DeviceErrUnavailable}, rcpt("ACCEPTED")}},
			{Method: "QueryStatus", Order: 1, Steps: []sample.ScriptStep{rcpt("ACCEPTED"), {Error: sample.DeviceErrRejected}}},
			{Method: "Cancel", Steps: []sample.ScriptStep{{ReceiptFixture: sample.ReceiptFixture{Status: "CANCELED", DoneAreaMu: &area}}}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o1 := model.WorkOrder{OrderID: 11, TenantID: 1, FarmID: 2}
	o2 := model.WorkOrder{OrderID: 22, TenantID: 1, FarmID: 2}
	if _, err := r.DryRun(ctx, o1); err != nil { // o1 首次出现 → order 1
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		call       func() (model.DeviceReceipt, error)
		wantErr    error
		wantStatus model.AttemptStatus
	}{
		{name: "o1 dispatch timeout (accepted)", call: func() (model.DeviceReceipt, error) { return r.Dispatch(ctx, o1, "k-11") },
			wantErr: adapter.ErrTimeout},
		{name: "o1 query by idem finds accepted job", call: func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{IdemKey: "k-11"})
		}, wantStatus: model.AttemptAccepted},
		{name: "o2 dispatch unavailable", call: func() (model.DeviceReceipt, error) { return r.Dispatch(ctx, o2, "k-22a") },
			wantErr: adapter.ErrUnavailable},
		{name: "o2 not registered after unaccepted error", call: func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{IdemKey: "k-22a"})
		}, wantErr: adapter.ErrJobNotFound},
		{name: "o2 dispatch retry accepted", call: func() (model.DeviceReceipt, error) { return r.Dispatch(ctx, o2, "k-22b") },
			wantStatus: model.AttemptAccepted},
		{name: "o2 query falls back to global sequence", call: func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "recorded-22"})
		}, wantStatus: model.AttemptExecuting},
		{name: "o1 query scripted rejected", call: func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "recorded-11"})
		}, wantErr: adapter.ErrRejected},
		{name: "o1 query exhausted repeats last", call: func() (model.DeviceReceipt, error) {
			return r.QueryStatus(ctx, adapter.StatusRef{DeviceJobID: "recorded-11"})
		}, wantStatus: model.AttemptAccepted},
		{name: "any-order cancel script", call: func() (model.DeviceReceipt, error) { return r.Cancel(ctx, "recorded-22") },
			wantStatus: model.AttemptCanceled},
		{name: "cancel exhausted default", call: func() (model.DeviceReceipt, error) { return r.Cancel(ctx, "recorded-11") },
			wantStatus: model.AttemptCanceled},
	}
	for _, tt := range tests {
		rc, err := tt.call()
		if !errors.Is(err, tt.wantErr) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.wantErr)
		}
		if tt.wantErr == nil && (rc.Status != tt.wantStatus || rc.DeviceJobID == "") {
			t.Fatalf("%s: receipt = %+v, want %s", tt.name, rc, tt.wantStatus)
		}
		if tt.name == "any-order cancel script" && (rc.DoneAreaMu == nil || *rc.DoneAreaMu != 4) {
			t.Fatalf("cancel area = %v", rc.DoneAreaMu)
		}
	}
	if n := len(r.DeviceCalls()); n != 11 {
		t.Fatalf("calls = %d, want 11 (errors are recorded too)", n)
	}
}

func TestNewRecordingAdapter_BadScript(t *testing.T) {
	for name, sc := range map[string]sample.DeviceScript{
		"unknown error": {Method: "Cancel", Steps: []sample.ScriptStep{{Error: "BOOM"}}},
		"bad receipt":   {Method: "QueryStatus", Steps: []sample.ScriptStep{{ReceiptFixture: sample.ReceiptFixture{Status: "FAILED"}}}},
	} {
		if _, err := NewRecordingAdapter(&sample.DeviceFixture{Scripts: []sample.DeviceScript{sc}}); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}
