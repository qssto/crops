package model_test

import (
	"errors"
	"testing"

	"agri-steward/internal/model"
)

func receiptArea(v float64) *float64 { return &v }

// TestMapReceipt_FullTable：回执映射全表（P1 §6.3 功能任务 9），与 docs/p1/回执映射表.md 逐行一致。
func TestMapReceipt_FullTable(t *testing.T) {
	tests := []struct {
		status model.AttemptStatus
		kind   model.FailureKind
		area   *float64
		effect model.ReceiptEffect
		to     model.WorkOrderStatus
	}{
		{model.AttemptSent, "", nil, model.EffectNone, ""},
		{model.AttemptAccepted, "", nil, model.EffectNone, ""},
		{model.AttemptExecuting, "", nil, model.EffectTransition, model.OrderExecuting},
		{model.AttemptDone, "", nil, model.EffectTransition, model.OrderDone},
		{model.AttemptPartiallyDone, "", receiptArea(7.5), model.EffectTransition, model.OrderPartiallyDone},
		{model.AttemptCanceled, "", nil, model.EffectTransition, model.OrderCanceled},
		{model.AttemptCanceled, "", receiptArea(3), model.EffectTransition, model.OrderPartiallyDone},
		{model.AttemptFailed, model.FailureInExecution, nil, model.EffectTransition, model.OrderFailed},
		{model.AttemptFailed, model.FailureNotExecuted, nil, model.EffectRetryOrFail, model.OrderFailed},
		{model.AttemptFailed, model.FailureUnknown, nil, model.EffectReconcile, ""},
		{model.AttemptNoReceipt, "", nil, model.EffectReconcile, ""},
	}
	if len(tests) != len(model.ReceiptRules) {
		t.Fatalf("table has %d rows, ReceiptRules has %d", len(tests), len(model.ReceiptRules))
	}
	for _, tt := range tests {
		t.Run(string(tt.status)+"/"+string(tt.kind), func(t *testing.T) {
			r, err := model.MapReceipt(tt.status, tt.kind, tt.area)
			if err != nil || r.Effect != tt.effect || r.To != tt.to {
				t.Fatalf("MapReceipt = %+v, %v; want %s → %q", r, err, tt.effect, tt.to)
			}
			// 迁移目标须是状态机里 DISPATCHED 的合法后继（本层不改状态机）。
			if r.To != "" && !model.CanTransition(model.OrderDispatched, r.To) {
				t.Fatalf("DISPATCHED → %s not in WorkOrderTransitions", r.To)
			}
		})
	}
	if _, err := model.MapReceipt(model.AttemptFailed, "", nil); !errors.Is(err, model.ErrBadReceipt) {
		t.Fatalf("FAILED without kind err = %v", err)
	}
	if _, err := model.MapReceipt("BOGUS", "", nil); !errors.Is(err, model.ErrBadReceipt) {
		t.Fatalf("unknown status err = %v", err)
	}
	if r, err := model.MapReceipt(model.AttemptDone, model.FailureUnknown, receiptArea(30)); err != nil || r.To != model.OrderDone {
		t.Fatalf("kind / area must be ignored for DONE: %+v, %v", r, err)
	}
	// 撤销时面积为 0 视同未作业：仍记 CANCELED。
	if r, err := model.MapReceipt(model.AttemptCanceled, "", receiptArea(0)); err != nil || r.To != model.OrderCanceled {
		t.Fatalf("CANCELED with 0 mu = %+v, %v; want CANCELED", r, err)
	}
}

func TestCheckAttemptState(t *testing.T) {
	tests := []struct {
		name   string
		status model.AttemptStatus
		kind   model.FailureKind
		area   *float64
		ok     bool
	}{
		{"executing", model.AttemptExecuting, "", nil, true},
		{"no receipt", model.AttemptNoReceipt, "", nil, true},
		{"failed with kind", model.AttemptFailed, model.FailureUnknown, nil, true},
		{"unknown status", "RUNNING", "", nil, false},
		{"empty status", "", "", nil, false},
		{"failed without kind", model.AttemptFailed, "", nil, false},
		{"failed unknown kind", model.AttemptFailed, "MID_AIR", nil, false},
		{"kind on done", model.AttemptDone, model.FailureInExecution, nil, false},
		{"negative area", model.AttemptPartiallyDone, "", receiptArea(-1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := model.CheckAttemptState(tt.status, tt.kind, tt.area)
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, model.ErrBadReceipt)) {
				t.Fatalf("CheckAttemptState = %v, want ok=%t", err, tt.ok)
			}
		})
	}
}

func TestDeviceReceiptValidate(t *testing.T) {
	tests := []struct {
		name string
		r    model.DeviceReceipt
		ok   bool
	}{
		{"accepted", model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "j1"}, true},
		{"done with area", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "j1", DoneAreaMu: receiptArea(30)}, true},
		{"rejected before job", model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureNotExecuted}, true},
		{"local SENT", model.DeviceReceipt{Status: model.AttemptSent, DeviceJobID: "j1"}, false},
		{"local NO_RECEIPT", model.DeviceReceipt{Status: model.AttemptNoReceipt, DeviceJobID: "j1"}, false},
		{"unknown status", model.DeviceReceipt{Status: "RUNNING", DeviceJobID: "j1"}, false},
		{"failed without kind", model.DeviceReceipt{Status: model.AttemptFailed, DeviceJobID: "j1"}, false},
		{"kind on done", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "j1", FailureKind: model.FailureUnknown}, false},
		{"negative area", model.DeviceReceipt{Status: model.AttemptDone, DeviceJobID: "j1", DoneAreaMu: receiptArea(-1)}, false},
		{"partial without area", model.DeviceReceipt{Status: model.AttemptPartiallyDone, DeviceJobID: "j1"}, false},
		{"accepted without job", model.DeviceReceipt{Status: model.AttemptAccepted}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.r.Validate()
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, model.ErrBadReceipt)) {
				t.Fatalf("Validate = %v, want ok=%t", err, tt.ok)
			}
		})
	}
}

func TestAttemptStatusFinal(t *testing.T) {
	for _, r := range model.ReceiptRules {
		if !r.Status.Valid() {
			t.Fatalf("status %s not valid", r.Status)
		}
		// FAILED 各细分都终结设备侧；其余状态终结 ⇔ 映射到作业单终态。
		want := r.Status == model.AttemptFailed || (r.To != "" && r.To.IsTerminal())
		if r.Status.IsFinal() != want {
			t.Errorf("%s/%s IsFinal = %t inconsistent with mapping %s", r.Status, r.FailureKind, r.Status.IsFinal(), r.To)
		}
	}
	if !model.RawDeviceCmd.Valid() {
		t.Fatal("DEVICE_CMD must be a registered raw kind")
	}
}
