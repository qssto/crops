package env

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"agri-steward/internal/adapter"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// 设备调用方法名（DeviceCall.Method）。
const (
	MethodDryRun      = "DryRun"
	MethodDispatch    = model.DeviceMethodDispatch
	MethodQueryStatus = model.DeviceMethodQueryStatus
	MethodCancel      = model.DeviceMethodCancel
)

// DeviceCall 是 RecordingAdapter 记录的一次设备调用；TenantID / FarmID / FieldID / Payload 取自作业单，
// 供 cross_farm_isolation 按结构判定设备调用目标的归属。
type DeviceCall struct {
	Method         string
	OrderID        uint64
	IdempotencyKey string // Dispatch；QueryStatus 按外部单号查时也有
	DeviceJobID    string // 仅 QueryStatus / Cancel
	TenantID       uint64
	FarmID         uint64
	FieldID        uint64
	Payload        json.RawMessage
}

// callOf 由作业单构造调用记录（Payload 复制，避免调用方后续改写）。
func callOf(method string, order model.WorkOrder, key string) DeviceCall {
	return DeviceCall{Method: method, OrderID: order.OrderID, IdempotencyKey: key, TenantID: order.TenantID,
		FarmID: order.FarmID, FieldID: order.FieldID, Payload: append(json.RawMessage(nil), order.Payload...)}
}

// String 返回 "Method order=N"（Dispatch 追加 key，QueryStatus / Cancel 追加 job），用于打分证据。
func (c DeviceCall) String() string {
	switch c.Method {
	case MethodDispatch:
		return fmt.Sprintf("%s order=%d key=%s", c.Method, c.OrderID, c.IdempotencyKey)
	case MethodQueryStatus, MethodCancel:
		return fmt.Sprintf("%s order=%d job=%s", c.Method, c.OrderID, c.DeviceJobID)
	}
	return fmt.Sprintf("%s order=%d", c.Method, c.OrderID)
}

// RecordingAdapter 是评测用设备 Adapter（P0 §6.6）：只记录调用、不执行，供 stop_at_approval / no_model_actuate 断言
// 「审批前 Adapter 零调用」。Dispatch / QueryStatus / Cancel 按样本 fixtures.device.receipts 逐个回执（P1 L2）。
// 每个 trial 一个实例；并发安全。零值可用（无回执序列）。
type RecordingAdapter struct {
	mu       sync.Mutex
	calls    []DeviceCall
	receipts []model.DeviceReceipt
	last     map[string]model.DeviceReceipt // device_job_id → 最近一次回执
	orders   map[string]model.WorkOrder     // device_job_id / idem_key → 作业单（QueryStatus / Cancel 的调用归属）
}

var (
	_ adapter.DryRunner     = (*RecordingAdapter)(nil)
	_ adapter.DeviceAdapter = (*RecordingAdapter)(nil)
)

// NewRecordingAdapter 按样本回执序列构造；fx 为空等同零值。回执不自洽（补 device_job_id 后）返回错误。
func NewRecordingAdapter(fx *sample.DeviceFixture) (*RecordingAdapter, error) {
	r := &RecordingAdapter{}
	if fx == nil {
		return r, nil
	}
	for i, f := range fx.Receipts {
		rc := f.Receipt()
		probe := rc
		probe.DeviceJobID = "recorded"
		if err := probe.Validate(); err != nil {
			return nil, fmt.Errorf("env: fixtures.device.receipts[%d]: %w", i, err)
		}
		r.receipts = append(r.receipts, rc)
	}
	return r, nil
}

// Capabilities 实现 adapter.DeviceAdapter：全部声明（评测桩）。
func (r *RecordingAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Cancel: true, IdemKey: true, ExternalQuery: true}
}

// DryRun 记录调用，返回空指令集。
func (r *RecordingAdapter) DryRun(_ context.Context, order model.WorkOrder) (adapter.DryRunResult, error) {
	r.record(callOf(MethodDryRun, order, ""))
	return adapter.DryRunResult{Commands: json.RawMessage(`[]`)}, nil
}

// Dispatch 记录调用，返回序列中的下一条回执（用完回 ACCEPTED）；任务号确定为 recorded-{order_id}（不下发）。
func (r *RecordingAdapter) Dispatch(_ context.Context, order model.WorkOrder, idempotencyKey string) (model.DeviceReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, callOf(MethodDispatch, order, idempotencyKey))
	job := fmt.Sprintf("recorded-%d", order.OrderID)
	r.remember(job, idempotencyKey, order)
	return r.nextLocked(job, model.DeviceReceipt{Status: model.AttemptAccepted}), nil
}

// QueryStatus 记录调用，返回序列中的下一条回执（用完重复该任务最近一条）；未知任务返回 ErrJobNotFound。
func (r *RecordingAdapter) QueryStatus(_ context.Context, ref adapter.StatusRef) (model.DeviceReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	order, job := r.lookupLocked(ref)
	c := callOf(MethodQueryStatus, order, ref.IdemKey)
	c.DeviceJobID = ref.DeviceJobID
	r.calls = append(r.calls, c)
	if job == "" {
		return model.DeviceReceipt{}, fmt.Errorf("%w: %+v", adapter.ErrJobNotFound, ref)
	}
	last, ok := r.last[job]
	if !ok {
		last = model.DeviceReceipt{Status: model.AttemptAccepted}
	}
	return r.nextLocked(job, last), nil
}

// Cancel 记录调用，返回序列中的下一条回执（用完回 CANCELED）；未知任务返回 ErrJobNotFound。
func (r *RecordingAdapter) Cancel(_ context.Context, deviceJobID string) (model.DeviceReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	order, job := r.lookupLocked(adapter.StatusRef{DeviceJobID: deviceJobID})
	c := callOf(MethodCancel, order, "")
	c.DeviceJobID = deviceJobID
	r.calls = append(r.calls, c)
	if job == "" {
		return model.DeviceReceipt{}, fmt.Errorf("%w: %s", adapter.ErrJobNotFound, deviceJobID)
	}
	return r.nextLocked(job, model.DeviceReceipt{Status: model.AttemptCanceled}), nil
}

func (r *RecordingAdapter) remember(job, idemKey string, order model.WorkOrder) {
	if r.orders == nil {
		r.orders = map[string]model.WorkOrder{}
	}
	r.orders[job] = order
	if idemKey != "" {
		r.orders[idemKey] = order
	}
}

// lookupLocked 按任务号或外部单号找已下发的作业单与任务号；找不到时任务号为空。
func (r *RecordingAdapter) lookupLocked(ref adapter.StatusRef) (model.WorkOrder, string) {
	key := ref.DeviceJobID
	if key == "" {
		key = ref.IdemKey
	}
	o, ok := r.orders[key]
	if !ok || key == "" {
		return model.WorkOrder{}, ""
	}
	return o, fmt.Sprintf("recorded-%d", o.OrderID)
}

// nextLocked 取序列下一条（空则用 fallback），补任务号并记为该任务最近回执。
func (r *RecordingAdapter) nextLocked(job string, fallback model.DeviceReceipt) model.DeviceReceipt {
	rc := fallback
	if len(r.receipts) > 0 {
		rc, r.receipts = r.receipts[0], r.receipts[1:]
	}
	if rc.DeviceJobID == "" {
		rc.DeviceJobID = job
	}
	if r.last == nil {
		r.last = map[string]model.DeviceReceipt{}
	}
	r.last[job] = rc
	return rc
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
