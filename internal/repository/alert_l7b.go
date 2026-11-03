package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// P1 L7-B 告警读写路径的查询：无引用建议扫描（系统级）、告警去重、OPEN 票积压（系统级）与告警列表（按农场）。
// ALERT.* 审计行只经 AuditRepo.Append 写入（service.AuditAlertSink），这里只读。

// AlertRepo 读告警相关事实（只读）。
type AlertRepo struct{ DB *gorm.DB }

// AdviceKey 是一张建议卡的定位（advice_card 每个 run 至多一张，uk_run）。
type AdviceKey struct {
	ID       uint64
	TenantID uint64
	FarmID   uint64
	RunID    uint64
}

// adviceWithoutCitationSQL 是 P1 §6.8 定版 SQL（REFUSED 不命中；无结尾分号，审查修复 P3）。
const adviceWithoutCitationSQL = `SELECT a.id, a.tenant_id, a.farm_id, a.run_id FROM advice_card a
 WHERE a.outcome<>'REFUSED' AND a.created_at BETWEEN ? AND ?
   AND NOT EXISTS (SELECT 1 FROM advice_citation c WHERE c.tenant_id=a.tenant_id AND c.farm_id=a.farm_id AND c.advice_id=a.id)`

// AdviceWithoutCitation 返回 [from, to] 内创建、非 REFUSED 且无任何 advice_citation 的建议卡（P1 §6.8 定版 SQL 照抄）。
func (r AlertRepo) AdviceWithoutCitation(ctx context.Context, sys SystemScope, from, to time.Time) ([]AdviceKey, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []AdviceKey
	if err := r.DB.WithContext(ctx).Raw(adviceWithoutCitationSQL, from, to).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list advice without citation: %w", err)
	}
	return rows, nil
}

// AlertedRuns 返回 runIDs 中自 since 起已有 action 审计行的 run（告警去重；走 idx_run，since 裁分区）。
func (r AlertRepo) AlertedRuns(ctx context.Context, scope model.Scope, action model.AuditAction, runIDs []uint64,
	since time.Time) (map[uint64]bool, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	out := map[uint64]bool{}
	if len(runIDs) == 0 {
		return out, nil
	}
	var ids []uint64
	if err := r.DB.WithContext(ctx).Raw(`SELECT DISTINCT run_id FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND run_id IN ? AND action=? AND created_at>=?`,
		scope.TenantID, scope.FarmID, runIDs, string(action), since).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: alerted runs %s: %w", action, err)
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// FarmCount 是一个农场的计数。
type FarmCount struct {
	TenantID uint64
	FarmID   uint64
	N        int
}

// OpenOrderTickets 按农场统计 OPEN 的 ORDER 票（审批积压 gauge）。
func (r AlertRepo) OpenOrderTickets(ctx context.Context, sys SystemScope) ([]FarmCount, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []FarmCount
	if err := r.DB.WithContext(ctx).Raw(`SELECT tenant_id, farm_id, COUNT(*) AS n FROM approval_ticket
 WHERE ticket_kind='ORDER' AND status='OPEN' GROUP BY tenant_id, farm_id`).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: count open order tickets: %w", err)
	}
	return rows, nil
}

// KnownFarms 返回 farm_profile 登记的全部 farm_id（去重升序；预置 0 值指标序列用，系统级）。
func (r AlertRepo) KnownFarms(ctx context.Context, sys SystemScope) ([]uint64, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var ids []uint64
	if err := r.DB.WithContext(ctx).Raw(`SELECT DISTINCT farm_id FROM farm_profile ORDER BY farm_id`).
		Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: list known farms: %w", err)
	}
	return ids, nil
}

// AlertRow 是告警列表的一行（audit_log 的 ALERT.* / 业务告警审计）。
type AlertRow struct {
	ID        uint64
	CreatedAt time.Time
	RunID     *uint64
	Action    string
	Target    string
	Result    string
	Summary   json.RawMessage
}

// AlertCursor 是告警列表的键集分页游标：上一页最后一行的 (created_at, id)；零值表示第一页。
type AlertCursor struct {
	At time.Time
	ID uint64
}

// IsZero 表示无游标（第一页）。
func (c AlertCursor) IsZero() bool { return c.At.IsZero() && c.ID == 0 }

// ListAlerts 读本农场 [from, to) 内 action 属于 actions 的审计行，时间倒序（走 idx_scope_time）。
// before 非零时只取 (created_at, id) < (before.At, before.ID) 的行（审查修复 P2：键集分页；写成展开的 OR 形式，
// 与行构造器比较等价且走索引范围）。WHERE 仍以 tenant_id=? AND farm_id=? 开头。
func (r AlertRepo) ListAlerts(ctx context.Context, scope model.Scope, actions []string, from, to time.Time,
	before AlertCursor, limit int) ([]AlertRow, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if len(actions) == 0 {
		return nil, nil
	}
	q := `SELECT id, created_at, run_id, action, target, result, summary FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND action IN ? AND created_at>=? AND created_at<?`
	args := []any{scope.TenantID, scope.FarmID, actions, from, to}
	if !before.IsZero() {
		q += ` AND (created_at<? OR (created_at=? AND id<?))`
		args = append(args, before.At, before.At, before.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	var rows []struct {
		ID        uint64
		CreatedAt time.Time
		RunID     *uint64
		Action    string
		Target    *string
		Result    string
		Summary   []byte
	}
	if err := r.DB.WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list alerts: %w", err)
	}
	out := make([]AlertRow, 0, len(rows))
	for _, x := range rows {
		out = append(out, AlertRow{ID: x.ID, CreatedAt: x.CreatedAt, RunID: x.RunID, Action: x.Action,
			Target: deref(x.Target), Result: x.Result, Summary: rawJSON(x.Summary)})
	}
	return out, nil
}
