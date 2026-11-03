package service

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 审批前设备调用拦截（P1 L7-B 红线）：下发器在每次可能动作设备的 Adapter 调用（DRY_RUN 的 DryRun、REAL 的 DispatchArchived
// 首发与续跑重发）之前，持 lock:order 在锁内重读作业单与 order.dispatch 消息里的审批票；任一不符即拒绝调用并
// Fire(pre_approval_device, P0, order_id)。复用 L4 第 ① 步的锁内重读，Adapter 层只做协议翻译、不做判断。

// 写入前另有 admitTicket 同口径校验票，不通过的消息直接丢弃（不推进状态、不写尝试）。

// ErrPreApprovalDevice 表示设备调用前复核发现审批未通过（票非 APPROVED / 票不属本单 / 单状态不符）：不调 Adapter，outbox 退避重试。
var ErrPreApprovalDevice = errors.New("service: device call before approval")

// DispatchTicketReader 读一张 ORDER 票（repository.ReopenRepo 实现）；db 为 nil 时用连接。
type DispatchTicketReader interface {
	Ticket(ctx context.Context, db *repository.DB, scope model.Scope, ticketID uint64) (repository.TicketRow, error)
}

// deviceCallCheck 是设备调用前对作业单状态的要求：
//   - DRY_RUN（DryRun）与 REAL（DispatchArchived）调用时意图都已落库，单须为 DISPATCHED；
//   - dryRun 为 true 时还要求尚无 dispatch_result（已有结果再调一次即重复动作）。
type deviceCallCheck struct {
	TicketID uint64
	DryRun   bool
}

// guardDeviceCall 在调用 Adapter 之前（调用方已持 lock:order）重读作业单与审批票。通过返回重读到的单；
// 不通过返回包装 ErrPreApprovalDevice 的错误并发 P0 告警；读库失败原样返回（不告警，outbox 退避重试）。
func (s *WorkOrderService) guardDeviceCall(ctx context.Context, ref model.OrderRef, c deviceCallCheck) (repository.DispatchOrder,
	error) {
	d := s.dispatch
	o, err := d.Store.OrderForDispatch(ctx, nil, ref)
	if err != nil {
		return o, fmt.Errorf("service: dispatch guard order %d: %w", ref.OrderID, err)
	}
	reason := ""
	switch {
	case model.WorkOrderStatus(o.Status) != model.OrderDispatched:
		reason = "order status " + o.Status
	case c.DryRun && o.HasResult:
		reason = "dry-run result already recorded"
	default:
		if reason, err = s.ticketBlockReason(ctx, ref, c.TicketID); err != nil {
			return o, err
		}
	}
	if reason == "" {
		return o, nil
	}
	s.fireBlocked(ctx, o, "device call blocked: "+reason)
	return o, fmt.Errorf("%w: order %d ticket %d: %s", ErrPreApprovalDevice, ref.OrderID, c.TicketID, reason)
}

// admitTicket 是写入前的审批票校验：admit / realReady / recordIntent 放行之前、任何写入（绑定设备、复检、Archive、
// SENT 尝试、迁移、idem 键）之前读票。不通过 → 发 P0 并返回 done=true：消息按"丢弃"惯例置 DONE、不重试，单与尝试保持原样
// （例如 Reopen 后旧票 T1 已 SUPERSEDED 的过时消息，不得把单推进 DISPATCHED、留下 SENT 尝试，进而挡住新票 T2 的合法消息）。
// 读库失败返回错误（outbox 退避重试，不告警）。guardDeviceCall 仍在 Adapter 调用前做最后复核（纵深防御）。
func (s *WorkOrderService) admitTicket(ctx context.Context, o repository.DispatchOrder, ticketID uint64) (bool, error) {
	reason, err := s.ticketBlockReason(ctx, o.Ref(), ticketID)
	if err != nil || reason == "" {
		return false, err
	}
	s.fireBlocked(ctx, o, "dispatch blocked before write: "+reason)
	s.dispatch.Logger.ErrorContext(ctx, "dispatch: approval ticket check failed before any write, message dropped",
		"alert", string(model.AlertPreApprovalDevice), "order_id", o.ID, "ticket_id", ticketID, "status", o.Status,
		"reason", reason)
	return true, nil
}

// ticketBlockReason 读审批票：须存在、属本单且 APPROVED。返回空串表示通过；读库失败返回错误。
func (s *WorkOrderService) ticketBlockReason(ctx context.Context, ref model.OrderRef, ticketID uint64) (string, error) {
	if ticketID == 0 {
		return "no approval ticket", nil
	}
	t, err := s.dispatch.Tickets.Ticket(ctx, nil, model.Scope{TenantID: ref.TenantID, FarmID: ref.FarmID}, ticketID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return "approval ticket not found", nil
	case err != nil:
		return "", fmt.Errorf("service: dispatch guard order %d ticket %d: %w", ref.OrderID, ticketID, err)
	case t.OrderID != ref.OrderID:
		return fmt.Sprintf("ticket belongs to order %d", t.OrderID), nil
	case model.TicketStatus(t.Status) != model.TicketApproved:
		return "ticket status " + t.Status, nil
	}
	return "", nil
}

// fireBlocked 记 blocked 下发结果并发 pre_approval_device P0（order_id 下钻）。
func (s *WorkOrderService) fireBlocked(ctx context.Context, o repository.DispatchOrder, detail string) {
	d := s.dispatch
	d.Observer.Dispatched(o.FarmID, model.DispatchMode(o.DispatchMode), DispatchBlocked)
	d.Alerts.Fire(ctx, model.Alert{Name: model.AlertPreApprovalDevice, Level: model.AlertP0,
		Scope: model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}, RunID: o.RunID, OrderID: o.ID, Detail: detail})
}
