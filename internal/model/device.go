package model

import "time"

// RawDeviceCmd 是设备下发原始指令归档（P1 §6.3：dispatch_attempt.command_raw_id 指向它）。
const RawDeviceCmd RawKind = "DEVICE_CMD"

// AttemptStatus 是 dispatch_attempt.status，也是设备回执的规范状态（P1 §6.3 回执映射表）。
type AttemptStatus string

const (
	AttemptSent          AttemptStatus = "SENT"           // 已写尝试、未得到平台受理
	AttemptAccepted      AttemptStatus = "ACCEPTED"       // 平台受理，返回 device_job_id
	AttemptExecuting     AttemptStatus = "EXECUTING"      // 设备执行中
	AttemptDone          AttemptStatus = "DONE"           // 全部完成
	AttemptFailed        AttemptStatus = "FAILED"         // 失败，细分见 FailureKind
	AttemptCanceled      AttemptStatus = "CANCELED"       // 已撤销
	AttemptPartiallyDone AttemptStatus = "PARTIALLY_DONE" // 部分完成，带 done_area_mu
	AttemptNoReceipt     AttemptStatus = "NO_RECEIPT"     // 过 receipt_due_at 仍无回执（L4 扫描置）
)

// Valid 判断是否为已登记的尝试状态。
func (s AttemptStatus) Valid() bool {
	switch s {
	case AttemptSent, AttemptAccepted, AttemptExecuting, AttemptDone, AttemptFailed, AttemptCanceled,
		AttemptPartiallyDone, AttemptNoReceipt:
		return true
	}
	return false
}

// IsFinal 判断设备侧是否已终结（不再有新回执）。NO_RECEIPT 不终结：可能迟到回执，转人工对账。
func (s AttemptStatus) IsFinal() bool {
	switch s {
	case AttemptDone, AttemptFailed, AttemptCanceled, AttemptPartiallyDone:
		return true
	}
	return false
}

// FailureKind 是 dispatch_attempt.failure_kind：FAILED 回执的失败阶段。
type FailureKind string

const (
	FailureNotExecuted FailureKind = "NOT_EXECUTED" // 设备未动作：可换下一次 attempt 重试（L4）
	FailureInExecution FailureKind = "IN_EXECUTION" // 执行中失败：作业单 FAILED，不自动重试
	FailureUnknown     FailureKind = "UNKNOWN"      // 无法判断是否动作：人工对账
)

// Valid 判断是否为已登记的失败阶段。
func (k FailureKind) Valid() bool {
	switch k {
	case FailureNotExecuted, FailureInExecution, FailureUnknown:
		return true
	}
	return false
}

// DispatchAttempt 是 dispatch_attempt 表行（0007）。
type DispatchAttempt struct {
	ID           uint64
	TenantID     uint64
	FarmID       uint64
	OrderID      uint64
	TicketID     uint64
	Attempt      int
	DeviceType   string
	DeviceID     string
	IdemKey      string
	DeviceJobID  string // 空表示平台尚未受理
	Mode         DispatchMode
	Status       AttemptStatus
	FailureKind  FailureKind // 空表示无
	DoneAreaMu   *float64
	CommandRawID uint64
	FlagReadAt   time.Time
	SentAt       time.Time
	ReceiptDueAt time.Time
	RowVer       int
	UpdatedAt    time.Time
}

// Ref 返回尝试所属作业单。
func (a DispatchAttempt) Ref() OrderRef {
	return OrderRef{TenantID: a.TenantID, FarmID: a.FarmID, OrderID: a.OrderID}
}

// 设备调用方法名（DeviceCallTrace.Method）。
const (
	DeviceMethodDispatch    = "Dispatch"
	DeviceMethodQueryStatus = "QueryStatus"
	DeviceMethodCancel      = "Cancel"
)

// DeviceCallTrace 是一次设备 Adapter 调用的 trace 记录（P1 §6.3 验收 4）。由 adapter.Traced 生成，
// 记录方（service）只依赖本类型，不 import adapter。
type DeviceCallTrace struct {
	ToolCallID   string
	Method       string
	DeviceType   string
	TenantID     uint64
	FarmID       uint64
	OrderID      uint64
	RunID        uint64
	IdemKey      string
	DeviceJobID  string
	Status       AttemptStatus // 回执状态；出错时为空
	Code         string        // 平台回执码
	CommandRawID uint64        // 仅 Dispatch：原始指令 raw_object.id
	Latency      time.Duration
	Error        string
}
