package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// CallRecorder 记录一次设备调用的 trace（P1 §6.3 功能任务 6）；实现在 service（DeviceTraceLog），只依赖 model 类型。
type CallRecorder interface {
	RecordDeviceCall(ctx context.Context, t model.DeviceCallTrace)
}

// RawPutter 归档指令原文（agent.RawStore / repository.RawStore 满足）。
type RawPutter interface {
	Put(ctx context.Context, p model.RawPut) (model.RawObject, error)
}

// CommandEncoder 是 Adapter 可选实现：给出发往设备平台的指令原文（归档即所发）；未实现时归档设备无关表示。
type CommandEncoder interface {
	EncodeCommand(order model.WorkOrder, idemKey string) ([]byte, error)
}

// CommandArchiver 是 Traced 暴露给下发器（L4）的能力：写 dispatch_attempt(SENT) 前先归档指令原文，
// 取 raw_object.id 作 command_raw_id（NOT NULL）。同一 order + idemKey 重复归档写同一 key（幂等）。
type CommandArchiver interface {
	Archive(ctx context.Context, order model.WorkOrder, idemKey string) (model.RawObject, error)
}

// ArchivedDispatcher 是 Traced 暴露给下发器（L4）的下发入口：指令原文已经 Archive 归档时用它下发，
// 不再重复归档；trace 的 command_raw_id 取传入值。L4 流程：Archive → InsertSentTx(command_raw_id) → DispatchArchived。
type ArchivedDispatcher interface {
	DispatchArchived(ctx context.Context, order model.WorkOrder, idemKey string, commandRawID uint64) (model.DeviceReceipt, error)
}

// TraceOrder 是 QueryStatus / Cancel 的 trace 归属（这两个方法入参不带作业单）；Dispatch 取自 order。
type TraceOrder struct {
	TenantID uint64
	FarmID   uint64
	OrderID  uint64
	RunID    uint64
}

type traceOrderKey struct{}

// WithTraceOrder 把作业单归属放进 ctx，供 Traced 记录 QueryStatus / Cancel。
func WithTraceOrder(ctx context.Context, o TraceOrder) context.Context {
	return context.WithValue(ctx, traceOrderKey{}, o)
}

func traceOrderFrom(ctx context.Context) TraceOrder {
	o, _ := ctx.Value(traceOrderKey{}).(TraceOrder)
	return o
}

// Traced 是 DeviceAdapter 装饰器：每次调用记录 tool_call_id / device_type / order_id / 耗时 / 回执码，
// Dispatch 先经 RawStore 归档指令原文（kind=DEVICE_CMD），归档失败不调用设备；DispatchArchived 用调用方已归档的原文。
type Traced struct {
	deviceType string
	next       DeviceAdapter
	rec        CallRecorder
	raw        RawPutter
	clk        clock.Clock
	log        *slog.Logger
}

var (
	_ DeviceAdapter      = (*Traced)(nil)
	_ CommandArchiver    = (*Traced)(nil)
	_ ArchivedDispatcher = (*Traced)(nil)
)

// ErrTracedConfig 表示 NewTraced 缺依赖。
var ErrTracedConfig = errors.New("adapter: traced needs device type, adapter, recorder, raw store and clock")

// NewTraced 包装 next；日志默认 slog.Default()，可用 WithLogger 替换。
func NewTraced(deviceType string, next DeviceAdapter, rec CallRecorder, raw RawPutter, clk clock.Clock) (*Traced, error) {
	if deviceType == "" || isNil(next) || rec == nil || raw == nil || clk == nil {
		return nil, ErrTracedConfig
	}
	return &Traced{deviceType: deviceType, next: next, rec: rec, raw: raw, clk: clk, log: slog.Default()}, nil
}

// WithLogger 替换告警日志（nil 忽略），返回 t 便于链式装配。
func (t *Traced) WithLogger(l *slog.Logger) *Traced {
	if l != nil {
		t.log = l
	}
	return t
}

// Capabilities 实现 DeviceAdapter（透传）。
func (t *Traced) Capabilities() Capabilities { return t.next.Capabilities() }

// deviceCommand 是未实现 CommandEncoder 时归档的设备无关指令表示。
type deviceCommand struct {
	IdemKey    string           `json:"idem_key"`
	OrderID    uint64           `json:"order_id"`
	RunID      uint64           `json:"run_id"`
	FieldID    uint64           `json:"field_id"`
	DeviceType string           `json:"device_type"`
	WorkType   model.WorkType   `json:"work_type"`
	Payload    json.RawMessage  `json:"payload"`
	TimeWindow model.TimeWindow `json:"time_window"`
}

// CommandRawName 是指令原文的 raw name：cmd-{sha256(idemKey) 前 32 位}.json（idem_key 含冒号，不能直接作 name）。
func CommandRawName(idemKey string) string {
	sum := sha256.Sum256([]byte(idemKey))
	return "cmd-" + hex.EncodeToString(sum[:16]) + ".json"
}

// Archive 实现 CommandArchiver：key 为 raw/{tenant}/{farm}/{作业窗口起始日}/{run_id}/DEVICE_CMD/{CommandRawName}，
// 日期取 order.TimeWindow.From（业务时间，重试 key 不变；缺省取当前时间）。
func (t *Traced) Archive(ctx context.Context, order model.WorkOrder, idemKey string) (model.RawObject, error) {
	if idemKey == "" {
		return model.RawObject{}, fmt.Errorf("adapter: archive order %d: idem key is required", order.OrderID)
	}
	body, err := t.encode(order, idemKey)
	if err != nil {
		return model.RawObject{}, fmt.Errorf("adapter: encode command order %d: %w", order.OrderID, err)
	}
	at := order.TimeWindow.From
	now := t.clk.Now()
	if at.IsZero() {
		at = now
	}
	obj, err := t.raw.Put(ctx, model.RawPut{TenantID: order.TenantID, FarmID: order.FarmID, RunID: order.RunID,
		Kind: model.RawDeviceCmd, Name: CommandRawName(idemKey), At: at, CreatedAt: now, Body: body,
		ContentType: "application/json"})
	if err != nil {
		return model.RawObject{}, fmt.Errorf("adapter: archive command order %d: %w", order.OrderID, err)
	}
	return obj, nil
}

func (t *Traced) encode(order model.WorkOrder, idemKey string) ([]byte, error) {
	if enc, ok := t.next.(CommandEncoder); ok {
		return enc.EncodeCommand(order, idemKey)
	}
	return json.Marshal(deviceCommand{IdemKey: idemKey, OrderID: order.OrderID, RunID: order.RunID, FieldID: order.FieldID,
		DeviceType: t.deviceType, WorkType: order.WorkType, Payload: order.Payload, TimeWindow: order.TimeWindow})
}

// Dispatch 实现 DeviceAdapter：先归档，再调用设备，最后记录 trace（含 command_raw_id）。
// 调用方已用 Archive 归档过（L4 写 dispatch_attempt(SENT) 前）时改用 DispatchArchived，避免重复归档。
func (t *Traced) Dispatch(ctx context.Context, order model.WorkOrder, idemKey string) (model.DeviceReceipt, error) {
	start := t.clk.Now()
	obj, err := t.Archive(ctx, order, idemKey)
	if err != nil {
		t.finish(ctx, t.dispatchTrace(order, idemKey, start), start, model.DeviceReceipt{}, err)
		return model.DeviceReceipt{}, err
	}
	return t.dispatch(ctx, order, idemKey, obj.ID, start)
}

// DispatchArchived 实现 ArchivedDispatcher：指令原文已由 Archive 归档（commandRawID 即 dispatch_attempt.command_raw_id），
// 不再归档，直接调用设备并记录 trace。commandRawID 为 0 → ErrRejected，不调用设备。
func (t *Traced) DispatchArchived(ctx context.Context, order model.WorkOrder, idemKey string,
	commandRawID uint64) (model.DeviceReceipt, error) {
	start := t.clk.Now()
	if commandRawID == 0 {
		err := fmt.Errorf("%w: order %d: command_raw_id is required", ErrRejected, order.OrderID)
		t.finish(ctx, t.dispatchTrace(order, idemKey, start), start, model.DeviceReceipt{}, err)
		return model.DeviceReceipt{}, err
	}
	return t.dispatch(ctx, order, idemKey, commandRawID, start)
}

func (t *Traced) dispatch(ctx context.Context, order model.WorkOrder, idemKey string, commandRawID uint64,
	start time.Time) (model.DeviceReceipt, error) {
	tr := t.dispatchTrace(order, idemKey, start)
	tr.CommandRawID = commandRawID
	r, err := t.next.Dispatch(ctx, order, idemKey)
	t.finish(ctx, tr, start, r, err)
	return r, err
}

func (t *Traced) dispatchTrace(order model.WorkOrder, idemKey string, start time.Time) model.DeviceCallTrace {
	return t.trace(model.DeviceMethodDispatch, TraceOrder{TenantID: order.TenantID, FarmID: order.FarmID,
		OrderID: order.OrderID, RunID: order.RunID}, idemKey, start)
}

// QueryStatus 实现 DeviceAdapter；归属取 WithTraceOrder（缺失时记 warn，trace 照写但无作业单归属）。
func (t *Traced) QueryStatus(ctx context.Context, ref StatusRef) (model.DeviceReceipt, error) {
	start := t.clk.Now()
	tr := t.trace(model.DeviceMethodQueryStatus, t.orderFrom(ctx, model.DeviceMethodQueryStatus, ref.DeviceJobID), ref.IdemKey, start)
	tr.DeviceJobID = ref.DeviceJobID
	r, err := t.next.QueryStatus(ctx, ref)
	t.finish(ctx, tr, start, r, err)
	return r, err
}

// Cancel 实现 DeviceAdapter；归属取 WithTraceOrder（缺失时记 warn）。
func (t *Traced) Cancel(ctx context.Context, deviceJobID string) (model.DeviceReceipt, error) {
	start := t.clk.Now()
	tr := t.trace(model.DeviceMethodCancel, t.orderFrom(ctx, model.DeviceMethodCancel, deviceJobID), "", start)
	tr.DeviceJobID = deviceJobID
	r, err := t.next.Cancel(ctx, deviceJobID)
	t.finish(ctx, tr, start, r, err)
	return r, err
}

// orderFrom 取 WithTraceOrder 的归属；缺失（OrderID 为 0）时记 warn：该 trace 无法按作业单 / 农场检索。
func (t *Traced) orderFrom(ctx context.Context, method, deviceJobID string) TraceOrder {
	o := traceOrderFrom(ctx)
	if o.OrderID == 0 {
		t.log.WarnContext(ctx, "adapter: device call without WithTraceOrder; trace has no order attribution",
			"device_type", t.deviceType, "method", method, "device_job_id", deviceJobID)
	}
	return o
}

func (t *Traced) trace(method string, o TraceOrder, idemKey string, start time.Time) model.DeviceCallTrace {
	return model.DeviceCallTrace{ToolCallID: deviceCallID(t.deviceType, method, o.OrderID, start), Method: method,
		DeviceType: t.deviceType, TenantID: o.TenantID, FarmID: o.FarmID, OrderID: o.OrderID, RunID: o.RunID, IdemKey: idemKey}
}

func (t *Traced) finish(ctx context.Context, tr model.DeviceCallTrace, start time.Time, r model.DeviceReceipt, err error) {
	tr.Latency = t.clk.Now().Sub(start)
	if r.DeviceJobID != "" {
		tr.DeviceJobID = r.DeviceJobID
	}
	tr.Status, tr.Code = r.Status, r.Code
	if err != nil {
		tr.Error = err.Error()
	}
	t.rec.RecordDeviceCall(ctx, tr)
}

// deviceCallID 是设备调用的 tool_call_id：dev-{sha256(类型/方法/作业单/纳秒时间) 前 24 位}。
func deviceCallID(deviceType, method string, orderID uint64, at time.Time) string {
	sum := sha256.Sum256([]byte(deviceType + "/" + method + "/" + strconv.FormatUint(orderID, 10) + "/" +
		strconv.FormatInt(at.UnixNano(), 10)))
	return "dev-" + hex.EncodeToString(sum[:12])
}
