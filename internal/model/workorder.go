package model

import "errors"

// WorkOrderStatus 是作业单状态（母文档 §4.5-2），Phase 1 才用到的状态也在此一并定义。
type WorkOrderStatus string

const (
	OrderDraft           WorkOrderStatus = "DRAFT"
	OrderPendingApproval WorkOrderStatus = "PENDING_APPROVAL"
	OrderApproved        WorkOrderStatus = "APPROVED"
	OrderDispatched      WorkOrderStatus = "DISPATCHED"
	OrderExecuting       WorkOrderStatus = "EXECUTING"
	OrderDone            WorkOrderStatus = "DONE"
	OrderPartiallyDone   WorkOrderStatus = "PARTIALLY_DONE"
	OrderRejected        WorkOrderStatus = "REJECTED"
	OrderExpired         WorkOrderStatus = "EXPIRED"
	OrderFailed          WorkOrderStatus = "FAILED"
	OrderCanceled        WorkOrderStatus = "CANCELED"
)

// WorkOrderStatuses 返回全部状态（用于表驱动测试与 schema 枚举比对）。
func WorkOrderStatuses() []WorkOrderStatus {
	return []WorkOrderStatus{
		OrderDraft, OrderPendingApproval, OrderApproved, OrderDispatched, OrderExecuting,
		OrderDone, OrderPartiallyDone, OrderRejected, OrderExpired, OrderFailed, OrderCanceled,
	}
}

// WorkOrderTransitions 是合法迁移表，照抄母文档 §4.6.2-11（P0 L1 冻结，各阶段只用不改）。
// 不在表中作为 from 出现的状态即终态。
var WorkOrderTransitions = map[WorkOrderStatus][]WorkOrderStatus{
	OrderDraft:           {OrderPendingApproval, OrderCanceled},
	OrderPendingApproval: {OrderApproved, OrderRejected, OrderExpired, OrderCanceled},
	// APPROVED → PENDING_APPROVAL：复检退回 / 排程变更；→ EXPIRED：超过 dispatch_deadline_at
	OrderApproved: {OrderDispatched, OrderPendingApproval, OrderExpired, OrderCanceled},
	// 允许跳过 EXECUTING，处理乱序回执
	OrderDispatched: {OrderExecuting, OrderDone, OrderPartiallyDone, OrderFailed, OrderCanceled},
	OrderExecuting:  {OrderDone, OrderPartiallyDone, OrderFailed, OrderCanceled},
}

// CanTransition 只查 WorkOrderTransitions 判断 from → to 是否合法。
func CanTransition(from, to WorkOrderStatus) bool {
	for _, allowed := range WorkOrderTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsTerminal 判断状态是否为终态（DONE / PARTIALLY_DONE / REJECTED / EXPIRED / FAILED / CANCELED）。
func (s WorkOrderStatus) IsTerminal() bool {
	_, hasNext := WorkOrderTransitions[s]
	return s.Valid() && !hasNext
}

// Valid 判断是否为已定义状态。
func (s WorkOrderStatus) Valid() bool {
	for _, v := range WorkOrderStatuses() {
		if v == s {
			return true
		}
	}
	return false
}

// DispatchMode 是下发方式，P0 固定 DRY_RUN。
type DispatchMode string

const (
	DispatchDryRun DispatchMode = "DRY_RUN"
	DispatchReal   DispatchMode = "REAL"
)

// WorkType 是作业类型（work_order.work_type）。P0 风险矩阵中 SPRAY 一律 HIGH。
type WorkType string

const (
	WorkSpray     WorkType = "SPRAY"
	WorkFertilize WorkType = "FERTILIZE"
	WorkIrrigate  WorkType = "IRRIGATE"
)

// ConstraintPhase 是约束校验阶段（constraint_check.phase）。
type ConstraintPhase string

const (
	PhasePreApproval ConstraintPhase = "PRE_APPROVAL"
	PhasePreDispatch ConstraintPhase = "PRE_DISPATCH"
)

var (
	// ErrIllegalTransition 表示迁移不在 WorkOrderTransitions 中。
	ErrIllegalTransition = errors.New("illegal work order transition")
	// ErrConcurrentUpdate 表示 row_ver 乐观锁冲突（UPDATE 影响行数为 0）。
	ErrConcurrentUpdate = errors.New("concurrent update")
	// ErrNotFound 表示按 (tenant_id, farm_id, id) 找不到记录。
	ErrNotFound = errors.New("not found")
)
