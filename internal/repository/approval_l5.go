package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// P1 L5 审批体系的 SQL（P1 §6.6 定版 SQL 照抄；文档未给的另行标注）：第二审批人生效守卫、驳回原因、即将过期扫描、
// 驳回导出、通知接收人。

// ConfirmTicketTx 照抄 §6.6「第二审批人生效」UPDATE：P0 决策守卫上追加 first_approver_id IS NOT NULL AND
// first_approver_id <> 本人。影响行数 0 → model.ErrConcurrentUpdate（调用方按现有冲突处理重读票据）。
func (r *DecisionRepo) ConfirmTicketTx(ctx context.Context, tx *gorm.DB, t TicketRow, operatorID uint64, now time.Time) error {
	if err := requireFarm(t.TenantID, t.FarmID); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE approval_ticket SET status='APPROVED', operator_id=?, decided_at=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN' AND expires_at > ?
   AND snapshot_digest=? AND first_approver_id IS NOT NULL AND first_approver_id <> ? AND row_ver=?`,
		operatorID, now, now, t.ID, t.TenantID, t.FarmID, now, t.SnapshotDigest, operatorID, t.RowVer)
	return affected(res, "confirm approval_ticket "+strconv.FormatUint(t.ID, 10))
}

// RejectionInput 是一条驳回原因的写入值；order_id / advice_id / run_id 由 SQL 从 work_order 取。
type RejectionInput struct {
	ReasonCode   model.RejectReasonCode
	ReasonText   *string
	OperatorID   uint64
	OperatorRole model.Role
}

// InsertRejectionTx 在 P0 驳回事务内写 rejection_reason（只追加，uk_ticket 一票一行）。文档只给 DDL，INSERT 自拟：
// INSERT … SELECT 从 work_order 取 order_id / advice_id / run_id；作业单缺失（0 行）→ model.ErrConcurrentUpdate 回滚驳回。
func (r *DecisionRepo) InsertRejectionTx(ctx context.Context, tx *gorm.DB, t TicketRow, in RejectionInput, now time.Time) error {
	if err := requireFarm(t.TenantID, t.FarmID); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`INSERT INTO rejection_reason (tenant_id, farm_id, ticket_id, order_id, advice_id, run_id, reason_code, reason_text, operator_id, operator_role, created_at)
SELECT tenant_id, farm_id, ?, id, advice_id, run_id, ?, ?, ?, ?, ? FROM work_order WHERE tenant_id=? AND farm_id=? AND id=?`,
		t.ID, string(in.ReasonCode), in.ReasonText, in.OperatorID, string(in.OperatorRole), now, t.TenantID, t.FarmID, t.OrderID)
	return affected(res, "insert rejection_reason ticket "+strconv.FormatUint(t.ID, 10))
}

// ExpiringTicket 是即将过期扫描取出的票。
type ExpiringTicket struct {
	ID       uint64
	TenantID uint64
	FarmID   uint64
	OrderID  uint64
}

// ListExpiring 照抄 §6.6「即将过期提醒」（worker 专用索引 idx_expire）：expires_at ∈ [now, now + 1h] 的 OPEN 票。
func (r *DecisionRepo) ListExpiring(ctx context.Context, sys SystemScope, now time.Time) ([]ExpiringTicket, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []ExpiringTicket
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, order_id FROM approval_ticket
 WHERE ticket_kind='ORDER' AND status='OPEN' AND expires_at BETWEEN ? AND ? + INTERVAL 1 HOUR LIMIT 100`,
		now, now).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list expiring approval_ticket: %w", err)
	}
	return rows, nil
}

// RejectionRepo 是驳回原因导出（P1 §6.6 流程 6）。
type RejectionRepo struct {
	DB *gorm.DB
}

// ListRejections 照抄 §6.6「驳回导出」：本农场 created_at ∈ [from, to] 的驳回原因，按时间升序（走 idx_scope_time）。
func (r RejectionRepo) ListRejections(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.RejectionReason, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []struct {
		ID           uint64
		TenantID     uint64
		FarmID       uint64
		TicketID     uint64
		OrderID      uint64
		AdviceID     uint64
		RunID        uint64
		ReasonCode   string
		ReasonText   *string
		OperatorID   uint64
		OperatorRole string
		CreatedAt    time.Time
	}
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT r.*, t.order_id FROM rejection_reason r JOIN approval_ticket t ON t.tenant_id=r.tenant_id AND t.farm_id=r.farm_id AND t.id=r.ticket_id
 WHERE r.tenant_id=? AND r.farm_id=? AND r.created_at BETWEEN ? AND ? ORDER BY r.created_at`,
		scope.TenantID, scope.FarmID, from, to).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list rejection_reason: %w", err)
	}
	out := make([]model.RejectionReason, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.RejectionReason{ID: row.ID, TenantID: row.TenantID, FarmID: row.FarmID, TicketID: row.TicketID,
			OrderID: row.OrderID, AdviceID: row.AdviceID, RunID: row.RunID, ReasonCode: model.RejectReasonCode(row.ReasonCode),
			ReasonText: row.ReasonText, OperatorID: row.OperatorID, OperatorRole: model.Role(row.OperatorRole), CreatedAt: row.CreatedAt})
	}
	return out, nil
}

// RecipientRepo 是通知接收人查询（P1 L5 notify.send 处理器）。
type RecipientRepo struct {
	DB *gorm.DB
}

// FarmUsersByRoles 返回本农场绑定了任一角色的用户（去重、升序）。文档未给，自拟：走 idx_farm_role (tenant_id, farm_id, role)。
func (r RecipientRepo) FarmUsersByRoles(ctx context.Context, scope model.Scope, roles []model.Role) ([]uint64, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		return nil, nil
	}
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = string(role)
	}
	var ids []uint64
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT DISTINCT user_id FROM user_role WHERE tenant_id=? AND farm_id=? AND role IN ? ORDER BY user_id`,
		scope.TenantID, scope.FarmID, names).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: farm users by roles: %w", err)
	}
	return ids, nil
}

// TicketParties 是通知接收人需要的票据当事人：发起人与可审批角色。
type TicketParties struct {
	InitiatorID  uint64
	RequiredRole []model.Role
}

// TicketParties 返回票据发起人与 required_role（通知接收人用）。ticketID 为 0 时取该单 ticket_seq 最大的 ORDER 票
// （如设备离线通知不带票）；查无 → model.ErrNotFound。文档未给，自拟：主键 / uk_order_seq 查询。
func (r RecipientRepo) TicketParties(ctx context.Context, scope model.Scope, orderID, ticketID uint64) (TicketParties, error) {
	if err := requireScope(scope); err != nil {
		return TicketParties{}, err
	}
	var rows []struct {
		InitiatorID  uint64
		RequiredRole []byte
	}
	q := r.DB.WithContext(ctx)
	if ticketID != 0 {
		q = q.Raw(`SELECT initiator_id, required_role FROM approval_ticket WHERE tenant_id=? AND farm_id=? AND id=? AND order_id=?`,
			scope.TenantID, scope.FarmID, ticketID, orderID)
	} else {
		q = q.Raw(`SELECT initiator_id, required_role FROM approval_ticket WHERE tenant_id=? AND farm_id=? AND order_id=? AND ticket_kind='ORDER' ORDER BY ticket_seq DESC LIMIT 1`,
			scope.TenantID, scope.FarmID, orderID)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return TicketParties{}, fmt.Errorf("repository: ticket parties order %d ticket %d: %w", orderID, ticketID, err)
	}
	if len(rows) == 0 {
		return TicketParties{}, fmt.Errorf("repository: ticket parties order %d ticket %d: %w", orderID, ticketID, model.ErrNotFound)
	}
	p := TicketParties{InitiatorID: rows[0].InitiatorID}
	if err := json.Unmarshal(rows[0].RequiredRole, &p.RequiredRole); err != nil {
		return TicketParties{}, fmt.Errorf("repository: ticket parties order %d required_role: %w", orderID, err)
	}
	return p, nil
}

// TicketRiskReasons 读票据 risk_reasons（评测观测用，P1 L5）：NULL（P0 票、复检退回的新票）返回 nil。查无 → model.ErrNotFound。
// 文档未给，自拟：主键查询。
func (ApprovalRepo) TicketRiskReasons(ctx context.Context, db *gorm.DB, scope model.Scope, ticketID uint64) ([]string, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []struct{ RiskReasons []byte }
	if err := db.WithContext(ctx).Raw(`SELECT risk_reasons FROM approval_ticket WHERE tenant_id=? AND farm_id=? AND id=?`,
		scope.TenantID, scope.FarmID, ticketID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: ticket %d risk_reasons: %w", ticketID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("repository: ticket %d risk_reasons: %w", ticketID, model.ErrNotFound)
	}
	if len(rows[0].RiskReasons) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(rows[0].RiskReasons, &out); err != nil {
		return nil, fmt.Errorf("repository: decode ticket %d risk_reasons: %w", ticketID, err)
	}
	return out, nil
}

// TicketDeniedCodes 读该票的 audit_log(APPROVAL, DENIED) 拒绝码（summary.code，按写入顺序；评测观测用，P1 L5）。
// 文档未给，自拟：走 idx_scope_time 按 target 过滤。
func (ApprovalRepo) TicketDeniedCodes(ctx context.Context, db *gorm.DB, scope model.Scope, ticketID uint64) ([]string, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	codes := []string{}
	if err := db.WithContext(ctx).Raw(
		`SELECT COALESCE(JSON_UNQUOTE(JSON_EXTRACT(summary, '$.code')), '') FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND action=? AND result=? AND target=? ORDER BY created_at, id`,
		scope.TenantID, scope.FarmID, string(model.AuditApproval), model.AuditResultDenied, ref("ticket", ticketID)).Scan(&codes).Error; err != nil {
		return nil, fmt.Errorf("repository: ticket %d denied audits: %w", ticketID, err)
	}
	return codes, nil
}
