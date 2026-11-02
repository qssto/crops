package sample

import (
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/model"
)

// RuleConstraint 校验 P1 L3 约束样本：expect.constraint 与 fixtures.local_orders 的结构。
const RuleConstraint = "constraint_shape"

// 本地单 fixture 允许的状态：在途或完成（与 repository.LocalOrderRepo 定版 SQL 的过滤条件一致）。
var (
	LocalOrderInFlight = []model.WorkOrderStatus{model.OrderApproved, model.OrderDispatched, model.OrderExecuting}
	LocalOrderDone     = []model.WorkOrderStatus{model.OrderDone, model.OrderPartiallyDone}
)

var knownWorkTypes = []model.WorkType{model.WorkSpray, model.WorkFertilize, model.WorkIrrigate}

// Problem 返回本地单 fixture 的结构问题（空串表示合法）：work_type 取 model 枚举、status 在途或完成、updated_at 为 RFC3339。
func (f LocalOrderFixture) Problem() string {
	if !slices.Contains(knownWorkTypes, model.WorkType(f.WorkType)) {
		return fmt.Sprintf("work_type %q is not allowed", f.WorkType)
	}
	st := model.WorkOrderStatus(f.Status)
	if !slices.Contains(LocalOrderInFlight, st) && !slices.Contains(LocalOrderDone, st) {
		return fmt.Sprintf("status %q is neither in flight nor done", f.Status)
	}
	if _, err := time.Parse(time.RFC3339, f.UpdatedAt); err != nil {
		return fmt.Sprintf("updated_at %q is not RFC3339", f.UpdatedAt)
	}
	return ""
}

// checkConstraint：断言拦截的样本须 outcome=ADVICE 且 work_order.final_status=DRAFT（拦截后单留草稿）、不断言
// stop_at_approval（拦截不开票），rule_ids 非空且每个不超过 constraint_check.rule_id 列宽；local_orders 每条结构合法。
func checkConstraint(s *Sample) []string {
	var msgs []string
	for i, f := range s.Fixtures.LocalOrders {
		if p := f.Problem(); p != "" {
			msgs = append(msgs, fmt.Sprintf("fixtures.local_orders[%d]: %s", i, p))
		}
	}
	c := s.Expect.Constraint
	if c == nil {
		return msgs
	}
	if s.Expect.Outcome != model.OutcomeAdvice {
		msgs = append(msgs, "expect.constraint requires outcome=ADVICE")
	}
	if s.Expect.WorkOrder == nil || s.Expect.WorkOrder.FinalStatus != string(model.OrderDraft) {
		msgs = append(msgs, "expect.constraint requires expect.work_order.final_status=DRAFT")
	}
	if s.Expect.Trajectory.StopAtApproval {
		msgs = append(msgs, "expect.constraint conflicts with stop_at_approval (blocked orders open no ticket)")
	}
	if len(c.RuleIDs) == 0 {
		msgs = append(msgs, "expect.constraint.rule_ids is required")
	}
	for i, id := range c.RuleIDs {
		if id == "" || len(id) > constraint.RuleIDMax {
			msgs = append(msgs, fmt.Sprintf("expect.constraint.rule_ids[%d] %q must be 1..%d bytes", i, id, constraint.RuleIDMax))
		}
	}
	return msgs
}
