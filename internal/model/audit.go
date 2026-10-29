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
)

// audit_log.result 取值。
const (
	AuditResultOK     = "OK"
	AuditResultFailed = "FAILED"
	AuditResultDenied = "DENIED"
)

// eventAudit 是 event_type → audit_log.action 的投影表（P0 L7 流程 6）；未列出的（STEP_*、DEGRADED、
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
}

// AuditAction 返回该事件投影到 audit_log 的 action；ok=false 表示不投影。
func (t EventType) AuditAction() (AuditAction, bool) {
	a, ok := eventAudit[t]
	return a, ok
}
