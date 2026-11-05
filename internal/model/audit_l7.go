package model

import (
	"errors"
	"time"
)

// P1 L7：保留期与审计导出的领域常量。

// ErrRawExpired 表示 raw_object 已过保留期被清理（deleted_at 非空），对象本体已删除。
var ErrRawExpired = errors.New("原文已过保留期")

// RawKinds 返回全部已登记的 raw_object.kind（retention 配置须逐个声明天数）。
func RawKinds() []RawKind {
	return []RawKind{RawLLMIO, RawToolRaw, RawSnapshot, RawCheckpoint, RawExport, RawDeviceCmd, RawReceiptRaw,
		RawCompactedMsgs}
}

// 审计导出（outbox topic audit.export）的状态，写在 outbox.payload.state。
const (
	AuditExportPending = "PENDING"
	AuditExportDone    = "DONE"
	AuditExportFailed  = "FAILED"
)

// AuditExportPayload 是 outbox(topic=audit.export) 的 payload：POST 写入请求参数与 PENDING，worker 处理器回写结果。
// 区间 [From, To) 按 audit_log.created_at；ChainFrom / ChainTo 是实际导出的 chain_seq 区间（导出后回写）。
type AuditExportPayload struct {
	FarmID      uint64    `json:"farm_id"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	RequestID   string    `json:"request_id"`
	RequestedBy uint64    `json:"requested_by"`
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"` // 失败原因码（不含 PII）
	RawObjectID uint64    `json:"raw_object_id,omitempty"`
	Rows        uint64    `json:"rows,omitempty"`
	ChainFrom   uint64    `json:"chain_from,omitempty"`
	ChainTo     uint64    `json:"chain_to,omitempty"`
}

// 审计导出失败原因码（payload.error）。
const (
	AuditExportErrChainBroken = "CHAIN_BROKEN" // 导出前 verify 发现链断
	AuditExportErrPIILeak     = "PII_LEAK"     // pii_clean 扫描出口文件命中，对象已删
)
