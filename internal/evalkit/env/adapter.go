package env

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
)

// 设备调用方法名（DeviceCall.Method）。
const (
	MethodDryRun   = "DryRun"
	MethodDispatch = "Dispatch"
)

// DeviceCall 是 RecordingAdapter 记录的一次设备调用。
type DeviceCall struct {
	Method         string
	OrderID        uint64
	IdempotencyKey string // 仅 Dispatch
}

// String 返回 "Method order=N"（Dispatch 追加 key），用于打分证据。
func (c DeviceCall) String() string {
	if c.Method == MethodDispatch {
		return fmt.Sprintf("%s order=%d key=%s", c.Method, c.OrderID, c.IdempotencyKey)
	}
	return fmt.Sprintf("%s order=%d", c.Method, c.OrderID)
}

// RecordingAdapter 是评测用设备 Adapter（P0 §6.6）：只记录调用、不执行，供 stop_at_approval 断言
// 「审批前 Adapter 零调用」。每个 trial 一个实例；并发安全。
type RecordingAdapter struct {
	mu    sync.Mutex
	calls []DeviceCall
}

var _ adapter.DeviceAdapter = (*RecordingAdapter)(nil)

// DryRun 记录调用，返回空指令集。
func (r *RecordingAdapter) DryRun(_ context.Context, order model.WorkOrder) (adapter.DryRunResult, error) {
	r.record(DeviceCall{Method: MethodDryRun, OrderID: order.OrderID})
	return adapter.DryRunResult{Commands: json.RawMessage(`[]`)}, nil
}

// Dispatch 记录调用，返回确定性的假任务号（不下发）。
func (r *RecordingAdapter) Dispatch(_ context.Context, order model.WorkOrder, idempotencyKey string) (string, error) {
	r.record(DeviceCall{Method: MethodDispatch, OrderID: order.OrderID, IdempotencyKey: idempotencyKey})
	return fmt.Sprintf("recorded-%d", order.OrderID), nil
}

func (r *RecordingAdapter) record(c DeviceCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

// DeviceCalls 返回按调用顺序的记录副本（无调用时为空切片，非 nil）。
func (r *RecordingAdapter) DeviceCalls() []DeviceCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]DeviceCall{}, r.calls...)
}
