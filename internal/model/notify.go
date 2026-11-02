package model

import "fmt"

// P1 L5 / L6 共用的 notify.send 接缝（第 4 批基线）：通知类型、dedup_key 格式与 payload 公共字段。
// 写入方只经 outbox(notify.send) + INSERT IGNORE（uk_dedup 去重），处理器与通道归 L5。

// NotifyKind 是 notify.send 消息的通知类型。
type NotifyKind string

const (
	NotifyPendingApproval NotifyKind = "PENDING_APPROVAL" // L5：Activate 后待审批（§6.6 流程 1）
	NotifyExpiringSoon    NotifyKind = "EXPIRING_SOON"    // L5：approval_expiring 扫描，expires_at 前 1h（§6.6 流程 5）
	NotifyRecheckReturned NotifyKind = "RECHECK_RETURNED" // L4-A：PRE_DISPATCH 复检不通过退回重审
	NotifyDeviceOffline   NotifyKind = "DEVICE_OFFLINE"   // L6：下发器 ③ 设备离线，只通知一次（§6.7 流程 4）
)

// NotifyDedupKey 是 notify.send 的 outbox dedup_key：notify:{kind}:{order_id}:{ticket_id}。
func NotifyDedupKey(kind NotifyKind, orderID, ticketID uint64) string {
	return fmt.Sprintf("notify:%s:%d:%d", kind, orderID, ticketID)
}

// NotifyEnvelope 是 notify.send payload 的公共字段；各类通知在此之上追加自己的字段（处理器忽略未知字段）。
type NotifyEnvelope struct {
	Kind     NotifyKind `json:"kind"`
	OrderID  uint64     `json:"order_id"`
	RunID    uint64     `json:"run_id,omitempty"`
	TicketID uint64     `json:"ticket_id,omitempty"`
}
