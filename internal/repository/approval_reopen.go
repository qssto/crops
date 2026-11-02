package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ReopenRepo 是复检退回（P1 §6.4 第 4 条）的票据与通知写入；SQL 照抄 §6.4「复检退回」两条，通知 INSERT IGNORE 按 uk_dedup 去重。
// 只在调用方事务内使用（Ticket 读可在事务外）。
type ReopenRepo struct{ DB *gorm.DB }

// Ticket 读一张 ORDER 票（含 row_ver，Supersede 守卫用）；db 为 nil 时用连接。
func (r ReopenRepo) Ticket(ctx context.Context, db *gorm.DB, scope model.Scope, ticketID uint64) (TicketRow, error) {
	if err := requireFarm(scope.TenantID, scope.FarmID); err != nil {
		return TicketRow{}, err
	}
	if db == nil {
		db = r.DB
	}
	var row TicketRow
	res := db.WithContext(ctx).Raw(
		`SELECT `+decisionTicketColumns+` FROM approval_ticket WHERE tenant_id=? AND farm_id=? AND id=? AND ticket_kind='ORDER'`,
		scope.TenantID, scope.FarmID, ticketID).Scan(&row)
	if res.Error != nil {
		return TicketRow{}, fmt.Errorf("repository: reopen read approval_ticket %d: %w", ticketID, res.Error)
	}
	if res.RowsAffected == 0 {
		return TicketRow{}, fmt.Errorf("repository: approval_ticket %d: %w", ticketID, model.ErrNotFound)
	}
	return row, nil
}

// SupersedeTicketTx 照抄 §6.4：旧票 APPROVED → SUPERSEDED（row_ver 守卫）；影响 0 行 → model.ErrConcurrentUpdate（整个退回事务回滚）。
func (r ReopenRepo) SupersedeTicketTx(ctx context.Context, tx *gorm.DB, t TicketRow, now time.Time) error {
	if err := requireFarm(t.TenantID, t.FarmID); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket SET status='SUPERSEDED', row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND ticket_kind='ORDER' AND status='APPROVED' AND row_ver=?`,
		now, t.TenantID, t.FarmID, t.ID, t.RowVer)
	return affected(res, fmt.Sprintf("supersede approval_ticket %d", t.ID))
}

// InsertReopenedTicketTx 照抄 §6.4：插入退回后的新票（ticket_seq 由调用方 +1，直接 OPEN 并写 expires_at），返回 id。
// 命中 uk_order_seq（同序号已插过）原样报错，整个退回事务回滚。
func (r ReopenRepo) InsertReopenedTicketTx(ctx context.Context, tx *gorm.DB, t model.ApprovalTicket, expiresAt,
	now time.Time) (uint64, error) {
	if err := requireFarm(t.TenantID, t.FarmID); err != nil {
		return 0, err
	}
	roles, err := json.Marshal(t.RequiredRole)
	if err != nil {
		return 0, fmt.Errorf("repository: encode required_role: %w", err)
	}
	res, err := execInTx(ctx, tx,
		`INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_kind, ticket_seq, risk_level, required_role, confirm_required, initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, 'ORDER', ?, ?, ?, ?, ?, ?, ?, 'OPEN', ?, ?, ?)`,
		t.TenantID, t.FarmID, t.RunID, t.OrderID, t.TicketSeq, string(t.RiskLevel), string(roles), t.ConfirmRequired,
		t.InitiatorID, t.Snapshot.Digest, t.Snapshot.RawObjectID, expiresAt, now, now)
	if err != nil {
		return 0, fmt.Errorf("repository: insert reopened approval_ticket order %d seq %d: %w", t.OrderID, t.TicketSeq, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("repository: reopened approval_ticket id: %w", err)
	}
	return uint64(id), nil
}

// InsertNotifyOutboxTx 写一条 notify.send（INSERT IGNORE，uk_dedup 去重）；返回是否新插入（false 即同 dedup_key 已有）。
func (r ReopenRepo) InsertNotifyOutboxTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, dedupKey string, payload []byte,
	now time.Time) (bool, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return false, err
	}
	res, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 'PENDING', ?, ?, ?)`,
		ref.TenantID, ref.FarmID, TopicNotifySend, ref.OrderID, dedupKey, string(payload), now, now, now)
	if err != nil {
		return false, fmt.Errorf("repository: insert outbox %s: %w", dedupKey, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("repository: outbox %s rows: %w", dedupKey, err)
	}
	return n == 1, nil
}
