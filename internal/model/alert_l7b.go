package model

// P1 L7-B 告警补充：告警审计 action、全部告警项登记表与规则 / 清单用到的额外告警名（只加不改 alert.go）。

// 额外告警项（P1）：规则只基于业务指标，不经 AlertSink 触发；名字登记在这里供《告警清单》与规则一致性测试。
const (
	AlertResumeFailureRate AlertName = "resume_failure_rate" // P1：审批后续跑失败率
	AlertTokenCostAnomaly  AlertName = "token_cost_anomaly"  // P1：token 用量异常
	AlertMCPSuccessLow     AlertName = "mcp_success_low"     // P1：MCP 成功率低于阈值（按 server）
)

// AlertNames 是全部告警项（含 alert.go 与本文件），as_alert_total 的 alert label 只取这些值。
var AlertNames = []AlertName{
	AlertAdviceNoCitation, AlertModelActuate, AlertPreApprovalDevice, AlertAuditWriteFailed, AlertAuditChainBroken,
	AlertAuditExportPIILeak, AlertDispatchFailureRate, AlertApprovalBacklog, AlertRecheckReturned, AlertExpiredSurge,
	AlertCrossFarmDenied, AlertFlagChanged, AlertResumeFailureRate, AlertTokenCostAnomaly, AlertMCPSuccessLow,
}

// AlertActionPrefix 是告警审计行 action 的前缀：ALERT.{name}（audit_log.action varchar(32)）。
const AlertActionPrefix = "ALERT."

// AuditAction 返回该告警项的审计 action：ALERT.{name}。
func (n AlertName) AuditAction() AuditAction { return AuditAction(AlertActionPrefix + string(n)) }

// ListActions 返回告警列表按该告警项检索的审计 action：经 AlertSink 的写 ALERT.{name}；
// 跨农场拒绝与开关变更另有业务审计（CROSS_FARM_DENIED / FLAG_CHANGE），一并列出供下钻。
func (n AlertName) ListActions() []AuditAction {
	switch n {
	case AlertCrossFarmDenied:
		return []AuditAction{n.AuditAction(), AuditCrossFarmDenied}
	case AlertFlagChanged:
		return []AuditAction{n.AuditAction(), AuditFlagChange}
	}
	return []AuditAction{n.AuditAction()}
}

// Known 表示 n 已登记。
func (n AlertName) Known() bool {
	for _, x := range AlertNames {
		if x == n {
			return true
		}
	}
	return false
}
