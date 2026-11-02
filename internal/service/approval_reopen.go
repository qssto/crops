package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// notifyKindRecheckReturned 是复检退回通知的 kind（outbox notify.send 的 dedup_key 前缀第二段）。
const notifyKindRecheckReturned = "RECHECK_RETURNED"

// ReopenStore 是复检退回的票据 / 通知写入，repository.ReopenRepo 实现；*Tx 方法只在调用方事务内使用。
type ReopenStore interface {
	Ticket(ctx context.Context, db *repository.DB, scope model.Scope, ticketID uint64) (repository.TicketRow, error)
	SupersedeTicketTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, now time.Time) error
	InsertReopenedTicketTx(ctx context.Context, tx *repository.DB, t model.ApprovalTicket, expiresAt, now time.Time) (uint64, error)
	InsertNotifyOutboxTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, dedupKey string, payload []byte,
		now time.Time) (bool, error)
}

// ReopenService 是复检退回（P1 §6.4 第 4 条）：下发前 PRE_DISPATCH 约束不通过时把已批单退回重审、换一张新票。
// 复用 ApprovalService 的依赖（快照、定级 TTL、约束复核、迁移入口、事件）。
type ReopenService struct {
	a     *ApprovalService
	store ReopenStore
}

// NewReopenService 校验依赖并构造。
func NewReopenService(a *ApprovalService, store ReopenStore) (*ReopenService, error) {
	if a == nil || store == nil {
		return nil, fmt.Errorf("service: reopen needs approval service and store")
	}
	return &ReopenService{a: a, store: store}, nil
}

// ReopenRequest 是一次复检退回：TicketID 为被退回的 APPROVED 票（order.dispatch 消息里的 ticket_id）；
// Order 为复检时的作业单（ConstraintChecks 放 PRE_DISPATCH 复检行，新票 PRE_APPROVAL 行由它复核得出），
// Card 为原建议卡（新快照展示用），RuleIDs 为未通过的 rule_id（不可为空）。
type ReopenRequest struct {
	TicketID uint64
	Order    model.WorkOrder
	Card     model.AdviceCard
	RuleIDs  []string
}

// ReopenPlan 是 Prepare 的结果（锁外读旧票、写新快照），交给持 lock:order 的 Reopen 落库。
type ReopenPlan struct {
	old     repository.TicketRow
	next    model.ApprovalTicket
	order   model.WorkOrder
	ruleIDs []string
}

// recheckReturned 是 notify.send「复检退回」的 payload（真实通道在 L5，worker 占位处理）。
type recheckReturned struct {
	Kind         string   `json:"kind"`
	OrderID      uint64   `json:"order_id"`
	RunID        uint64   `json:"run_id"`
	OldTicketID  uint64   `json:"old_ticket_id"`
	NewTicketID  uint64   `json:"new_ticket_id"`
	NewTicketSeq int      `json:"new_ticket_seq"`
	RuleIDs      []string `json:"rule_ids"`
}

// Prepare 在锁外读旧票（含 row_ver）并写新快照（ticket_seq+1，initiator_id、风险定级沿用旧票）：
// 旧票须属该单且为 APPROVED，否则返回 ErrBadApproval（已退回过的重复投递由下发器 ① 先行丢弃）。
func (s *ReopenService) Prepare(ctx context.Context, req ReopenRequest) (ReopenPlan, error) {
	o := req.Order
	if req.TicketID == 0 || o.OrderID == 0 || len(req.RuleIDs) == 0 {
		return ReopenPlan{}, fmt.Errorf("%w: reopen needs ticket_id, order_id and rule_ids", ErrBadApproval)
	}
	scope := model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}
	old, err := s.store.Ticket(ctx, s.a.d.DB, scope, req.TicketID)
	if err != nil {
		return ReopenPlan{}, fmt.Errorf("service: reopen order %d: %w", o.OrderID, err)
	}
	if old.OrderID != o.OrderID || model.TicketStatus(old.Status) != model.TicketApproved {
		return ReopenPlan{}, fmt.Errorf("%w: ticket %d is order %d / %s, want order %d / APPROVED",
			ErrBadApproval, old.ID, old.OrderID, old.Status, o.OrderID)
	}
	prev, err := old.Ticket()
	if err != nil {
		return ReopenPlan{}, fmt.Errorf("service: reopen order %d: %w", o.OrderID, err)
	}
	next := model.ApprovalTicket{RunID: prev.RunID, TenantID: prev.TenantID, FarmID: prev.FarmID, OrderID: prev.OrderID,
		TicketKind: model.TicketKindOrder, TicketSeq: prev.TicketSeq + 1, RiskLevel: prev.RiskLevel,
		RequiredRole: prev.RequiredRole, ConfirmRequired: prev.ConfirmRequired, InitiatorID: prev.InitiatorID,
		Status: model.TicketOpen}
	next.Snapshot, err = s.a.putSnapshot(ctx, next,
		agent.ApprovalRequest{Order: o, Card: req.Card, InitiatorID: prev.InitiatorID}, s.a.d.Clock.Now())
	if err != nil {
		return ReopenPlan{}, err
	}
	return ReopenPlan{old: old, next: next, order: o, ruleIDs: append([]string(nil), req.RuleIDs...)}, nil
}

// Reopen 单事务落库复检退回（调用方须持 lock:order）：work_order 行锁 → 旧票 APPROVED → SUPERSEDED（row_ver 守卫，
// 影响 0 行 → model.ErrConcurrentUpdate，整事务回滚）→ APPROVED → PENDING_APPROVAL(RECHECK_FAILED) →
// 新票 OPEN（expires_at = now + 定级 TTL）→ constraint_check(PRE_APPROVAL, attempt = 新 ticket_seq) →
// INSERT IGNORE outbox(notify.send, dedup_key = notify:RECHECK_RETURNED:{order_id}:{旧 ticket_id}) →
// APPROVAL_OPEN(ReopenedPayload，actor=system；EventRepo 同事务投影 audit APPROVAL)。不 resume、不关 run。
func (s *ReopenService) Reopen(ctx context.Context, p ReopenPlan) (model.ApprovalTicket, error) {
	d := s.a.d
	t := p.next
	if t.OrderID == 0 || p.old.ID == 0 {
		return model.ApprovalTicket{}, fmt.Errorf("%w: empty reopen plan", ErrBadApproval)
	}
	ttl, err := d.Risk.TTL(t.RiskLevel)
	if err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("service: reopen order %d: %w", t.OrderID, err)
	}
	checks, _ := d.Constraints.Review(p.order) // 未通过是退回原因本身，照样落库给审批人看
	now := d.Clock.Now()
	expires := now.Add(ttl)
	ref := p.old.OrderRef()
	notified := true
	err = d.Tx.InTx(ctx, func(tx *repository.DB) error {
		if err := d.Tickets.LockOrderTx(ctx, tx, ref); err != nil {
			return err
		}
		if err := s.store.SupersedeTicketTx(ctx, tx, p.old, now); err != nil {
			return err
		}
		if err := d.WorkOrders.TransitionFrom(ctx, tx, ref, model.OrderApproved, model.OrderPendingApproval,
			model.Actor{Type: model.ActorSystem}, model.ReasonRecheckFailed); err != nil {
			return err
		}
		id, err := s.store.InsertReopenedTicketTx(ctx, tx, t, expires, now)
		if err != nil {
			return err
		}
		t.TicketID = id
		if err := d.Tickets.UpsertChecksTx(ctx, tx, ref, t.TicketSeq, checks, now); err != nil {
			return err
		}
		if notified, err = s.notifyTx(ctx, tx, ref, p, t, now); err != nil {
			return err
		}
		return s.appendReopened(ctx, tx, t, p, now)
	})
	if err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("service: reopen order %d ticket %d: %w", t.OrderID, p.old.ID, err)
	}
	if !notified {
		d.Logger.WarnContext(ctx, "reopen: notify already queued", "order_id", t.OrderID, "old_ticket_id", p.old.ID)
	}
	d.Logger.InfoContext(ctx, "reopen: order returned to approval", "order_id", t.OrderID, "old_ticket_id", p.old.ID,
		"ticket_id", t.TicketID, "ticket_seq", t.TicketSeq, "rule_ids", p.ruleIDs)
	t.ExpiresAt = &expires
	return t, nil
}

// RecheckReturnedDedupKey 是复检退回通知的 outbox dedup_key（同一张旧票只通知一次）。
func RecheckReturnedDedupKey(orderID, oldTicketID uint64) string {
	return fmt.Sprintf("notify:%s:%d:%d", notifyKindRecheckReturned, orderID, oldTicketID)
}

func (s *ReopenService) notifyTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, p ReopenPlan,
	t model.ApprovalTicket, now time.Time) (bool, error) {
	body, err := json.Marshal(recheckReturned{Kind: notifyKindRecheckReturned, OrderID: t.OrderID, RunID: t.RunID,
		OldTicketID: p.old.ID, NewTicketID: t.TicketID, NewTicketSeq: t.TicketSeq, RuleIDs: p.ruleIDs})
	if err != nil {
		return false, fmt.Errorf("encode notify payload: %w", err)
	}
	return s.store.InsertNotifyOutboxTx(ctx, tx, ref, RecheckReturnedDedupKey(t.OrderID, p.old.ID), body, now)
}

func (s *ReopenService) appendReopened(ctx context.Context, tx *repository.DB, t model.ApprovalTicket, p ReopenPlan,
	now time.Time) error {
	b, err := json.Marshal(model.ReopenedPayload{TicketID: t.TicketID, OrderID: t.OrderID, TicketSeq: t.TicketSeq,
		RiskLevel: t.RiskLevel, ConfirmRequired: t.ConfirmRequired, ReopenedFrom: p.old.ID,
		Reason: model.ReasonRecheckFailed, RuleIDs: p.ruleIDs})
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", model.EventApprovalOpen, err)
	}
	if _, err := s.a.d.Events.AppendTx(ctx, tx, model.RunEvent{TenantID: t.TenantID, FarmID: t.FarmID, RunID: t.RunID,
		Type: model.EventApprovalOpen, Actor: model.ActorSystem, Payload: b, CreatedAt: now}); err != nil {
		return fmt.Errorf("append %s: %w", model.EventApprovalOpen, err)
	}
	return nil
}
