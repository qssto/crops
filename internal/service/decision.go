package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// 审批决策（P0 §6.7 流程 2）的拒绝原因；controller 以 Error() 作为响应错误码。
var (
	ErrTicketClosed     = errors.New("TICKET_CLOSED")
	ErrTicketExpired    = errors.New("TICKET_EXPIRED")
	ErrSnapshotStale    = errors.New("SNAPSHOT_STALE")
	ErrRoleInsufficient = errors.New("ROLE_INSUFFICIENT")
	ErrSameApprover     = errors.New("SAME_APPROVER")
	ErrRequestInFlight  = errors.New("REQUEST_IN_FLIGHT")
	// ErrRequestConflict 表示 X-Request-Id 已被另一笔决策（其他农场 / 票据 / 审批人 / 决策取值）使用。
	ErrRequestConflict = errors.New("REQUEST_ID_CONFLICT")
	// ErrOrderBusy 表示 lock:order 被他人持有（同一作业单的并发决策 / 扫描），可稍后重试。
	ErrOrderBusy = errors.New("ORDER_BUSY")
)

// 审批决策取值：请求只接受 APPROVE / REJECT；CONFIRM 是 HIGH 票第二审批人的落库取值。
const (
	DecisionApprove = "APPROVE"
	DecisionReject  = "REJECT"
	decisionConfirm = "CONFIRM"
)

const (
	idemPendingTTL = 10 * time.Second
	idemDoneTTL    = 24 * time.Hour
	orderLockTTL   = 10 * time.Second
	// resumeTimeout 是决策生效后异步 Resume 的兜底超时；失败由 run_finalize 扫描补偿。
	resumeTimeout = 2 * time.Minute
	// DefaultDispatchWindow 是审批生效到 dispatch_deadline_at 的默认时长。
	DefaultDispatchWindow = 2 * time.Hour
	maxCommentRunes       = 512
	digestLen             = 64
	maxRequestIDLen       = 64
)

// DecisionStore 是决策与过期需要的仓储能力，repository.DecisionRepo 实现。
type DecisionStore interface {
	InTx(ctx context.Context, fn func(tx *repository.DB) error) error
	TicketTx(ctx context.Context, db *repository.DB, scope model.Scope, ticketID uint64) (repository.TicketRow, error)
	DecisionByRequest(ctx context.Context, requestID string) (repository.DecisionRow, bool, error)
	InsertDecisionTx(ctx context.Context, tx *repository.DB, d repository.DecisionRow) (uint64, error)
	DecideTicketTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, to model.TicketStatus, operatorID uint64, now time.Time) error
	SetFirstApproverTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, userID uint64, now time.Time) error
	ExpireTicketTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, now time.Time) (bool, error)
	SetDispatchDeadlineTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, deadline, now time.Time) error
	InsertDispatchOutboxTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, payload []byte, now time.Time) error
	CloseRunTx(ctx context.Context, tx *repository.DB, scope model.Scope, runID uint64, reason model.ReasonCode, now time.Time) error
	InsertRejectedMemoryTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, summary string, now time.Time) error
	AuditDenied(ctx context.Context, scope model.Scope, t repository.TicketRow, code string) error
	ListPending(ctx context.Context, scope model.Scope) ([]repository.PendingRow, error)
}

// OrderTransitioner 是作业单唯一迁移入口，WorkOrderService 实现。
type OrderTransitioner interface {
	Transition(ctx context.Context, tx *repository.DB, ref model.OrderRef, to model.WorkOrderStatus,
		actor model.Actor, reason model.ReasonCode) error
	TransitionFrom(ctx context.Context, tx *repository.DB, ref model.OrderRef, from, to model.WorkOrderStatus,
		actor model.Actor, reason model.ReasonCode) error
}

// IdemStore 是 idem:approve 去重占位（比较后改写 / 删除须原子），repository.RedisIdem 实现。
type IdemStore interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, bool, error)
	Replace(ctx context.Context, key, expect, value string, ttl time.Duration) (bool, error)
	DeleteIf(ctx context.Context, key, expect string) (bool, error)
}

// KeyLocker 取 Redis token 锁（键已带 as:{env}: 前缀）；被持有返回 repository.ErrLockHeld。repository.Locker 实现。
type KeyLocker interface {
	AcquireLease(ctx context.Context, key string, ttl time.Duration) (agent.Lease, error)
}

// PendingTicket 是待审批列表的一行。
type PendingTicket = repository.PendingRow

// DecideInput 是一次审批决策；ticket_id 与 farm_id 来自路径，request_id 来自 X-Request-Id。
type DecideInput struct {
	TicketID       uint64
	RequestID      string
	Decision       string // APPROVE / REJECT
	SnapshotDigest string
	Comment        string
}

// DecideResult 是决策结果；HIGH 票第一审批人通过时 AwaitingConfirm=true、票据仍 OPEN。
type DecideResult struct {
	DecisionID      uint64             `json:"decision_id"`
	TicketID        uint64             `json:"ticket_id"`
	Status          model.TicketStatus `json:"status"`
	AwaitingConfirm bool               `json:"awaiting_confirm"`
}

// DecisionDeps 是 DecisionService 的依赖；Resumer 为空时不异步续跑（由 run_finalize 扫描补偿）。
type DecisionDeps struct {
	Store          DecisionStore
	Orders         OrderTransitioner
	Events         EventStore
	Idem           IdemStore
	Locks          KeyLocker
	Keys           rediskey.Keys
	Resumer        agent.Resumer
	Clock          clock.Clock
	Logger         *slog.Logger
	DispatchWindow time.Duration // 0 取 DefaultDispatchWindow
}

// DecisionService 是审批决策入口与票据过期收尾（作业单迁移一律经 OrderTransitioner）。
type DecisionService struct {
	d  DecisionDeps
	wg sync.WaitGroup
}

// NewDecisionService 构造 DecisionService。
func NewDecisionService(d DecisionDeps) *DecisionService {
	if d.DispatchWindow <= 0 {
		d.DispatchWindow = DefaultDispatchWindow
	}
	return &DecisionService{d: d}
}

// Pending 返回本农场 OPEN 的待审批票据（P0 §6.6 列表 SQL）。
func (s *DecisionService) Pending(ctx context.Context, scope model.Scope) ([]PendingTicket, error) {
	rows, err := s.d.Store.ListPending(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("service: pending approvals: %w", err)
	}
	return rows, nil
}

// Wait 等待已启动的异步 Resume 结束（优雅退出与测试用）。
func (s *DecisionService) Wait() { s.wg.Wait() }

func validateDecide(in DecideInput) error {
	switch {
	case in.TicketID == 0:
		return fmt.Errorf("%w: ticket_id required", ErrBadInput)
	case in.RequestID == "" || len(in.RequestID) > maxRequestIDLen:
		return fmt.Errorf("%w: X-Request-Id required (1-%d chars)", ErrBadInput, maxRequestIDLen)
	case in.Decision != DecisionApprove && in.Decision != DecisionReject:
		return fmt.Errorf("%w: decision must be APPROVE or REJECT", ErrBadInput)
	case len(in.SnapshotDigest) != digestLen:
		return fmt.Errorf("%w: snapshot_digest must be %d hex chars", ErrBadInput, digestLen)
	case utf8.RuneCountInString(in.Comment) > maxCommentRunes:
		return fmt.Errorf("%w: comment over %d chars", ErrBadInput, maxCommentRunes)
	}
	return nil
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("service: idem token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Decide 执行一次审批决策：idem:approve 去重 → lock:order → 校验（OPEN → 未过期 → digest → 角色 / 非发起人 → 非同一审批人）
// → 生效 / 驳回 / 记第一审批人（各自单事务）。提交后键改写 DONE:{decision_id}；失败按 token 删除。
func (s *DecisionService) Decide(ctx context.Context, scope model.Scope, in DecideInput) (DecideResult, error) {
	if err := validateDecide(in); err != nil {
		return DecideResult{}, err
	}
	key := s.d.Keys.IdemApprove(in.RequestID)
	token, err := newToken()
	if err != nil {
		return DecideResult{}, err
	}
	pending := "PENDING:" + token
	won, err := s.d.Idem.SetNX(ctx, key, pending, idemPendingTTL)
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: decide: %w", err)
	}
	if !won {
		v, found, err := s.d.Idem.Get(ctx, key)
		if err != nil {
			return DecideResult{}, fmt.Errorf("service: decide: %w", err)
		}
		if found && strings.HasPrefix(v, "PENDING:") {
			return DecideResult{}, ErrRequestInFlight
		}
		return s.replay(ctx, scope, in) // DONE 命中或占位已过期：以 uk_request 为准
	}
	res, err := s.decideOnce(ctx, scope, in)
	if err != nil {
		if _, delErr := s.d.Idem.DeleteIf(context.WithoutCancel(ctx), key, pending); delErr != nil {
			err = errors.Join(err, delErr)
		}
		return DecideResult{}, err
	}
	if _, err := s.d.Idem.Replace(context.WithoutCancel(ctx), key, pending, fmt.Sprintf("DONE:%d", res.DecisionID), idemDoneTTL); err != nil {
		// 决策已提交；DONE 键写失败时由 uk_request 兜底去重，只告警
		s.d.Logger.WarnContext(ctx, "decide: mark idem done", "request_id", in.RequestID, "err", err)
	}
	return res, nil
}

// decideOnce 先查 uk_request（占位丢失后的重放），再执行决策。
func (s *DecisionService) decideOnce(ctx context.Context, scope model.Scope, in DecideInput) (DecideResult, error) {
	if _, found, err := s.d.Store.DecisionByRequest(ctx, in.RequestID); err != nil {
		return DecideResult{}, fmt.Errorf("service: decide: %w", err)
	} else if found {
		return s.replay(ctx, scope, in)
	}
	return s.decide(ctx, scope, in)
}

// replay 按 uk_request 返回首次决策的结果；查不到（占位刚过期、决策未落库）按在途处理，客户端重试。
// 已落库决策与本次请求的农场 / 票据 / 审批人 / 决策取值不一致时返回 ErrRequestConflict，不静默回放首次结果。
func (s *DecisionService) replay(ctx context.Context, scope model.Scope, in DecideInput) (DecideResult, error) {
	d, found, err := s.d.Store.DecisionByRequest(ctx, in.RequestID)
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: replay decision: %w", err)
	}
	if !found {
		return DecideResult{}, ErrRequestInFlight
	}
	if !sameRequest(d, scope, in) {
		return DecideResult{}, ErrRequestConflict
	}
	t, err := s.d.Store.TicketTx(ctx, nil, scope, d.TicketID)
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: replay decision: %w", err)
	}
	st := model.TicketStatus(t.Status)
	return DecideResult{DecisionID: d.ID, TicketID: t.ID, Status: st,
		AwaitingConfirm: d.Decision == DecisionApprove && needsConfirm(t) && st == model.TicketOpen}, nil
}

// sameRequest 判断已落库决策是否就是本次请求的重放；APPROVE 请求在 HIGH 票第二人处落库为 CONFIRM。
func sameRequest(d repository.DecisionRow, scope model.Scope, in DecideInput) bool {
	decision := d.Decision
	if decision == decisionConfirm {
		decision = DecisionApprove
	}
	return d.TenantID == scope.TenantID && d.FarmID == scope.FarmID && d.TicketID == in.TicketID &&
		d.OperatorID == scope.UserID && decision == in.Decision
}

// lockOrder 取 lock:order:{order_id}；被持有返回 ErrOrderBusy。
func (s *DecisionService) lockOrder(ctx context.Context, orderID uint64) (func(), error) {
	return acquireOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, orderID)
}

// acquireOrderLock 取 lock:order:{order_id}（审批决策与扫描共用）；被持有返回 ErrOrderBusy。返回的 release 只告警不报错。
func acquireOrderLock(ctx context.Context, locks KeyLocker, keys rediskey.Keys, log *slog.Logger, orderID uint64) (func(), error) {
	lease, err := locks.AcquireLease(ctx, keys.LockOrder(orderID), orderLockTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		return nil, fmt.Errorf("service: order %d: %w", orderID, ErrOrderBusy)
	}
	if err != nil {
		return nil, fmt.Errorf("service: lock order %d: %w", orderID, err)
	}
	return func() {
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "release order lock", "order_id", orderID, "err", err)
		}
	}, nil
}

// decide 持 lock:order 校验并执行决策。
func (s *DecisionService) decide(ctx context.Context, scope model.Scope, in DecideInput) (DecideResult, error) {
	t, err := s.d.Store.TicketTx(ctx, nil, scope, in.TicketID)
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: decide: %w", err)
	}
	release, err := s.lockOrder(ctx, t.OrderID)
	if err != nil {
		return DecideResult{}, err
	}
	defer release()
	if t, err = s.d.Store.TicketTx(ctx, nil, scope, in.TicketID); err != nil { // 持锁后重读，以锁内状态为准
		return DecideResult{}, fmt.Errorf("service: decide: %w", err)
	}
	role, err := s.check(ctx, scope, t, in)
	if err != nil {
		return DecideResult{}, s.deny(ctx, scope, t, err)
	}
	now := s.d.Clock.Now()
	d := repository.DecisionRow{TenantID: scope.TenantID, FarmID: scope.FarmID, TicketID: t.ID, RequestID: in.RequestID,
		OperatorID: scope.UserID, OperatorRole: string(role), SnapshotDigest: in.SnapshotDigest, CreatedAt: now}
	if in.Comment != "" {
		d.Comment = &in.Comment
	}
	var res DecideResult
	switch {
	case in.Decision == DecisionReject:
		d.Decision = DecisionReject
		res, err = s.reject(ctx, scope, t, d, now)
	case needsConfirm(t) && t.FirstApproverID == nil:
		d.Decision = DecisionApprove
		res, err = s.recordFirst(ctx, scope, t, d, now)
	default:
		d.Decision = DecisionApprove
		if needsConfirm(t) {
			d.Decision = decisionConfirm
		}
		res, err = s.approve(ctx, scope, t, d, now)
	}
	if errors.Is(err, model.ErrConcurrentUpdate) {
		return DecideResult{}, s.explainConflict(ctx, scope, t.ID, in, err)
	}
	return res, err
}

// needsConfirm：HIGH 票（confirm_required=1）须二人确认。
func needsConfirm(t repository.TicketRow) bool {
	return t.ConfirmRequired != 0 || model.RiskLevel(t.RiskLevel) == model.RiskHigh
}

// check 按 §6.7 流程 2 顺序校验，返回审批人用于本票的角色。过期时同事务收尾后返回 ErrTicketExpired。
func (s *DecisionService) check(ctx context.Context, scope model.Scope, t repository.TicketRow, in DecideInput) (model.Role, error) {
	if model.TicketStatus(t.Status) != model.TicketOpen {
		return "", ErrTicketClosed
	}
	if t.ExpiresAt == nil || !t.ExpiresAt.After(s.d.Clock.Now()) {
		if _, err := s.expireLocked(ctx, t, model.Actor{Type: model.ActorUser, ID: scope.UserID}); err != nil {
			// 客户端按 TICKET_EXPIRED 处理；收尾失败单独记错，票据由 approval_expire 扫描兜底
			s.d.Logger.ErrorContext(ctx, "decide: expire ticket", "ticket_id", t.ID, "err", err)
			return "", errors.Join(ErrTicketExpired, err)
		}
		return "", ErrTicketExpired
	}
	if in.SnapshotDigest != t.SnapshotDigest {
		return "", ErrSnapshotStale
	}
	roles, err := t.Roles()
	if err != nil {
		return "", err
	}
	idx := slices.IndexFunc(scope.Roles, func(r model.Role) bool { return slices.Contains(roles, r) })
	if idx < 0 || scope.UserID == t.InitiatorID {
		return "", ErrRoleInsufficient
	}
	if in.Decision == DecisionApprove && t.FirstApproverID != nil && *t.FirstApproverID == scope.UserID {
		return "", ErrSameApprover
	}
	return scope.Roles[idx], nil
}

// deny 对业务拒绝写 audit_log(APPROVAL, DENIED)；审计写失败与拒绝原因一并返回。
func (s *DecisionService) deny(ctx context.Context, scope model.Scope, t repository.TicketRow, reason error) error {
	code := ""
	for _, e := range []error{ErrTicketClosed, ErrTicketExpired, ErrSnapshotStale, ErrRoleInsufficient, ErrSameApprover} {
		if errors.Is(reason, e) {
			code = e.Error()
			break
		}
	}
	if code == "" {
		return fmt.Errorf("service: decide ticket %d: %w", t.ID, reason)
	}
	if err := s.d.Store.AuditDenied(context.WithoutCancel(ctx), scope, t, code); err != nil {
		return errors.Join(reason, err)
	}
	return reason
}

// explainConflict 在守卫 UPDATE 影响 0 行后重读票据，按 TICKET_CLOSED / TICKET_EXPIRED / SNAPSHOT_STALE 返回。
func (s *DecisionService) explainConflict(ctx context.Context, scope model.Scope, ticketID uint64, in DecideInput, cause error) error {
	t, err := s.d.Store.TicketTx(ctx, nil, scope, ticketID)
	if err != nil {
		return errors.Join(cause, err)
	}
	switch {
	case model.TicketStatus(t.Status) != model.TicketOpen:
		return s.deny(ctx, scope, t, ErrTicketClosed)
	case t.ExpiresAt == nil || !t.ExpiresAt.After(s.d.Clock.Now()):
		return s.deny(ctx, scope, t, ErrTicketExpired) // 收尾交给 approval_expire 扫描
	case t.SnapshotDigest != in.SnapshotDigest:
		return s.deny(ctx, scope, t, ErrSnapshotStale)
	}
	return fmt.Errorf("service: decide ticket %d: %w", ticketID, cause)
}

// decidedEvent 同事务追加 APPROVAL_DECIDED（payload 只含控制字段）；actor 带 ID，审计投影据此记操作人。
func (s *DecisionService) decidedEvent(ctx context.Context, tx *repository.DB, t repository.TicketRow,
	actor model.Actor, payload map[string]any, now time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("service: encode APPROVAL_DECIDED: %w", err)
	}
	if _, err := s.d.Events.AppendTx(ctx, tx, model.RunEvent{TenantID: t.TenantID, FarmID: t.FarmID, RunID: t.RunID,
		Type: model.EventApprovalDecided, Actor: actor.Type, ActorID: actor.ID, Payload: body, CreatedAt: now}); err != nil {
		return fmt.Errorf("service: append APPROVAL_DECIDED ticket %d: %w", t.ID, err)
	}
	return nil
}

// recordFirst 记 HIGH 票第一审批人（单事务：approval_decision(APPROVE) + first_approver_id），票据仍 OPEN。
func (s *DecisionService) recordFirst(ctx context.Context, scope model.Scope, t repository.TicketRow,
	d repository.DecisionRow, now time.Time) (DecideResult, error) {
	var id uint64
	err := s.d.Store.InTx(ctx, func(tx *repository.DB) error {
		var err error
		if id, err = s.d.Store.InsertDecisionTx(ctx, tx, d); err != nil {
			return err
		}
		return s.d.Store.SetFirstApproverTx(ctx, tx, t, scope.UserID, now)
	})
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: first approval ticket %d: %w", t.ID, err)
	}
	return DecideResult{DecisionID: id, TicketID: t.ID, Status: model.TicketOpen, AwaitingConfirm: true}, nil
}

// approve 生效（单事务）：approval_decision → 票据 APPROVED → Transition(→ APPROVED) + dispatch_deadline_at
// → outbox(order.dispatch) → APPROVAL_DECIDED；提交后异步 Resume。
func (s *DecisionService) approve(ctx context.Context, scope model.Scope, t repository.TicketRow,
	d repository.DecisionRow, now time.Time) (DecideResult, error) {
	actor := model.Actor{Type: model.ActorUser, ID: scope.UserID}
	var id uint64
	err := s.d.Store.InTx(ctx, func(tx *repository.DB) error {
		var err error
		if id, err = s.d.Store.InsertDecisionTx(ctx, tx, d); err != nil {
			return err
		}
		if err := s.d.Store.DecideTicketTx(ctx, tx, t, model.TicketApproved, scope.UserID, now); err != nil {
			return err
		}
		if err := s.d.Orders.Transition(ctx, tx, t.OrderRef(), model.OrderApproved, actor, ""); err != nil {
			return err
		}
		if err := s.d.Store.SetDispatchDeadlineTx(ctx, tx, t.OrderRef(), now.Add(s.d.DispatchWindow), now); err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"order_id": t.OrderID, "ticket_id": t.ID, "run_id": t.RunID})
		if err != nil {
			return fmt.Errorf("service: encode outbox payload: %w", err)
		}
		if err := s.d.Store.InsertDispatchOutboxTx(ctx, tx, t, payload, now); err != nil {
			return err
		}
		return s.decidedEvent(ctx, tx, t, actor, map[string]any{"ticket_id": t.ID, "order_id": t.OrderID,
			"decision": d.Decision, "decision_id": id, "operator_id": scope.UserID}, now)
	})
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: approve ticket %d: %w", t.ID, err)
	}
	s.resumeAsync(ctx, t)
	return DecideResult{DecisionID: id, TicketID: t.ID, Status: model.TicketApproved}, nil
}

// reject 驳回（单事务）：approval_decision → 票据 REJECTED → Transition(→ REJECTED) → run COMPLETED(APPROVAL_REJECTED)
// → episodic_memory(REJECTED) → APPROVAL_DECIDED。
func (s *DecisionService) reject(ctx context.Context, scope model.Scope, t repository.TicketRow,
	d repository.DecisionRow, now time.Time) (DecideResult, error) {
	actor := model.Actor{Type: model.ActorUser, ID: scope.UserID}
	var id uint64
	err := s.d.Store.InTx(ctx, func(tx *repository.DB) error {
		var err error
		if id, err = s.d.Store.InsertDecisionTx(ctx, tx, d); err != nil {
			return err
		}
		if err := s.d.Store.DecideTicketTx(ctx, tx, t, model.TicketRejected, scope.UserID, now); err != nil {
			return err
		}
		if err := s.d.Orders.Transition(ctx, tx, t.OrderRef(), model.OrderRejected, actor, model.ReasonApprovalRejected); err != nil {
			return err
		}
		if err := s.d.Store.CloseRunTx(ctx, tx, t.Scope(), t.RunID, model.ReasonApprovalRejected, now); err != nil {
			return err
		}
		summary := fmt.Sprintf("作业单 %d 审批驳回", t.OrderID)
		if d.Comment != nil {
			summary += "：" + *d.Comment
		}
		if err := s.d.Store.InsertRejectedMemoryTx(ctx, tx, t, truncateRunes(summary, maxCommentRunes), now); err != nil {
			return err
		}
		return s.decidedEvent(ctx, tx, t, actor, map[string]any{"ticket_id": t.ID, "order_id": t.OrderID,
			"decision": DecisionReject, "decision_id": id, "operator_id": scope.UserID}, now)
	})
	if err != nil {
		return DecideResult{}, fmt.Errorf("service: reject ticket %d: %w", t.ID, err)
	}
	return DecideResult{DecisionID: id, TicketID: t.ID, Status: model.TicketRejected}, nil
}

// truncateRunes 按字符截断。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ExpireTicket 持 lock:order 收尾一张已过 expires_at 的 OPEN 票（approval_expire 扫描调用）；票据已被决策时返回 false。
func (s *DecisionService) ExpireTicket(ctx context.Context, t repository.TicketRow) (bool, error) {
	release, err := s.lockOrder(ctx, t.OrderID)
	if err != nil {
		return false, err
	}
	defer release()
	return s.expireLocked(ctx, t, model.Actor{Type: model.ActorSystem})
}

// expireLocked 单事务：票据 OPEN → EXPIRED（row_ver 守卫，0 行即跳过）→ Transition(PENDING_APPROVAL → EXPIRED)
// → run COMPLETED(APPROVAL_EXPIRED) → APPROVAL_DECIDED(decision=EXPIRED)。调用方须持 lock:order。
func (s *DecisionService) expireLocked(ctx context.Context, t repository.TicketRow, actor model.Actor) (bool, error) {
	now := s.d.Clock.Now()
	expired := false
	err := s.d.Store.InTx(ctx, func(tx *repository.DB) error {
		ok, err := s.d.Store.ExpireTicketTx(ctx, tx, t, now)
		if err != nil || !ok {
			return err
		}
		if err := s.d.Orders.Transition(ctx, tx, t.OrderRef(), model.OrderExpired, actor, model.ReasonApprovalExpired); err != nil {
			return err
		}
		if err := s.d.Store.CloseRunTx(ctx, tx, t.Scope(), t.RunID, model.ReasonApprovalExpired, now); err != nil {
			return err
		}
		expired = true
		return s.decidedEvent(ctx, tx, t, actor, map[string]any{"ticket_id": t.ID, "order_id": t.OrderID,
			"decision": string(model.TicketExpired)}, now)
	})
	if err != nil {
		return false, fmt.Errorf("service: expire ticket %d: %w", t.ID, err)
	}
	return expired, nil
}

// resumeAsync 在生效事务提交后异步续跑 run（不阻塞决策响应）；失败只告警，由 run_finalize 扫描补偿。
func (s *DecisionService) resumeAsync(ctx context.Context, t repository.TicketRow) {
	if s.d.Resumer == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeTimeout)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		if _, err := s.d.Resumer.Resume(rctx, t.Scope(), t.RunID); err != nil {
			s.d.Logger.WarnContext(rctx, "decide: resume run", "run_id", t.RunID, "ticket_id", t.ID, "err", err)
		}
	}()
}
