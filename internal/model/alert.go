package model

import "context"

// P1 L7 告警接缝：告警项名与级别对齐 Gate 1 第 7 条的 P0/P1 口径，规则见 deploy/alerts/p1.yaml，处置见《告警清单》。
// 接口放在 model，repository 层（审计封链）也能持有。

// AlertLevel 是告警级别。
type AlertLevel string

const (
	AlertP0 AlertLevel = "P0" // 电话告警
	AlertP1 AlertLevel = "P1"
)

// AlertName 是告警项名（Prometheus label 取值，只用本文件登记的常量）。
type AlertName string

const (
	AlertAdviceNoCitation    AlertName = "advice_no_citation"    // P0：非拒答建议无引用
	AlertModelActuate        AlertName = "model_actuate"         // P0：模型发起设备动作
	AlertPreApprovalDevice   AlertName = "pre_approval_device"   // P0：审批前设备调用
	AlertAuditWriteFailed    AlertName = "audit_write_failed"    // P0：审计写失败 / 封链失败
	AlertAuditChainBroken    AlertName = "audit_chain_broken"    // P0：校验或锚点核对发现链断
	AlertAuditExportPIILeak  AlertName = "audit_export_pii_leak" // P0：导出文件 pii_clean 命中
	AlertDispatchFailureRate AlertName = "dispatch_failure_rate" // P1
	AlertApprovalBacklog     AlertName = "approval_backlog"      // P1
	AlertRecheckReturned     AlertName = "recheck_returned"      // P1
	AlertExpiredSurge        AlertName = "expired_surge"         // P1
	AlertCrossFarmDenied     AlertName = "cross_farm_denied"     // P1
	AlertFlagChanged         AlertName = "flag_changed"          // P1
)

// Alert 是一次告警；RunID / OrderID 至少一个非 0 时供下钻（不进 Prometheus label）。
type Alert struct {
	Name    AlertName
	Level   AlertLevel
	Scope   Scope
	RunID   uint64
	OrderID uint64
	Detail  string // 不含 PII
}

// AlertSink 接收告警。不返回错误：告警失败不得影响业务路径，实现自己记日志。
type AlertSink interface {
	Fire(ctx context.Context, a Alert)
}
