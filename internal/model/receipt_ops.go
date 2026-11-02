package model

// P1 L4-B：回执入库 / 待对账 / 中止补偿 / DONE 回写用到的取值（P1 §6.5 技术实现第 5-9 条）。

// RawReceiptRaw 是设备回执原文存证（device_receipt.raw_object_id 指向它；未匹配回执也存它）。
const RawReceiptRaw RawKind = "RECEIPT_RAW"

// ActorDevice 是设备回执驱动的迁移发起方（work_order_transition.actor_type=device）。
const ActorDevice ActorType = "device"

// L4 新增的迁移原因（work_order_transition.reason_code）。
const (
	ReasonReceipt        ReasonCode = "RECEIPT"          // 设备回执推进
	ReasonReconciled     ReasonCode = "RECONCILED"       // 人工对账
	ReasonCanceledByUser ReasonCode = "CANCELED_BY_USER" // 用户 / 审批人中止
)

// L4 新增的审计动作。
const (
	AuditReceiptUnmatched AuditAction = "RECEIPT_UNMATCHED" // 回执反查不到归属，写在 (0,0)，转人工
	AuditReconcile        AuditAction = "RECONCILE"         // 人工对账结论
	AuditOrderCancel      AuditAction = "ORDER_CANCEL"      // 用户发起中止
)

// EpisodicWorkDone 是 REAL 单 DONE / PARTIALLY_DONE 写入的 episodic_memory.kind（ref_id=order_id）。
const EpisodicWorkDone = "WORK_DONE"

// ReceiptOutcome 是 device_receipt.effect：本条回执对状态机的实际作用。
type ReceiptOutcome string

const (
	OutcomeApplied  ReceiptOutcome = "APPLIED"  // 前进：已更新 dispatch_attempt 并按映射表处置
	OutcomeStale    ReceiptOutcome = "STALE"    // 不前进（乱序 / 重复状态），只入库
	OutcomeLate     ReceiptOutcome = "LATE"     // 工单已终态，只入库
	OutcomeConflict ReceiptOutcome = "CONFLICT" // 与已有终态矛盾，只入库并告警
)

// ReceiptSource 是 device_receipt.source。
type ReceiptSource string

const (
	SourceWebhook ReceiptSource = "WEBHOOK" // 设备平台推送
	SourcePoll    ReceiptSource = "POLL"    // 回执超时扫描主动 QueryStatus
	SourceManual  ReceiptSource = "MANUAL"  // 中止处理器调 Cancel 拿到的回执
)

// Valid 判断是否为已登记的来源。
func (s ReceiptSource) Valid() bool {
	return s == SourceWebhook || s == SourcePoll || s == SourceManual
}

// AwaitingReconcile 判断尝试是否在待对账列表（回执映射表 Effect=RECONCILE：NO_RECEIPT、FAILED+UNKNOWN）。
func AwaitingReconcile(s AttemptStatus, kind FailureKind) bool {
	return s == AttemptNoReceipt || (s == AttemptFailed && kind == FailureUnknown)
}

// AttemptRank 是尝试状态的前进序（SENT < ACCEPTED = 待对账 < EXECUTING < 终结），回执只在严格更大时生效。
// 待对账（NO_RECEIPT、FAILED+UNKNOWN）与 ACCEPTED 同级：迟到的 EXECUTING / 终结回执照常推进，迟到的 ACCEPTED 记 STALE。
func AttemptRank(s AttemptStatus, kind FailureKind) int {
	switch {
	case s == AttemptSent:
		return 0
	case s == AttemptAccepted || AwaitingReconcile(s, kind):
		return 1
	case s == AttemptExecuting:
		return 2
	case s.IsFinal():
		return 3
	}
	return -1
}
