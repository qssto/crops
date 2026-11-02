// Package adapter 是设备 Adapter：只被下发器（service/workorder_dispatch.go）调用（母文档 §4.6.2-2），不注册为模型工具，
// 不 import repository / agent（archtest 守卫）。
//
// 接口按 ISP 拆两个：DryRunner 是 P0 dry-run 下发（MockAdapter）；DeviceAdapter 是 P1 真实下发三方法
// Dispatch / QueryStatus / Cancel（P1 §6.3），按 work_order.device_type 在 Registry 注册，经 Traced 记录 trace 与指令原文。
package adapter

import (
	"context"
	"encoding/json"
	"errors"

	"agri-steward/internal/model"
)

// DryRunResult 是 dry-run 返回的将下发指令（进程内，无外部 IO）。
type DryRunResult struct {
	Commands json.RawMessage
}

// DryRunner 是 P0 dry-run 下发接缝（MockAdapter / 评测 RecordingAdapter 实现）。
type DryRunner interface {
	DryRun(ctx context.Context, order model.WorkOrder) (DryRunResult, error)
}

// Capabilities 是设备平台声明的能力（P1 §6.1 开放规则）：未实现 Cancel、或幂等键与外部单号查询都不支持的，Register 拒绝。
type Capabilities struct {
	Cancel        bool // 支持撤销已受理任务
	IdemKey       bool // 透传幂等键：同键重复提交返回同一任务
	ExternalQuery bool // 按外部单号（= idem_key）查任务
}

// StatusRef 定位一次下发：DeviceJobID 优先；未受理（下发超时）时按 IdemKey 查（需 ExternalQuery）。
type StatusRef struct {
	IdemKey     string
	DeviceJobID string
}

// DeviceAdapter 是真实下发接缝。入参是 work_order 的设备无关表示，各实现自行转协议；返回标准化回执。
type DeviceAdapter interface {
	Capabilities() Capabilities
	// Dispatch 提交作业；idemKey = outbox.dedup_key，同键重复调用不重复作业。
	Dispatch(ctx context.Context, order model.WorkOrder, idemKey string) (model.DeviceReceipt, error)
	// QueryStatus 查最新回执。
	QueryStatus(ctx context.Context, ref StatusRef) (model.DeviceReceipt, error)
	// Cancel 撤销任务；设备侧已终结时返回当前回执（状态非 CANCELED），由调用方按映射表处理。
	Cancel(ctx context.Context, deviceJobID string) (model.DeviceReceipt, error)
}

// 设备调用错误。下发器（L4）按类别决定重试 / 查询 / 对账。
var (
	// ErrDeviceNotOpen 表示设备类型未注册或注册为不可开放（四类真实设备骨架）。
	ErrDeviceNotOpen = errors.New("adapter: device type not open")
	// ErrCapability 表示能力声明不满足开放规则，或调用了未声明的能力（如无 ExternalQuery 时按 idem_key 查）。
	ErrCapability = errors.New("adapter: capability not supported")
	// ErrUnavailable 表示平台不可用（连接失败 / 5xx），请求未被处理，可退避重试。
	ErrUnavailable = errors.New("adapter: device platform unavailable")
	// ErrTimeout 表示请求超时：平台是否受理未知，Dispatch 超时后须按 idem_key 查询再决定。
	ErrTimeout = errors.New("adapter: device platform timeout")
	// ErrRejected 表示平台拒绝请求本身（4xx，如参数不合法、幂等键冲突），重试无效。
	ErrRejected = errors.New("adapter: request rejected by device platform")
	// ErrJobNotFound 表示平台查无此任务。
	ErrJobNotFound = errors.New("adapter: device job not found")
	// ErrBadReceipt 表示平台回执不合规（无法解析、状态未登记、字段自相矛盾）。
	ErrBadReceipt = model.ErrBadReceipt
)
