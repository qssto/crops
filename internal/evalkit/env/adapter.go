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
// P1-L4-B：fixtures.device.scripts 按方法、按作业单（首次出现次序）排队，优先于全局序列，可返回错误（TIMEOUT /
// UNAVAILABLE / REJECTED）；不声明时行为不变。每个 trial 一个实例；并发安全。零值可用（无回执序列）。
type RecordingAdapter struct {
	mu       sync.Mutex
	calls    []DeviceCall
	receipts []model.DeviceReceipt
	last     map[string]model.DeviceReceipt // device_job_id → 最近一次回执
	orders   map[string]recordedJob         // device_job_id / idem_key → 作业单与任务号（QueryStatus / Cancel 的调用归属）
	jobs     map[uint64]int                 // order_id → 已分配的任务号个数（每个新 idem_key 一个）
	scripts  []*deviceScript                // P1-L4-B：按方法 / 作业单的脚本
	ordinals map[uint64]int                 // order_id → 首次出现在 DryRun / Dispatch 的次序（1 起）
}

// recordedJob 是一次已登记下发的作业单与任务号。
type recordedJob struct {
	order model.WorkOrder
	job   string
}

// deviceScript 是一条脚本的剩余步骤。
type deviceScript struct {
	method string
	order  int
	steps  []sample.ScriptStep
}

// scriptErrs 是脚本错误名到 adapter 错误的映射。
var scriptErrs = map[string]error{sample.DeviceErrTimeout: adapter.ErrTimeout, sample.DeviceErrUnavailable: adapter.ErrUnavailable,
	sample.DeviceErrRejected: adapter.ErrRejected}

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
	for i, sc := range fx.Scripts {
		if err := checkScript(sc); err != nil {
			return nil, fmt.Errorf("env: fixtures.device.scripts[%d]: %w", i, err)
		}
		r.scripts = append(r.scripts, &deviceScript{method: sc.Method, order: sc.Order, steps: append([]sample.ScriptStep(nil), sc.Steps...)})
	}
	return r, nil
}

// checkScript 校验脚本的错误名与回执自洽（样本校验同口径，构造时兜底）。
func checkScript(sc sample.DeviceScript) error {
	for j, st := range sc.Steps {
		if st.Error != "" {
			if scriptErrs[st.Error] == nil {
				return fmt.Errorf("steps[%d]: unknown error %q", j, st.Error)
			}
			continue
		}
		probe := st.Receipt()
		probe.DeviceJobID = "recorded"
		if err := probe.Validate(); err != nil {
			return fmt.Errorf("steps[%d]: %w", j, err)
		}
	}
	return nil
}

// Capabilities 实现 adapter.DeviceAdapter：全部声明（评测桩）。
func (r *RecordingAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Cancel: true, IdemKey: true, ExternalQuery: true}
}

// DryRun 记录调用，返回空指令集。
func (r *RecordingAdapter) DryRun(_ context.Context, order model.WorkOrder) (adapter.DryRunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, callOf(MethodDryRun, order, ""))
	r.ordinalLocked(order.OrderID)
	return adapter.DryRunResult{Commands: json.RawMessage(`[]`)}, nil
}

// Dispatch 记录调用，返回脚本 / 序列中的下一条回执（用完回 ACCEPTED）；不下发。任务号按 idem_key 分配：
// 作业单第一个受理的 idem_key 为 recorded-{order_id}，第 n 个（NOT_EXECUTED 重试的新尝试）为 recorded-{order_id}-{n}，
// 同一 idem_key 重投复用原任务号（dispatch_attempt.uk_job 不冲突）。
// 脚本错误步返回对应错误；Accepted 为 true 时仍登记任务（之后按 idem_key 可查到），否则视为平台未受理。
func (r *RecordingAdapter) Dispatch(_ context.Context, order model.WorkOrder, idempotencyKey string) (model.DeviceReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, callOf(MethodDispatch, order, idempotencyKey))
	r.ordinalLocked(order.OrderID)
	job := r.jobLocked(order.OrderID, idempotencyKey)
	st, scripted := r.scriptLocked(MethodDispatch, order.OrderID)
	if scripted && st.Error != "" {
		if st.Accepted {
			r.remember(job, idempotencyKey, order)
		}
		return model.DeviceReceipt{}, scriptErr(MethodDispatch, order.OrderID, st.Error)
	}
	r.remember(job, idempotencyKey, order)
	if scripted {
		return r.keepLocked(job, st.Receipt()), nil
	}
	return r.nextLocked(job, model.DeviceReceipt{Status: model.AttemptAccepted}), nil
}

// QueryStatus 记录调用，返回脚本 / 序列中的下一条回执（用完重复该任务最近一条）；未知任务返回 ErrJobNotFound。
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
	if rc, ok, err := r.scriptedLocked(MethodQueryStatus, order.OrderID, job); ok {
		return rc, err
	}
	last, ok := r.last[job]
	if !ok {
		last = model.DeviceReceipt{Status: model.AttemptAccepted}
	}
	return r.nextLocked(job, last), nil
}

// Cancel 记录调用，返回脚本 / 序列中的下一条回执（用完回 CANCELED）；未知任务返回 ErrJobNotFound。
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
	if rc, ok, err := r.scriptedLocked(MethodCancel, order.OrderID, job); ok {
		return rc, err
	}
	return r.nextLocked(job, model.DeviceReceipt{Status: model.AttemptCanceled}), nil
}

// ordinalLocked 登记作业单首次出现的次序（脚本 order 字段按它匹配）。
func (r *RecordingAdapter) ordinalLocked(orderID uint64) {
	if r.ordinals == nil {
		r.ordinals = map[uint64]int{}
	}
	if _, ok := r.ordinals[orderID]; !ok {
		r.ordinals[orderID] = len(r.ordinals) + 1
	}
}

// scriptLocked 取 (method, 作业单) 的下一个脚本步：先匹配 order 精确的脚本，再匹配 order=0 的脚本。
func (r *RecordingAdapter) scriptLocked(method string, orderID uint64) (sample.ScriptStep, bool) {
	ord := r.ordinals[orderID]
	for _, exact := range []bool{true, false} {
		for _, sc := range r.scripts {
			if sc.method != method || len(sc.steps) == 0 || exact != (sc.order != 0) || exact && sc.order != ord {
				continue
			}
			st := sc.steps[0]
			sc.steps = sc.steps[1:]
			return st, true
		}
	}
	return sample.ScriptStep{}, false
}

// scriptedLocked 执行 QueryStatus / Cancel 的脚本步；ok 为 false 表示无脚本可用（走全局序列）。
func (r *RecordingAdapter) scriptedLocked(method string, orderID uint64, job string) (model.DeviceReceipt, bool, error) {
	st, ok := r.scriptLocked(method, orderID)
	switch {
	case !ok:
		return model.DeviceReceipt{}, false, nil
	case st.Error != "":
		return model.DeviceReceipt{}, true, scriptErr(method, orderID, st.Error)
	}
	return r.keepLocked(job, st.Receipt()), true, nil
}

// scriptErr 把脚本错误名转为 adapter 错误（errors.Is 可判定）。
func scriptErr(method string, orderID uint64, name string) error {
	return fmt.Errorf("%w: scripted %s order=%d", scriptErrs[name], method, orderID)
}

// jobLocked 返回 idem_key 的任务号：已登记过则复用，否则为作业单下一个待登记的任务号（登记时才计数，
// 平台未受理的下发不占号）。
func (r *RecordingAdapter) jobLocked(orderID uint64, idemKey string) string {
	if j, ok := r.orders[idemKey]; ok && idemKey != "" {
		return j.job
	}
	if n := r.jobs[orderID] + 1; n > 1 {
		return fmt.Sprintf("recorded-%d-%d", orderID, n)
	}
	return fmt.Sprintf("recorded-%d", orderID)
}

// remember 登记任务号与 idem_key 的归属；新任务号使作业单的计数 +1。
func (r *RecordingAdapter) remember(job, idemKey string, order model.WorkOrder) {
	if r.orders == nil {
		r.orders = map[string]recordedJob{}
		r.jobs = map[uint64]int{}
	}
	if _, seen := r.orders[job]; !seen {
		r.jobs[order.OrderID]++
	}
	rec := recordedJob{order: order, job: job}
	r.orders[job] = rec
	if idemKey != "" {
		r.orders[idemKey] = rec
	}
}

// lookupLocked 按任务号或外部单号找已下发的作业单与任务号；找不到时任务号为空。
func (r *RecordingAdapter) lookupLocked(ref adapter.StatusRef) (model.WorkOrder, string) {
	key := ref.DeviceJobID
	if key == "" {
		key = ref.IdemKey
	}
	rec, ok := r.orders[key]
	if !ok || key == "" {
		return model.WorkOrder{}, ""
	}
	return rec.order, rec.job
}

// nextLocked 取序列下一条（空则用 fallback），补任务号并记为该任务最近回执。
func (r *RecordingAdapter) nextLocked(job string, fallback model.DeviceReceipt) model.DeviceReceipt {
	rc := fallback
	if len(r.receipts) > 0 {
		rc, r.receipts = r.receipts[0], r.receipts[1:]
	}
	return r.keepLocked(job, rc)
}

// keepLocked 补任务号并记为该任务最近回执（QueryStatus 用完序列后重复它）。
func (r *RecordingAdapter) keepLocked(job string, rc model.DeviceReceipt) model.DeviceReceipt {
	if rc.DeviceJobID == "" {
		rc.DeviceJobID = job
	}
	if r.last == nil {
		r.last = map[string]model.DeviceReceipt{}
	}
	r.last[job] = rc
	return rc
}

// DeviceCalls 返回按调用顺序的记录副本（无调用时为空切片，非 nil）。
func (r *RecordingAdapter) DeviceCalls() []DeviceCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]DeviceCall{}, r.calls...)
}
