package model

// P1 L4-A：下发器真实通道、复检退回与 outbox 租约用到的原因码与审计动作（只追加，不改 P0 / P1 已有取值）。

const (
	// ReasonRecheckFailed：下发前 PRE_DISPATCH 约束不通过，APPROVED → PENDING_APPROVAL 退回重审（§6.4 第 4 条）。
	ReasonRecheckFailed ReasonCode = "RECHECK_FAILED"
	// ReasonRetryBlocked：重试（attempt > 1）在 ①②④ 被拦下（过时效 / 开关关 / 复检不通过），DISPATCHED → FAILED（§6.5 第 8 步）。
	ReasonRetryBlocked ReasonCode = "RETRY_BLOCKED"
	// ReasonDispatchRejected：设备平台明确拒绝受理（adapter.ErrRejected，设备未动作），DISPATCHED → FAILED。
	ReasonDispatchRejected ReasonCode = "DISPATCH_REJECTED"
	// ReasonDeviceUnavailable：第 ③ 步选不到设备且等待超限（过 dispatch_deadline_at，无时效时超 dispatch.device_wait_max），
	// 放弃下发：首次 APPROVED → EXPIRED（迁移表里 APPROVED 不能直接 → FAILED）、重试 DISPATCHED → FAILED。
	ReasonDeviceUnavailable ReasonCode = "DEVICE_UNAVAILABLE"
)

const (
	AuditDeviceUnavailable AuditAction = "DEVICE_UNAVAILABLE" // L4-A ③：选不到本农场在线设备，推后重试（DENIED）；等待超限放弃（FAILED）
	AuditOutboxDead        AuditAction = "OUTBOX_DEAD"        // L4-A：outbox 消息 5 次失败转 DEAD
	AuditNotifySend        AuditAction = "NOTIFY_SEND"        // L4-A：notify.send 占位处理（真实通道在 L5）
	AuditDispatchManual    AuditAction = "DISPATCH_MANUAL"    // L4-A：SENT 无受理且设备既无幂等键也无外部查询，转人工
)

// ReopenedPayload 是复检退回后新票的 APPROVAL_OPEN 事件 payload（在 P0 openPayload 字段上追加退回来源与原因）。
type ReopenedPayload struct {
	TicketID        uint64     `json:"ticket_id"`
	OrderID         uint64     `json:"order_id"`
	TicketSeq       int        `json:"ticket_seq"`
	RiskLevel       RiskLevel  `json:"risk_level"`
	ConfirmRequired bool       `json:"confirm_required"`
	ReopenedFrom    uint64     `json:"reopened_from"`
	Reason          ReasonCode `json:"reason"`
	RuleIDs         []string   `json:"rule_ids"`
}

// RealDispatchedPayload 是 REAL 单 DISPATCHED 事件 payload（INTENT / RESULT），在 DispatchedPayload 上追加尝试信息。
type RealDispatchedPayload struct {
	DispatchedPayload
	Attempt     int    `json:"attempt"`
	DeviceType  string `json:"device_type"`
	DeviceID    string `json:"device_id"`
	DeviceJobID string `json:"device_job_id,omitempty"`
}
