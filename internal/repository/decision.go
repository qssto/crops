package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// DecisionRepo 是审批决策与 HITL 扫描的 SQL（P0 §6.7 定版 SQL 照抄；文档未给的另行标注）。
// 带 Tx 后缀的方法只在调用方事务内执行；读票据的 SQL 自写，不依赖 L5 的 ApprovalRepo。
type DecisionRepo struct {
	db    *gorm.DB
	clock clock.Clock
}

// NewDecisionRepo 构造 DecisionRepo。
func NewDecisionRepo(db *gorm.DB, c clock.Clock) *DecisionRepo {
	return &DecisionRepo{db: db, clock: c}
}

// InTx 在一个事务内执行 fn（service 以此划定事务边界，不 import gorm）；提交后执行 SSE 发布。
func (r *DecisionRepo) InTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return inTx(ctx, r.db, fn)
}

// TicketRow 是 approval_ticket 的读取列（含 row_ver）。
type TicketRow struct {
	ID              uint64
	TenantID        uint64
	FarmID          uint64
	RunID           uint64
	OrderID         uint64
	TicketSeq       int
	RiskLevel       string
	RequiredRole    string // json 数组
	ConfirmRequired int
	InitiatorID     uint64
	FirstApproverID *uint64
	SnapshotDigest  string
	SnapshotRawID   uint64
	Status          string
	OperatorID      *uint64
	ExpiresAt       *time.Time
	DecidedAt       *time.Time
	RowVer          int
}

// Scope 返回票据所在农场的系统 Scope。
func (t TicketRow) Scope() model.Scope { return model.Scope{TenantID: t.TenantID, FarmID: t.FarmID} }

// OrderRef 返回票据关联的作业单。
func (t TicketRow) OrderRef() model.OrderRef {
	return model.OrderRef{TenantID: t.TenantID, FarmID: t.FarmID, OrderID: t.OrderID}
}

// Roles 解析 required_role。
func (t TicketRow) Roles() ([]model.Role, error) {
	var roles []model.Role
	if err := json.Unmarshal([]byte(t.RequiredRole), &roles); err != nil {
		return nil, fmt.Errorf("repository: ticket %d required_role: %w", t.ID, err)
	}
	return roles, nil
}

// Ticket 转为对外契约 model.ApprovalTicket。
func (t TicketRow) Ticket() (model.ApprovalTicket, error) {
	roles, err := t.Roles()
	if err != nil {
		return model.ApprovalTicket{}, err
	}
	return model.ApprovalTicket{TicketID: t.ID, RunID: t.RunID, TenantID: t.TenantID, FarmID: t.FarmID, OrderID: t.OrderID,
		TicketKind: model.TicketKindOrder, TicketSeq: t.TicketSeq, RiskLevel: model.RiskLevel(t.RiskLevel), RequiredRole: roles,
		ConfirmRequired: t.ConfirmRequired != 0, InitiatorID: t.InitiatorID, FirstApprover: t.FirstApproverID,
		Snapshot: model.TicketSnapshot{Digest: t.SnapshotDigest, RawObjectID: t.SnapshotRawID},
		Status:   model.TicketStatus(t.Status), Operator: t.OperatorID, ExpiresAt: t.ExpiresAt, DecidedAt: t.DecidedAt}, nil
}

const decisionTicketColumns = `id, tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role, confirm_required, initiator_id, first_approver_id, snapshot_digest, snapshot_raw_id, status, operator_id, expires_at, decided_at, row_ver`

// TicketTx 读一张 ORDER 票（限定租户 / 农场）；db 可为事务或连接。
func (r *DecisionRepo) TicketTx(ctx context.Context, db *gorm.DB, scope model.Scope, ticketID uint64) (TicketRow, error) {
	if db == nil {
		db = r.db
	}
	var row TicketRow
	res := db.WithContext(ctx).Raw(
		`SELECT `+decisionTicketColumns+` FROM approval_ticket WHERE id=? AND tenant_id=? AND farm_id=? AND ticket_kind='ORDER'`,
		ticketID, scope.TenantID, scope.FarmID).Scan(&row)
	if res.Error != nil {
		return TicketRow{}, fmt.Errorf("repository: get approval_ticket %d: %w", ticketID, res.Error)
	}
	if res.RowsAffected == 0 {
		return TicketRow{}, fmt.Errorf("repository: approval_ticket %d: %w", ticketID, model.ErrNotFound)
	}
	return row, nil
}

// DecisionRow 是 approval_decision 行（只追加）。
type DecisionRow struct {
	ID             uint64
	TenantID       uint64
	FarmID         uint64
	TicketID       uint64
	RequestID      string
	Decision       string // APPROVE / CONFIRM / REJECT
	OperatorID     uint64
	OperatorRole   string
	SnapshotDigest string
	Comment        *string
	CreatedAt      time.Time
}

// DecisionByRequest 按 uk_request 查已落库的决策（Redis 去重键丢失时兜底）。uk_request 全局唯一（0004 DDL），
// 故不按租户过滤：调用方须比对返回行的归属与请求是否一致，不一致按 request_id 冲突处理、不得回放。
func (r *DecisionRepo) DecisionByRequest(ctx context.Context, requestID string) (DecisionRow, bool, error) {
	var row DecisionRow
	res := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, ticket_id, request_id, decision, operator_id, operator_role, snapshot_digest, comment, created_at FROM approval_decision WHERE request_id=?`,
		requestID).Scan(&row)
	if res.Error != nil {
		return DecisionRow{}, false, fmt.Errorf("repository: get approval_decision %s: %w", requestID, res.Error)
	}
	return row, res.RowsAffected > 0, nil
}

// InsertDecisionTx 追加一条 approval_decision，返回 id；request_id 重复由 uk_request 拒绝。
func (r *DecisionRepo) InsertDecisionTx(ctx context.Context, tx *gorm.DB, d DecisionRow) (uint64, error) {
	res, err := execInTx(ctx, tx,
		`INSERT INTO approval_decision (tenant_id, farm_id, ticket_id, request_id, decision, operator_id, operator_role, snapshot_digest, comment, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.TenantID, d.FarmID, d.TicketID, d.RequestID, d.Decision, d.OperatorID, d.OperatorRole, d.SnapshotDigest, d.Comment, d.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("repository: insert approval_decision %s: %w", d.RequestID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("repository: approval_decision id: %w", err)
	}
	return uint64(id), nil
}

// affected 把影响行数 0 统一转为 model.ErrConcurrentUpdate。
func affected(res *gorm.DB, what string) error {
	if res.Error != nil {
		return fmt.Errorf("repository: %s: %w", what, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repository: %s: %w", what, model.ErrConcurrentUpdate)
	}
	return nil
}

// DecideTicketTx 照抄 P0 §6.7 决策 UPDATE（过期 / 快照 / row_ver 守卫，与过期扫描互斥）；影响行数 0 → model.ErrConcurrentUpdate，
// 调用方重读票据按 TICKET_CLOSED / TICKET_EXPIRED 返回。
func (r *DecisionRepo) DecideTicketTx(ctx context.Context, tx *gorm.DB, t TicketRow, to model.TicketStatus,
	operatorID uint64, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket SET status=?, operator_id=?, decided_at=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN' AND expires_at > ? AND snapshot_digest=? AND row_ver=?`,
		string(to), operatorID, now, now, t.ID, t.TenantID, t.FarmID, now, t.SnapshotDigest, t.RowVer)
	return affected(res, "decide approval_ticket "+strconv.FormatUint(t.ID, 10))
}

// SetFirstApproverTx 照抄 P0 §6.7 HIGH 第一审批人 UPDATE。
func (r *DecisionRepo) SetFirstApproverTx(ctx context.Context, tx *gorm.DB, t TicketRow, userID uint64, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket SET first_approver_id=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='OPEN' AND first_approver_id IS NULL AND expires_at > ? AND row_ver=?`,
		userID, now, t.ID, t.TenantID, t.FarmID, now, t.RowVer)
	return affected(res, "first approver approval_ticket "+strconv.FormatUint(t.ID, 10))
}

// ExpireTicketTx 照抄 P0 §6.7 过期 UPDATE；影响行数 0（已被决策 / 已改写）返回 false，调用方跳过。
func (r *DecisionRepo) ExpireTicketTx(ctx context.Context, tx *gorm.DB, t TicketRow, now time.Time) (bool, error) {
	if err := requireTx(tx); err != nil {
		return false, err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket SET status='EXPIRED', row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='OPEN' AND row_ver=?`,
		now, t.ID, t.TenantID, t.FarmID, t.RowVer)
	if res.Error != nil {
		return false, fmt.Errorf("repository: expire approval_ticket %d: %w", t.ID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// VoidPreparingTx 作废停在 PREPARING 的票（run 已失败、永不会中断；文档未给 SQL，按过期 UPDATE 的守卫写法）。
func (r *DecisionRepo) VoidPreparingTx(ctx context.Context, tx *gorm.DB, t TicketRow, now time.Time) (bool, error) {
	if err := requireTx(tx); err != nil {
		return false, err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket SET status='EXPIRED', row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='PREPARING' AND row_ver=?`,
		now, t.ID, t.TenantID, t.FarmID, t.RowVer)
	if res.Error != nil {
		return false, fmt.Errorf("repository: void approval_ticket %d: %w", t.ID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// SetDispatchDeadlineTx 在 Transition(→ APPROVED) 之后写 dispatch_deadline_at（Transition 不写业务列；文档未给 SQL）。
func (r *DecisionRepo) SetDispatchDeadlineTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, deadline, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE work_order SET dispatch_deadline_at=?, updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status='APPROVED'`,
		deadline, now, ref.OrderID, ref.TenantID, ref.FarmID)
	return affected(res, "dispatch deadline work_order "+strconv.FormatUint(ref.OrderID, 10))
}

// InsertDispatchOutboxTx 照抄 P0 §6.7 outbox(order.dispatch)，dedup_key=order.dispatch:{order_id}:{ticket_id}:1。
func (r *DecisionRepo) InsertDispatchOutboxTx(ctx context.Context, tx *gorm.DB, t TicketRow, payload []byte, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	err := tx.WithContext(ctx).Exec(
		`INSERT INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'order.dispatch', ?, CONCAT('order.dispatch:', ?, ':', ?, ':1'), ?, 'PENDING', ?, ?, ?)`,
		t.TenantID, t.FarmID, t.OrderID, t.OrderID, t.ID, string(payload), now, now, now).Error
	if err != nil {
		return fmt.Errorf("repository: insert outbox order.dispatch %d: %w", t.OrderID, err)
	}
	return nil
}

// CloseRunTx 照抄 P0 §6.7 驳回 / 过期时 run 收尾：INTERRUPTED(WAIT_APPROVAL) → COMPLETED(reason_code)。
// 影响行数 0 → model.ErrConcurrentUpdate（票据 OPEN 时 run 必为 INTERRUPTED(WAIT_APPROVAL)，不一致即回滚）。
func (r *DecisionRepo) CloseRunTx(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64,
	reason model.ReasonCode, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE agent_run SET status='COMPLETED', interrupt_type=NULL, reason_code=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='INTERRUPTED' AND interrupt_type='WAIT_APPROVAL'`,
		string(reason), now, runID, scope.TenantID, scope.FarmID)
	return affected(res, "close agent_run "+strconv.FormatUint(runID, 10))
}

// InsertRejectedMemoryTx 驳回同事务写 episodic_memory(REJECTED)，ref_id 取 order_id，INSERT IGNORE 幂等（P0 §6.7 流程 5）。
func (r *DecisionRepo) InsertRejectedMemoryTx(ctx context.Context, tx *gorm.DB, t TicketRow, summary string, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	err := tx.WithContext(ctx).Exec(
		`INSERT IGNORE INTO episodic_memory (tenant_id, farm_id, field_id, kind, ref_id, summary, occurred_at)
SELECT tenant_id, farm_id, field_id, 'REJECTED', ?, ?, ? FROM agent_run WHERE id=? AND tenant_id=? AND farm_id=?`,
		t.OrderID, summary, now, t.RunID, t.TenantID, t.FarmID).Error
	if err != nil {
		return fmt.Errorf("repository: insert episodic_memory REJECTED order %d: %w", t.OrderID, err)
	}
	return nil
}

// AuditDenied 写 audit_log(action=APPROVAL, result=DENIED)：拒绝本身不改状态，独立短写（P0 §6.7 流程 2）。
// L7 收编到 AuditRepo：target 与事件投影一致为 ticket:{id}。
func (r *DecisionRepo) AuditDenied(ctx context.Context, scope model.Scope, t TicketRow, code string) error {
	summary, err := json.Marshal(map[string]any{"ticket_id": t.ID, "order_id": t.OrderID, "code": code})
	if err != nil {
		return fmt.Errorf("repository: encode audit summary: %w", err)
	}
	runID := t.RunID
	rec := NewAuditRecord(scope, &runID, model.Actor{Type: model.ActorUser, ID: scope.UserID}, model.AuditApproval,
		ref("ticket", t.ID), summary, model.AuditResultDenied, r.clock.Now())
	if err := (AuditRepo{DB: r.db}).Append(ctx, rec); err != nil {
		return fmt.Errorf("repository: APPROVAL DENIED ticket %d: %w", t.ID, err)
	}
	return nil
}

// PendingRow 是待审批列表的一行。L8 起补 snapshot_digest / first_approver / awaiting_confirm 与作业单快照（含约束校验），
// 后两项由 service 填。
type PendingRow struct {
	ID              uint64           `json:"ticket_id"`
	OrderID         uint64           `json:"order_id"`
	RiskLevel       string           `json:"risk_level"`
	ExpiresAt       *time.Time       `json:"expires_at"`
	SnapshotDigest  string           `json:"snapshot_digest"`
	FirstApproverID *uint64          `json:"first_approver"`
	ConfirmRequired int              `json:"-"`
	AwaitingConfirm bool             `json:"awaiting_confirm" gorm:"-"`
	WorkOrder       *model.WorkOrder `json:"work_order" gorm:"-"`
}

// ListPending 照抄 P0 §6.6 待审批列表 SQL；L8 在 SELECT 列上补 snapshot_digest / first_approver_id / confirm_required
// （WHERE / ORDER / LIMIT 不变）。
func (r *DecisionRepo) ListPending(ctx context.Context, scope model.Scope) ([]PendingRow, error) {
	rows := []PendingRow{}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, order_id, risk_level, expires_at, snapshot_digest, first_approver_id, confirm_required FROM approval_ticket WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN' ORDER BY created_at LIMIT 50`,
		scope.TenantID, scope.FarmID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list pending approval_ticket: %w", err)
	}
	return rows, nil
}

// OrdersWithChecks 批量读作业单快照及其全部约束校验（待审批列表用，两条 IN 查询、无 N+1；文档未给 SQL）。
// 返回按 order_id 索引；ids 为空返回空表。
func (r *DecisionRepo) OrdersWithChecks(ctx context.Context, scope model.Scope, ids []uint64) (map[uint64]model.WorkOrder, error) {
	out := make(map[uint64]model.WorkOrder, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var orders []struct {
		DispatchOrder
		PayloadRaw        []byte `gorm:"column:payload"`
		DispatchResultRaw []byte `gorm:"column:dispatch_result"`
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload, time_window_from, time_window_to,
  status, dispatch_mode, dispatch_result FROM work_order WHERE tenant_id=? AND farm_id=? AND id IN (?)`,
		scope.TenantID, scope.FarmID, ids).Scan(&orders).Error; err != nil {
		return nil, fmt.Errorf("repository: list pending work_order: %w", err)
	}
	for _, row := range orders {
		o := row.DispatchOrder
		o.Payload = rawJSON(row.PayloadRaw)
		w := o.WorkOrder()
		w.DispatchResult = rawJSON(row.DispatchResultRaw)
		w.ConstraintChecks = []model.ConstraintCheck{}
		out[o.ID] = w
	}
	var checks []struct {
		OrderID   uint64
		Phase     string
		Attempt   int
		RuleID    string
		Passed    bool
		Observed  []byte
		Threshold []byte
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT order_id, phase, attempt, rule_id, passed, observed, threshold FROM constraint_check
 WHERE tenant_id=? AND farm_id=? AND order_id IN (?) ORDER BY order_id, phase, attempt, id`,
		scope.TenantID, scope.FarmID, ids).Scan(&checks).Error; err != nil {
		return nil, fmt.Errorf("repository: list pending constraint_check: %w", err)
	}
	for _, c := range checks {
		w, ok := out[c.OrderID]
		if !ok {
			continue
		}
		w.ConstraintChecks = append(w.ConstraintChecks, model.ConstraintCheck{Phase: model.ConstraintPhase(c.Phase),
			Attempt: c.Attempt, RuleID: c.RuleID, Passed: c.Passed, Observed: rawJSON(c.Observed), Threshold: rawJSON(c.Threshold)})
		out[c.OrderID] = w
	}
	return out, nil
}

// ListExpired 照抄 P0 §6.7 过期扫描（idx_expire）：OPEN 且 expires_at 早于 now。
func (r *DecisionRepo) ListExpired(ctx context.Context, now time.Time) ([]TicketRow, error) {
	var rows []TicketRow
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, order_id, run_id, row_ver FROM approval_ticket
 WHERE ticket_kind='ORDER' AND status='OPEN' AND expires_at < ? LIMIT 100`, now).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list expired approval_ticket: %w", err)
	}
	return rows, nil
}

// OrderKey 是扫描取出的作业单。
type OrderKey struct {
	ID       uint64
	TenantID uint64
	FarmID   uint64
	RowVer   int
}

// Ref 返回作业单定位。
func (k OrderKey) Ref() model.OrderRef {
	return model.OrderRef{TenantID: k.TenantID, FarmID: k.FarmID, OrderID: k.ID}
}

// ListDispatchOverdue 照抄 P0 §6.7：APPROVED 且过 dispatch_deadline_at 的作业单（idx_dispatch_deadline）。
func (r *DecisionRepo) ListDispatchOverdue(ctx context.Context, now time.Time) ([]OrderKey, error) {
	var rows []OrderKey
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, row_ver FROM work_order WHERE status='APPROVED' AND dispatch_deadline_at < ? LIMIT 100`,
		now).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list dispatch overdue work_order: %w", err)
	}
	return rows, nil
}

// StaleTicket 是 PREPARING 扫描的一行：票据 + 所属 run 的状态。
type StaleTicket struct {
	TicketRow
	RunStatus     string
	InterruptType *string
}

// ListPreparing 取创建早于 before 仍 PREPARING 的票及其 run 状态（ticket_activate；文档未给 SQL）。
func (r *DecisionRepo) ListPreparing(ctx context.Context, before time.Time) ([]StaleTicket, error) {
	var rows []StaleTicket
	if err := r.db.WithContext(ctx).Raw(
		`SELECT t.id, t.tenant_id, t.farm_id, t.order_id, t.run_id, t.row_ver, r.status AS run_status, r.interrupt_type
 FROM approval_ticket t JOIN agent_run r ON r.id=t.run_id AND r.tenant_id=t.tenant_id AND r.farm_id=t.farm_id
 WHERE t.ticket_kind='ORDER' AND t.status='PREPARING' AND t.created_at < ? LIMIT ?`,
		before, scanLimit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list PREPARING approval_ticket: %w", err)
	}
	return rows, nil
}

// ListApprovedInterrupted 取 run 仍 INTERRUPTED(WAIT_APPROVAL) 而票据 APPROVED 早于 before 的 run（run_finalize；文档未给 SQL）。
func (r *DecisionRepo) ListApprovedInterrupted(ctx context.Context, before time.Time) ([]RunKey, error) {
	var rows []RunKey
	if err := r.db.WithContext(ctx).Raw(
		`SELECT r.id, r.tenant_id, r.farm_id, r.row_ver
 FROM agent_run r JOIN approval_ticket t ON t.run_id=r.id AND t.tenant_id=r.tenant_id AND t.farm_id=r.farm_id
 WHERE r.status='INTERRUPTED' AND r.interrupt_type='WAIT_APPROVAL' AND t.ticket_kind='ORDER' AND t.status='APPROVED' AND t.decided_at < ? LIMIT ?`,
		before, scanLimit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list approved interrupted agent_run: %w", err)
	}
	return rows, nil
}
