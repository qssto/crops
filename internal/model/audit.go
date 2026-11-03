package model

// AuditAction 是 audit_log.action（P0 L7 SQL 注释的取值）。
type AuditAction string

const (
	AuditAsk             AuditAction = "ASK"
	AuditToolCall        AuditAction = "TOOL_CALL"
	AuditKBLoad          AuditAction = "KB_LOAD"
	AuditLLMCall         AuditAction = "LLM_CALL"
	AuditValidate        AuditAction = "VALIDATE"
	AuditApproval        AuditAction = "APPROVAL"
	AuditOrderTransition AuditAction = "ORDER_TRANSITION"
	AuditDispatch        AuditAction = "DISPATCH"
	AuditRawRead         AuditAction = "RAW_READ"
	// P1
	AuditFlagChange      AuditAction = "FLAG_CHANGE"       // L0：开关变更（actor / 旧值 / 新值 / scope）
	AuditCrossFarmDenied AuditAction = "CROSS_FARM_DENIED" // L1：跨农场 / IDOR / 越权访问拒绝
	AuditDispatchHeld    AuditAction = "DISPATCH_HELD"     // L0/L8：下发器第 ② 步读到开关关闭
	// P1 L7
	AuditExport         AuditAction = "AUDIT_EXPORT"    // 审计导出文件生成（target export:{id}）
	AuditRetentionPurge AuditAction = "RETENTION_PURGE" // 保留期清理一批（summary {kind, count, max_expire_at}）
	// 敏感操作二次留痕（§6.8 技术实现第 2 条）：与主审计同事务，target = 主记录 id。
	AuditSensitiveFlagChange     AuditAction = "SENSITIVE.FLAG_CHANGE"
	AuditSensitiveApprovalDecide AuditAction = "SENSITIVE.APPROVAL_DECIDE"
	AuditSensitiveOrderCancel    AuditAction = "SENSITIVE.ORDER_CANCEL"
	AuditSensitiveRawRead        AuditAction = "SENSITIVE.RAW_READ"
	AuditSensitiveExport         AuditAction = "SENSITIVE.AUDIT_EXPORT"
	// P1 L8-B：一键关停演练（平台链 (0,0)，target flag:enable_robot_dispatch，summary 含 T0 / 耗时毫秒 / 各计数）。
	AuditKillSwitchDrill AuditAction = "KILL_SWITCH_DRILL"
)

// audit_log.result 取值。
const (
	AuditResultOK     = "OK"
	AuditResultFailed = "FAILED"
	AuditResultDenied = "DENIED"
)

// eventAudit 是 event_type → audit_log.action 的投影表（P0 L7 流程 6）；未列出的（STEP_*、
// RUN_INTERRUPTED / RUN_RESUMED / RUN_COMPLETED / RUN_FAILED）不投影。
var eventAudit = map[EventType]AuditAction{
	EventRunStarted:       AuditAsk,
	EventToolCallEnd:      AuditToolCall,
	EventKBLoaded:         AuditKBLoad,
	EventLLMCallEnd:       AuditLLMCall,
	EventValidationFailed: AuditValidate,
	EventAdviceReady:      AuditValidate,
	EventApprovalOpen:     AuditApproval,
	EventApprovalPending:  AuditApproval,
	EventApprovalDecided:  AuditApproval,
	EventOrderTransition:  AuditOrderTransition,
	EventDispatched:       AuditDispatch,
	// P1 L3：约束拦截按审批类审计，result=FAILED（repository.auditResult）。
	EventConstraintBlocked: AuditApproval,
	// P1 L6：降级事件（target fault:{fault}，result=OK）。
	EventDegraded: AuditDegraded,
}

// AuditAction 返回该事件投影到 audit_log 的 action；ok=false 表示不投影。
func (t EventType) AuditAction() (AuditAction, bool) {
	a, ok := eventAudit[t]
	return a, ok
}

// sensitiveAudit 是主审计 action → 二次留痕 action（P1 L7）。审批决策不经此表：APPROVAL 审计由多种事件投影，
// 决策留痕在写 approval_decision 的同一事务内单独写（repository.DecisionRepo.InsertDecisionTx）。
var sensitiveAudit = map[AuditAction]AuditAction{
	AuditFlagChange:  AuditSensitiveFlagChange,
	AuditOrderCancel: AuditSensitiveOrderCancel,
	AuditRawRead:     AuditSensitiveRawRead,
	AuditExport:      AuditSensitiveExport,
}

// SensitiveAudit 返回主审计成功（result=OK）时须同事务追加的二次留痕 action；ok=false 表示不是敏感操作。
func SensitiveAudit(action, result string) (AuditAction, bool) {
	if result != AuditResultOK {
		return "", false
	}
	a, ok := sensitiveAudit[AuditAction(action)]
	return a, ok
}
