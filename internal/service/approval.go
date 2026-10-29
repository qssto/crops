package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// firstTicketSeq 是作业单第一张票的序号（§6.6 定版 INSERT 写死 1；P1 复检退回换票 +1）。
const firstTicketSeq = 1

// snapshotContentType 是审批快照对象的类型。
const snapshotContentType = "application/json"

// ErrBadApproval 表示开票 / 激活参数不合法。
var ErrBadApproval = errors.New("service: bad approval request")

// Transactor 开事务执行 fn（fn 返回错误即回滚），repository.Transactor 实现。
type Transactor interface {
	InTx(ctx context.Context, fn func(tx *repository.DB) error) error
}

// ApprovalStore 是票据与约束校验读写，repository.ApprovalRepo 实现；*Tx 方法只在调用方事务内使用。
type ApprovalStore interface {
	TicketByOrderSeq(ctx context.Context, db *repository.DB, ref model.OrderRef, seq int) (model.ApprovalTicket, bool, error)
	TicketByRun(ctx context.Context, db *repository.DB, scope model.Scope, runID uint64) (model.ApprovalTicket, bool, error)
	InsertChecksTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, attempt int, checks []model.ConstraintCheck, now time.Time) error
	InsertTicketTx(ctx context.Context, tx *repository.DB, t model.ApprovalTicket, now time.Time) (uint64, bool, error)
	ActivateTx(ctx context.Context, tx *repository.DB, scope model.Scope, runID uint64, expiresAt, now time.Time) (int64, error)
	ListOpen(ctx context.Context, db *repository.DB, scope model.Scope) ([]repository.PendingTicket, error)
}

// RiskPolicy 按作业类型定风险，*policy.RiskMatrix 实现。
type RiskPolicy interface {
	For(wt model.WorkType) (policy.Risk, error)
	TTL(level model.RiskLevel) (time.Duration, error)
}

// ApprovalDeps 是 ApprovalService 的依赖（均必填）。
type ApprovalDeps struct {
	Clock      clock.Clock
	DB         *repository.DB // 事务外的读
	Tx         Transactor
	Tickets    ApprovalStore
	Orders     WorkOrderStore
	WorkOrders *WorkOrderService
	Events     EventStore
	Raw        agent.RawStore
	Risk       RiskPolicy
}

// ApprovalService 是审批闸门（agent.ApprovalGate，P0 §6.6 流程 4-5）与待审批列表。
type ApprovalService struct{ d ApprovalDeps }

var _ agent.ApprovalGate = (*ApprovalService)(nil)

// NewApprovalService 校验依赖并构造。
func NewApprovalService(d ApprovalDeps) (*ApprovalService, error) {
	var missing []string
	for name, isNil := range map[string]bool{"Clock": d.Clock == nil, "DB": d.DB == nil, "Tx": d.Tx == nil,
		"Tickets": d.Tickets == nil, "Orders": d.Orders == nil, "WorkOrders": d.WorkOrders == nil,
		"Events": d.Events == nil, "Raw": d.Raw == nil, "Risk": d.Risk == nil} {
		if isNil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("service: approval deps missing %v", missing)
	}
	return &ApprovalService{d: d}, nil
}

// approvalSnapshot 是审批快照（MinIO 对象本体，决策时比对 digest）：审批人看到的作业单、建议卡与风险定级。
type approvalSnapshot struct {
	TicketSeq       int              `json:"ticket_seq"`
	RiskLevel       model.RiskLevel  `json:"risk_level"`
	RequiredRole    []model.Role     `json:"required_role"`
	ConfirmRequired bool             `json:"confirm_required"`
	InitiatorID     uint64           `json:"initiator_id"`
	Order           model.WorkOrder  `json:"order"`
	Card            model.AdviceCard `json:"card"`
}

// openPayload / pendingPayload 是 APPROVAL_OPEN / APPROVAL_PENDING 事件的 payload（只含控制字段）。
type openPayload struct {
	TicketID        uint64          `json:"ticket_id"`
	OrderID         uint64          `json:"order_id"`
	TicketSeq       int             `json:"ticket_seq"`
	RiskLevel       model.RiskLevel `json:"risk_level"`
	ConfirmRequired bool            `json:"confirm_required"`
}

type pendingPayload struct {
	TicketID  uint64    `json:"ticket_id"`
	OrderID   uint64    `json:"order_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func validateOpen(scope model.Scope, req agent.ApprovalRequest) error {
	o := req.Order
	var problems []string
	if o.OrderID == 0 || o.RunID == 0 || req.InitiatorID == 0 {
		problems = append(problems, "order_id / run_id / initiator_id required")
	}
	if o.TenantID != scope.TenantID || o.FarmID != scope.FarmID {
		problems = append(problems, fmt.Sprintf("order (%d,%d) vs scope (%d,%d)", o.TenantID, o.FarmID, scope.TenantID, scope.FarmID))
	}
	if req.Card.AdviceID != o.AdviceID || req.Card.RunID != o.RunID {
		problems = append(problems, "card does not belong to order")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %v", ErrBadApproval, problems)
	}
	return nil
}

// snapshotDigestPrefix 是快照对象名中摘要前缀的长度：同一张票不同内容的快照落到不同对象，
// 并发开票不会互相覆盖；内容相同的重跑仍写同一对象。
const snapshotDigestPrefix = 16

// Open 为已落库的 DRAFT 单开 PREPARING 票（§6.6 流程 4）：已有票直接返回（整图重跑）；否则读库中作业单
// （run_id 须与请求一致），按其 work_type 查 risk_matrix 定级；快照先 RawStore.Put（key 含 order_id、ticket_seq
// 与快照摘要前缀，内容相同的重跑写同一对象，并发开票内容不同则各写各的）并算 sha256，再单事务写
// constraint_check(PRE_APPROVAL) + approval_ticket + Transition(DRAFT → PENDING_APPROVAL，已是则跳过) + APPROVAL_OPEN。
// 事务内命中 uk_order_seq（并发开票先提交）时返回库中已有的票。
func (s *ApprovalService) Open(ctx context.Context, scope model.Scope, req agent.ApprovalRequest) (model.ApprovalTicket, error) {
	if err := validateOpen(scope, req); err != nil {
		return model.ApprovalTicket{}, err
	}
	o := req.Order
	ref := model.OrderRef{TenantID: scope.TenantID, FarmID: scope.FarmID, OrderID: o.OrderID}
	if t, ok, err := s.existingTicket(ctx, ref); err != nil || ok {
		return t, err
	}
	cur, err := s.orderForTicket(ctx, ref, o)
	if err != nil {
		return model.ApprovalTicket{}, err
	}
	risk, err := s.d.Risk.For(cur.WorkType)
	if err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("service: open ticket order %d: %w", o.OrderID, err)
	}
	now := s.d.Clock.Now()
	t := model.ApprovalTicket{RunID: cur.RunID, TenantID: scope.TenantID, FarmID: scope.FarmID, OrderID: o.OrderID,
		TicketKind: model.TicketKindOrder, TicketSeq: firstTicketSeq, RiskLevel: risk.Level, RequiredRole: risk.Roles,
		ConfirmRequired: risk.ConfirmRequired, InitiatorID: req.InitiatorID, Status: model.TicketPreparing}
	if t.Snapshot, err = s.putSnapshot(ctx, t, req, now); err != nil {
		return model.ApprovalTicket{}, err
	}
	created := false
	err = s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		if err := s.d.Tickets.InsertChecksTx(ctx, tx, ref, firstTicketSeq, o.ConstraintChecks, now); err != nil {
			return err
		}
		id, ok, err := s.d.Tickets.InsertTicketTx(ctx, tx, t, now)
		if err != nil {
			return err
		}
		t.TicketID, created = id, ok
		if !created { // 并发开票已在其事务内写过迁移与事件
			return nil
		}
		return s.openTx(ctx, tx, ref, cur, t, now)
	})
	if err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("service: open ticket order %d: %w", o.OrderID, err)
	}
	if !created {
		return s.ticketAfterRace(ctx, ref)
	}
	return t, nil
}

// existingTicket 读作业单第一张票（事务外，读已提交的行）。
func (s *ApprovalService) existingTicket(ctx context.Context, ref model.OrderRef) (model.ApprovalTicket, bool, error) {
	t, ok, err := s.d.Tickets.TicketByOrderSeq(ctx, s.d.DB, ref, firstTicketSeq)
	if err != nil {
		return model.ApprovalTicket{}, false, fmt.Errorf("service: open ticket order %d: %w", ref.OrderID, err)
	}
	return t, ok, nil
}

// ticketAfterRace 在事务内命中 uk_order_seq 后读回库中已有的票（快照与定级以先提交者为准）。
func (s *ApprovalService) ticketAfterRace(ctx context.Context, ref model.OrderRef) (model.ApprovalTicket, error) {
	t, ok, err := s.existingTicket(ctx, ref)
	if err != nil {
		return model.ApprovalTicket{}, err
	}
	if !ok {
		return model.ApprovalTicket{}, fmt.Errorf("service: open ticket order %d: duplicate ticket not found: %w", ref.OrderID, model.ErrNotFound)
	}
	return t, nil
}

// orderForTicket 读库中作业单：定级用库中 work_type，并校验 run_id / work_type 与请求一致（快照展示的是请求中的单）。
func (s *ApprovalService) orderForTicket(ctx context.Context, ref model.OrderRef, o model.WorkOrder) (model.OrderState, error) {
	cur, err := s.d.Orders.GetState(ctx, s.d.DB, ref)
	if err != nil {
		return model.OrderState{}, fmt.Errorf("service: open ticket order %d: %w", ref.OrderID, err)
	}
	if cur.RunID != o.RunID || cur.WorkType != o.WorkType {
		return model.OrderState{}, fmt.Errorf("%w: order %d is run %d / %s in db, request says run %d / %s",
			ErrBadApproval, ref.OrderID, cur.RunID, cur.WorkType, o.RunID, o.WorkType)
	}
	return cur, nil
}

// putSnapshot 写审批快照对象并返回 digest 与 raw_object id。
func (s *ApprovalService) putSnapshot(ctx context.Context, t model.ApprovalTicket, req agent.ApprovalRequest, now time.Time) (model.TicketSnapshot, error) {
	body, err := json.Marshal(approvalSnapshot{TicketSeq: t.TicketSeq, RiskLevel: t.RiskLevel, RequiredRole: t.RequiredRole,
		ConfirmRequired: t.ConfirmRequired, InitiatorID: t.InitiatorID, Order: req.Order, Card: req.Card})
	if err != nil {
		return model.TicketSnapshot{}, fmt.Errorf("service: encode approval snapshot: %w", err)
	}
	digest := repository.Digest(body)
	obj, err := s.d.Raw.Put(ctx, model.RawPut{TenantID: t.TenantID, FarmID: t.FarmID, RunID: t.RunID,
		Kind: model.RawSnapshot, Name: snapshotName(t, digest), At: now, Body: body, ContentType: snapshotContentType})
	if err != nil {
		return model.TicketSnapshot{}, fmt.Errorf("service: put approval snapshot order %d: %w", t.OrderID, err)
	}
	return model.TicketSnapshot{Digest: digest, RawObjectID: obj.ID}, nil
}

// snapshotName 是快照对象名 ticket-{order_id}-{ticket_seq}-{digest 前 16 位}。
func snapshotName(t model.ApprovalTicket, digest string) string {
	return fmt.Sprintf("ticket-%d-%d-%s", t.OrderID, t.TicketSeq, digest[:snapshotDigestPrefix])
}

// openTx 是新票的后两写：DRAFT → PENDING_APPROVAL（开票前读到的已是 PENDING_APPROVAL 则跳过）与 APPROVAL_OPEN。
// 迁移由 Transition 重读状态并按 row_ver 守卫；cur 只用于判断是否跳过：能走到这里说明本事务插入了新票，
// 别的开票者不可能已在其事务内迁移过这张单。
func (s *ApprovalService) openTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, cur model.OrderState, t model.ApprovalTicket, now time.Time) error {
	if cur.Status != model.OrderPendingApproval {
		if err := s.d.WorkOrders.Transition(ctx, tx, ref, model.OrderPendingApproval, model.Actor{Type: model.ActorAgent}, ""); err != nil {
			return err
		}
	}
	return s.appendEvent(ctx, tx, t, model.EventApprovalOpen, openPayload{TicketID: t.TicketID, OrderID: t.OrderID,
		TicketSeq: t.TicketSeq, RiskLevel: t.RiskLevel, ConfirmRequired: t.ConfirmRequired}, now)
}

// errNotActivated 是 ActivateTx 未命中行（票已不是 PREPARING 或 run 未中断），用于回滚后重读判定。
var errNotActivated = errors.New("activate matched no row")

// Activate 在 run 已 INTERRUPTED(WAIT_APPROVAL) 后开放该 run 的票（§6.6 流程 5）：OPEN 票原样返回（幂等）；
// PREPARING 票单事务 UPDATE JOIN agent_run 守卫置 OPEN、写 expires_at（risk_matrix 的 approval_ttl）+ APPROVAL_PENDING；
// UPDATE 命中 0 行时重读：已被并发的 Activate（如 ticket_activate 扫描）置 OPEN 则返回库中的票，否则
// （无票、票已决或 run 未中断）返回 agent.ErrNotActivatable。
func (s *ApprovalService) Activate(ctx context.Context, scope model.Scope, runID uint64) (model.ApprovalTicket, error) {
	if runID == 0 {
		return model.ApprovalTicket{}, fmt.Errorf("%w: run_id required", ErrBadApproval)
	}
	t, err := s.activatable(ctx, scope, runID)
	if err != nil || t.Status == model.TicketOpen {
		return t, err
	}
	ttl, err := s.d.Risk.TTL(t.RiskLevel)
	if err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("service: activate ticket %d: %w", t.TicketID, err)
	}
	now := s.d.Clock.Now()
	expires := now.Add(ttl)
	err = s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		n, err := s.d.Tickets.ActivateTx(ctx, tx, scope, runID, expires, now)
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotActivated
		}
		return s.appendEvent(ctx, tx, t, model.EventApprovalPending,
			pendingPayload{TicketID: t.TicketID, OrderID: t.OrderID, ExpiresAt: expires}, now)
	})
	switch {
	case errors.Is(err, errNotActivated):
		again, err := s.activatable(ctx, scope, runID)
		if err != nil {
			return model.ApprovalTicket{}, err
		}
		if again.Status == model.TicketOpen {
			return again, nil
		}
		return model.ApprovalTicket{}, fmt.Errorf("service: run %d not INTERRUPTED(WAIT_APPROVAL), ticket %d still %s: %w",
			runID, again.TicketID, again.Status, agent.ErrNotActivatable)
	case err != nil:
		return model.ApprovalTicket{}, fmt.Errorf("service: activate ticket %d: %w", t.TicketID, err)
	}
	t.Status, t.ExpiresAt = model.TicketOpen, &expires
	return t, nil
}

// activatable 读该 run 的票：OPEN 或 PREPARING 原样返回；无票或票已决返回 agent.ErrNotActivatable。
func (s *ApprovalService) activatable(ctx context.Context, scope model.Scope, runID uint64) (model.ApprovalTicket, error) {
	t, ok, err := s.d.Tickets.TicketByRun(ctx, s.d.DB, scope, runID)
	if err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("service: activate run %d: %w", runID, err)
	}
	switch {
	case !ok:
		return model.ApprovalTicket{}, fmt.Errorf("service: run %d has no ticket: %w", runID, agent.ErrNotActivatable)
	case t.Status != model.TicketOpen && t.Status != model.TicketPreparing:
		return model.ApprovalTicket{}, fmt.Errorf("service: run %d ticket %d is %s: %w", runID, t.TicketID, t.Status, agent.ErrNotActivatable)
	}
	return t, nil
}

func (s *ApprovalService) appendEvent(ctx context.Context, tx *repository.DB, t model.ApprovalTicket, typ model.EventType, payload any, now time.Time) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", typ, err)
	}
	if _, err := s.d.Events.AppendTx(ctx, tx, model.RunEvent{TenantID: t.TenantID, FarmID: t.FarmID, RunID: t.RunID,
		Type: typ, Actor: model.ActorAgent, Payload: b, CreatedAt: now}); err != nil {
		return fmt.Errorf("append %s: %w", typ, err)
	}
	return nil
}

// ListPending 是农场待审批列表（OPEN 票，最多 50 条；PREPARING 票不可见）。
func (s *ApprovalService) ListPending(ctx context.Context, scope model.Scope) ([]repository.PendingTicket, error) {
	out, err := s.d.Tickets.ListOpen(ctx, s.d.DB, scope)
	if err != nil {
		return nil, fmt.Errorf("service: list pending: %w", err)
	}
	return out, nil
}
