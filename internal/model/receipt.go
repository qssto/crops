package model

import (
	"errors"
	"fmt"
	"time"
)

// ErrBadReceipt 表示设备回执不合规（状态未登记、FAILED 缺 failure_kind、面积为负等）。
var ErrBadReceipt = errors.New("model: bad device receipt")

// DeviceReceipt 是标准化后的设备回执（P1 §6.3）：各 Adapter 把自家协议转成它，下发器（L4）只认它。
// SENT / NO_RECEIPT 是本地态，设备不会回这两个状态。
type DeviceReceipt struct {
	Status      AttemptStatus
	DeviceJobID string      // 平台任务号；受理前为空
	DoneAreaMu  *float64    // 已完成面积（亩）；PARTIALLY_DONE 必填
	DeviceTime  time.Time   // 设备 / 平台侧时间（UTC），零值表示平台未给
	FailureKind FailureKind // 仅 FAILED
	Code        string      // 平台回执码（原样保留，进 trace）
}

// Validate 校验回执自洽。
func (r DeviceReceipt) Validate() error {
	if r.Status == AttemptSent || r.Status == AttemptNoReceipt {
		return fmt.Errorf("%w: status %q is not a device status", ErrBadReceipt, r.Status)
	}
	if err := CheckAttemptState(r.Status, r.FailureKind, r.DoneAreaMu); err != nil {
		return err
	}
	var problem string
	switch {
	case r.Status == AttemptPartiallyDone && (r.DoneAreaMu == nil || *r.DoneAreaMu <= 0):
		problem = "PARTIALLY_DONE needs done_area_mu > 0"
	case r.Status != AttemptFailed && r.DeviceJobID == "":
		problem = fmt.Sprintf("%s needs device_job_id", r.Status)
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrBadReceipt, problem)
}

// CheckAttemptState 校验写入 dispatch_attempt 的状态组合：status 已登记；FAILED 必带已登记的 failure_kind，
// 其余状态不得带；done_area_mu 非空时 >= 0。不合规返回 ErrBadReceipt（DeviceReceipt.Validate 与仓储写入共用）。
func CheckAttemptState(status AttemptStatus, kind FailureKind, doneAreaMu *float64) error {
	var problem string
	switch {
	case !status.Valid():
		problem = fmt.Sprintf("status %q is not registered", status)
	case status == AttemptFailed && !kind.Valid():
		problem = fmt.Sprintf("FAILED needs failure_kind, got %q", kind)
	case status != AttemptFailed && kind != "":
		problem = fmt.Sprintf("failure_kind %q only allowed with FAILED", kind)
	case doneAreaMu != nil && *doneAreaMu < 0:
		problem = "done_area_mu must be >= 0"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrBadReceipt, problem)
}

// ReceiptEffect 是回执对作业单的处置（L4 消费，本层不改状态机）。
type ReceiptEffect string

const (
	EffectNone        ReceiptEffect = "NONE"          // 不迁移，只更新 dispatch_attempt
	EffectTransition  ReceiptEffect = "TRANSITION"    // 迁移到 ReceiptRule.To
	EffectRetryOrFail ReceiptEffect = "RETRY_OR_FAIL" // 设备未动作：attempt+1 重试，末次 → FAILED
	EffectReconcile   ReceiptEffect = "RECONCILE"     // 不迁移，进待对账列表，人工处理
)

// ReceiptRule 是「回执 → WorkOrder 迁移事件」映射表的一行（docs/p1/回执映射表.md 与之一致）。
type ReceiptRule struct {
	Status      AttemptStatus
	FailureKind FailureKind // 仅 FAILED 行非空
	DoneArea    bool        // 仅 CANCELED 细分：true 表示 done_area_mu > 0（撤销前已部分作业）
	Effect      ReceiptEffect
	To          WorkOrderStatus // 仅 TRANSITION / RETRY_OR_FAIL（末次）非空
}

// ReceiptRules 是完整映射表：每个尝试状态（FAILED 按 failure_kind、CANCELED 按是否已有作业面积细分）恰好一行。
// CANCELED 且 done_area_mu > 0 记 PARTIALLY_DONE：撤销前已部分喷施，安全间隔期与重复作业判断必须计入（安全优先）。
var ReceiptRules = []ReceiptRule{
	{Status: AttemptSent, Effect: EffectNone},
	{Status: AttemptAccepted, Effect: EffectNone},
	{Status: AttemptExecuting, Effect: EffectTransition, To: OrderExecuting},
	{Status: AttemptDone, Effect: EffectTransition, To: OrderDone},
	{Status: AttemptPartiallyDone, Effect: EffectTransition, To: OrderPartiallyDone},
	{Status: AttemptCanceled, Effect: EffectTransition, To: OrderCanceled},
	{Status: AttemptCanceled, DoneArea: true, Effect: EffectTransition, To: OrderPartiallyDone},
	{Status: AttemptFailed, FailureKind: FailureInExecution, Effect: EffectTransition, To: OrderFailed},
	{Status: AttemptFailed, FailureKind: FailureNotExecuted, Effect: EffectRetryOrFail, To: OrderFailed},
	{Status: AttemptFailed, FailureKind: FailureUnknown, Effect: EffectReconcile},
	{Status: AttemptNoReceipt, Effect: EffectReconcile},
}

// MapReceipt 查映射表；非 FAILED 忽略 kind，非 CANCELED 忽略 doneAreaMu。表外组合返回 ErrBadReceipt。
// L4 传回执（或 dispatch_attempt 行）的 status / failure_kind / done_area_mu。
func MapReceipt(status AttemptStatus, kind FailureKind, doneAreaMu *float64) (ReceiptRule, error) {
	if status != AttemptFailed {
		kind = ""
	}
	area := status == AttemptCanceled && doneAreaMu != nil && *doneAreaMu > 0
	for _, r := range ReceiptRules {
		if r.Status == status && r.FailureKind == kind && r.DoneArea == area {
			return r, nil
		}
	}
	return ReceiptRule{}, fmt.Errorf("%w: no mapping for %s/%s", ErrBadReceipt, status, kind)
}
