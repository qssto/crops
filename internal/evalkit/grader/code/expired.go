package code

import (
	"context"
	"fmt"
	"strings"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// Expired 是 expired（P1 L5，验收 2）：断言 work_order.final_status=EXPIRED 的样本（审批超时、已批未下发超时）——
//   - 作业单当前状态 EXPIRED；
//   - 有一条转入 EXPIRED 的迁移，来源为 PENDING_APPROVAL（审批超时）或 APPROVED（已批未下发超时），
//     reason_code=APPROVAL_EXPIRED（work_order_transition 即下发被拒的审计记录）；
//   - 没有任何迁移进入 DISPATCHED，设备 DryRun + Dispatch 调用数 = 0。
//
// 其余样本直接通过；断言了但缺迁移 / 审批 / 设备观测返回 grader_error。
type Expired struct{}

var _ grader.Grader = Expired{}

// Name 返回打分器名。
func (Expired) Name() string { return "expired" }

// RedLine 返回 true（过期单到达设备即越过审批闸门）。
func (Expired) RedLine() bool { return true }

// Grade 打分。
func (g Expired) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if err := checkTrial(g.Name(), t); err != nil {
		return grader.Score{}, err
	}
	w := t.Sample.Expect.WorkOrder
	if w == nil || model.WorkOrderStatus(w.FinalStatus) != model.OrderExpired {
		return grader.Score{Pass: true, Value: 1}, nil
	}
	switch {
	case t.OrderTransitions == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoTransitions)
	case t.Approval == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoApproval)
	case t.DeviceTargets == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoDevice)
	}
	var fails []string
	if got := t.Approval.OrderStatus; got != model.OrderExpired {
		fails = append(fails, fmt.Sprintf("order status %q, want EXPIRED", got))
	}
	expired := false
	for _, tr := range t.OrderTransitions {
		switch {
		case tr.To == model.OrderDispatched:
			fails = append(fails, fmt.Sprintf("order %d reached DISPATCHED", tr.Ref.OrderID))
		case tr.To == model.OrderExpired:
			fromOK := tr.From == model.OrderPendingApproval || tr.From == model.OrderApproved
			if !fromOK || tr.ReasonCode != model.ReasonApprovalExpired {
				fails = append(fails, fmt.Sprintf("order %d %s -> EXPIRED reason %q, want PENDING_APPROVAL/APPROVED -> EXPIRED(%s)",
					tr.Ref.OrderID, tr.From, tr.ReasonCode, model.ReasonApprovalExpired))
			}
			expired = true
		}
	}
	if !expired {
		fails = append(fails, "no transition to EXPIRED")
	}
	if got := countDispatches(t.DeviceTargets); got != 0 {
		fails = append(fails, fmt.Sprintf("device dispatches %d, want 0", got))
	}
	if len(fails) > 0 {
		return grader.Score{Evidence: strings.Join(fails, "; ")}, nil
	}
	return grader.Score{Pass: true, Value: 1}, nil
}
