package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ApprovalRepo 是 constraint_check / approval_ticket 读写（P0 §6.6 定版 SQL）。写方法只在调用方事务内使用；
// 读方法接受任意句柄。
type ApprovalRepo struct{}

// PendingTicket 是待审批列表的一行（§6.6 SQL 末段的四列）。
type PendingTicket struct {
	TicketID  uint64
	OrderID   uint64
	RiskLevel model.RiskLevel
	ExpiresAt *time.Time
}

// ticketRow 是 approval_ticket 的一行（列名即 SELECT 别名）。
type ticketRow struct {
	ID              uint64
	RunID           uint64
	TenantID        uint64
	FarmID          uint64
	OrderID         uint64
	TicketKind      string
	TicketSeq       int
	RiskLevel       string
	RequiredRole    string
	ConfirmRequired bool
	InitiatorID     uint64
	FirstApproverID *uint64
	SnapshotDigest  string
	SnapshotRawID   uint64
	Status          string
	OperatorID      *uint64
	ExpiresAt       *time.Time
	DecidedAt       *time.Time
}

const ticketColumns = `id, run_id, tenant_id, farm_id, order_id, ticket_kind, ticket_seq, risk_level, required_role, confirm_required,
  initiator_id, first_approver_id, snapshot_digest, snapshot_raw_id, status, operator_id, expires_at, decided_at`

func (r ticketRow) ticket() (model.ApprovalTicket, error) {
	var roles []model.Role
	if err := json.Unmarshal([]byte(r.RequiredRole), &roles); err != nil {
		return model.ApprovalTicket{}, fmt.Errorf("repository: ticket %d required_role: %w", r.ID, err)
	}
	return model.ApprovalTicket{TicketID: r.ID, RunID: r.RunID, TenantID: r.TenantID, FarmID: r.FarmID,
		OrderID: r.OrderID, TicketKind: model.TicketKind(r.TicketKind), TicketSeq: r.TicketSeq,
		RiskLevel: model.RiskLevel(r.RiskLevel), RequiredRole: roles, ConfirmRequired: r.ConfirmRequired,
		InitiatorID: r.InitiatorID, FirstApprover: r.FirstApproverID,
		Snapshot: model.TicketSnapshot{Digest: r.SnapshotDigest, RawObjectID: r.SnapshotRawID},
		Status:   model.TicketStatus(r.Status), Operator: r.OperatorID, ExpiresAt: r.ExpiresAt, DecidedAt: r.DecidedAt}, nil
}

// scanTicket 执行一条返回 0 / 1 行的票据查询；无行返回 found=false。
func scanTicket(ctx context.Context, db *gorm.DB, query string, args ...any) (model.ApprovalTicket, bool, error) {
	var row ticketRow
	res := db.WithContext(ctx).Raw(query, args...).Scan(&row)
	if res.Error != nil {
		return model.ApprovalTicket{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return model.ApprovalTicket{}, false, nil
	}
	t, err := row.ticket()
	if err != nil {
		return model.ApprovalTicket{}, false, err
	}
	return t, true, nil
}

// TicketByOrderSeq 读某作业单第 seq 张 ORDER 票（uk_order_seq），供开票前判断是否已开过。
func (ApprovalRepo) TicketByOrderSeq(ctx context.Context, db *gorm.DB, ref model.OrderRef, seq int) (model.ApprovalTicket, bool, error) {
	t, ok, err := scanTicket(ctx, db, `SELECT `+ticketColumns+` FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND order_id=? AND ticket_seq=?`,
		ref.TenantID, ref.FarmID, ref.OrderID, seq)
	if err != nil {
		return model.ApprovalTicket{}, false, fmt.Errorf("repository: ticket order %d seq %d: %w", ref.OrderID, seq, err)
	}
	return t, ok, nil
}

// TicketByRun 读该 run 最新一张 ORDER 票（idx_run；P0 一个 run 至多一张单一张票，取 ticket_seq 最大者）。
func (ApprovalRepo) TicketByRun(ctx context.Context, db *gorm.DB, scope model.Scope, runID uint64) (model.ApprovalTicket, bool, error) {
	t, ok, err := scanTicket(ctx, db, `SELECT `+ticketColumns+` FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND run_id=? AND ticket_kind='ORDER' ORDER BY ticket_seq DESC, id DESC LIMIT 1`,
		scope.TenantID, scope.FarmID, runID)
	if err != nil {
		return model.ApprovalTicket{}, false, fmt.Errorf("repository: ticket run %d: %w", runID, err)
	}
	return t, ok, nil
}

// InsertChecksTx 写审批前约束校验（§6.6：INSERT IGNORE，uk_check 幂等）；attempt 取 ticket_seq。
func (ApprovalRepo) InsertChecksTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, attempt int,
	checks []model.ConstraintCheck, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	for _, c := range checks {
		if c.Phase != model.PhasePreApproval {
			return fmt.Errorf("repository: constraint_check %s phase %q, want %s", c.RuleID, c.Phase, model.PhasePreApproval)
		}
		err := tx.WithContext(ctx).Exec(
			`INSERT IGNORE INTO constraint_check (tenant_id, farm_id, order_id, phase, attempt, rule_id, passed, observed, threshold, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ref.TenantID, ref.FarmID, ref.OrderID, string(c.Phase), attempt, c.RuleID, c.Passed,
			string(c.Observed), string(c.Threshold), now).Error
		if err != nil {
			return fmt.Errorf("repository: insert constraint_check order %d rule %s: %w", ref.OrderID, c.RuleID, err)
		}
	}
	return nil
}

// InsertTicketTx 插入 PREPARING 票（§6.6 定版 SQL，ticket_seq 固定 1）；命中 uk_order_seq 时返回已有 id 且 created=false。
func (ApprovalRepo) InsertTicketTx(ctx context.Context, tx *gorm.DB, t model.ApprovalTicket, now time.Time) (id uint64, created bool, err error) {
	roles, err := json.Marshal(t.RequiredRole)
	if err != nil {
		return 0, false, fmt.Errorf("repository: encode required_role: %w", err)
	}
	res, err := execInTx(ctx, tx,
		`INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_kind, ticket_seq, risk_level, required_role, confirm_required,
  initiator_id, snapshot_digest, snapshot_raw_id, status, created_at, updated_at)
VALUES (?, ?, ?, ?, 'ORDER', 1, ?, ?, ?, ?, ?, ?, 'PREPARING', ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
		t.TenantID, t.FarmID, t.RunID, t.OrderID, string(t.RiskLevel), string(roles), t.ConfirmRequired,
		t.InitiatorID, t.Snapshot.Digest, t.Snapshot.RawObjectID, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("repository: insert approval_ticket order %d: %w", t.OrderID, err)
	}
	last, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("repository: approval_ticket id: %w", err)
	}
	// 新插入影响 1 行；命中 uk_order_seq 且 id 未变时为 0 行（未开 CLIENT_FOUND_ROWS）。
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("repository: approval_ticket rows: %w", err)
	}
	return uint64(last), n == 1, nil
}

// ActivateTx 把该 run 的 PREPARING 票置 OPEN（§6.6 定版 UPDATE JOIN：run 须已 INTERRUPTED(WAIT_APPROVAL)），
// 返回影响行数（0 表示前置条件不满足）。
func (ApprovalRepo) ActivateTx(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64,
	expiresAt, now time.Time) (int64, error) {
	if err := requireTx(tx); err != nil {
		return 0, err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket t JOIN agent_run r ON r.id=t.run_id AND r.tenant_id=t.tenant_id AND r.farm_id=t.farm_id
   SET t.status='OPEN', t.expires_at=?, t.row_ver=t.row_ver+1, t.updated_at=?
 WHERE t.tenant_id=? AND t.farm_id=? AND t.run_id=? AND t.ticket_kind='ORDER' AND t.status='PREPARING'
   AND r.status='INTERRUPTED' AND r.interrupt_type='WAIT_APPROVAL'`,
		expiresAt, now, scope.TenantID, scope.FarmID, runID)
	if res.Error != nil {
		return 0, fmt.Errorf("repository: activate ticket run %d: %w", runID, res.Error)
	}
	return res.RowsAffected, nil
}

// ListOpen 是待审批列表（§6.6 定版 SELECT，最多 50 条，按创建时间）。
func (ApprovalRepo) ListOpen(ctx context.Context, db *gorm.DB, scope model.Scope) ([]PendingTicket, error) {
	var rows []struct {
		ID        uint64
		OrderID   uint64
		RiskLevel string
		ExpiresAt *time.Time
	}
	err := db.WithContext(ctx).Raw(`SELECT id, order_id, risk_level, expires_at FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN' ORDER BY created_at LIMIT 50`,
		scope.TenantID, scope.FarmID).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list open tickets: %w", err)
	}
	out := make([]PendingTicket, len(rows))
	for i, r := range rows {
		out[i] = PendingTicket{TicketID: r.ID, OrderID: r.OrderID, RiskLevel: model.RiskLevel(r.RiskLevel), ExpiresAt: r.ExpiresAt}
	}
	return out, nil
}
