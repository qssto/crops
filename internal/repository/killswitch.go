package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// KillSwitchRepo 是一键关停报告与灰度前检查的只读查询（P1 §6.9 技术实现第 1、4 条，cmd/auditctl 专用）。
// dispatch_attempt 两条与 DISPATCH_HELD 计数以 §6.9 定版 SQL 为底（走 idx_mode_sent / idx_action，加解除上界）；审计行读取自写，
// 只读已封链行（chain_seq IS NOT NULL，绕过封链直接 INSERT 的行不认）。
type KillSwitchRepo struct{ DB *gorm.DB }

// AuditEntry 是已封链审计行的只读投影（summary 由调用方按 action 解析）；Chain 是带链字段的整行，供 VerifyRowHash 重算。
type AuditEntry struct {
	ID        uint64
	CreatedAt time.Time
	Result    string
	Summary   json.RawMessage
	Chain     ChainRow
}

// ErrAuditRowHash 表示审计行按 L7-A 规范化重算的 row_hash 与库中不一致（行被改过或绕过封链写入）。
var ErrAuditRowHash = errors.New("repository: audit row_hash mismatch")

// VerifyRowHash 按 L7-A 规范化重算本行 row_hash（只校验行自身，不校验与前一行的链接；全链校验用 auditctl verify）。
func (e AuditEntry) VerifyRowHash() error {
	got, err := AuditRowHash(e.Chain.PrevHash, e.Chain)
	if err != nil {
		return fmt.Errorf("repository: rehash audit_log %d: %w", e.ID, err)
	}
	if got != e.Chain.RowHash {
		return fmt.Errorf("%w: audit_log %d", ErrAuditRowHash, e.ID)
	}
	return nil
}

// platformAudits 读平台链 (0,0) 上已封链（chain_seq IS NOT NULL）的审计行；query 须选 chainRowCols。
func (r KillSwitchRepo) platformAudits(ctx context.Context, query string, args ...any) ([]AuditEntry, error) {
	rows, err := r.DB.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return nil, err
	}
	chain, scanErr := scanChainRows(rows)
	if err := errors.Join(scanErr, rows.Close()); err != nil {
		return nil, err
	}
	out := make([]AuditEntry, 0, len(chain))
	for _, c := range chain {
		out = append(out, AuditEntry{ID: c.ID, CreatedAt: c.Rec.CreatedAt, Result: c.Rec.Result, Summary: rawJSON(c.Rec.Summary),
			Chain: c})
	}
	return out, nil
}

// FlagChangesSince 返回平台链上 target 的已封链 FLAG_CHANGE 审计（created_at >= since，时间升序，最多 limit 条）；
// GLOBAL 行写入的审计落 (0,0)，调用方按 summary 再筛 scope_type / old / new 值。SQL 文档未给，自写。
func (r KillSwitchRepo) FlagChangesSince(ctx context.Context, target string, since time.Time, limit int) ([]AuditEntry, error) {
	rows, err := r.platformAudits(ctx, `SELECT `+chainRowCols+` FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND action=? AND created_at >= ? AND target=? AND chain_seq IS NOT NULL ORDER BY created_at, id LIMIT ?`,
		0, 0, string(model.AuditFlagChange), since, target, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: FLAG_CHANGE %s since %s: %w", target, since.Format(time.RFC3339), err)
	}
	return rows, nil
}

// FlagChangeByID 按 id 取平台链上已封链的 FLAG_CHANGE 审计（gray-preflight 核对演练 summary 引用的 T0）；没有时 found=false。
func (r KillSwitchRepo) FlagChangeByID(ctx context.Context, id uint64) (AuditEntry, bool, error) {
	rows, err := r.platformAudits(ctx, `SELECT `+chainRowCols+` FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND id=? AND action=? AND chain_seq IS NOT NULL`,
		0, 0, id, string(model.AuditFlagChange))
	if err != nil {
		return AuditEntry{}, false, fmt.Errorf("repository: FLAG_CHANGE audit %d: %w", id, err)
	}
	if len(rows) == 0 {
		return AuditEntry{}, false, nil
	}
	return rows[0], true, nil
}

// LatestDrill 返回平台链上 created_at >= since 的最近一条已封链 KILL_SWITCH_DRILL 审计；没有时 found=false。SQL 文档未给，自写。
func (r KillSwitchRepo) LatestDrill(ctx context.Context, since time.Time) (AuditEntry, bool, error) {
	rows, err := r.platformAudits(ctx, `SELECT `+chainRowCols+` FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND action=? AND created_at >= ? AND chain_seq IS NOT NULL ORDER BY created_at DESC, id DESC LIMIT 1`,
		0, 0, string(model.AuditKillSwitchDrill), since)
	if err != nil {
		return AuditEntry{}, false, fmt.Errorf("repository: latest KILL_SWITCH_DRILL since %s: %w", since.Format(time.RFC3339), err)
	}
	if len(rows) == 0 {
		return AuditEntry{}, false, nil
	}
	return rows[0], true, nil
}

// RealSentAfter 返回 T0 之后、until（解除关停时刻，nil 表示未解除）之前发出的 REAL 尝试的最大 sent_at（没有时为 nil）与条数。
// §6.9 定版 SQL 加了上界 AND (? IS NULL OR sent_at < ?)（L8-B 审查 P2：解除后重跑不把正常下发算进来），跨农场走 idx_mode_sent。
func (r KillSwitchRepo) RealSentAfter(ctx context.Context, sys SystemScope, t0 time.Time, until *time.Time) (*time.Time, int64, error) {
	if err := requireSystem(sys); err != nil {
		return nil, 0, err
	}
	var maxSent sql.NullTime
	var n int64
	err := r.DB.WithContext(ctx).Raw(
		`SELECT MAX(sent_at), COUNT(*) FROM dispatch_attempt WHERE mode='REAL' AND sent_at > ? AND (? IS NULL OR sent_at < ?)`,
		t0, until, until).Row().Scan(&maxSent, &n)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: REAL dispatch_attempt sent after %s: %w", t0.Format(time.RFC3339Nano), err)
	}
	if !maxSent.Valid {
		return nil, n, nil
	}
	at := maxSent.Time
	return &at, n, nil
}

// RealReadAfter 返回 [T0, until) 内发出且 flag_read_at >= T0 的 REAL 尝试条数，期望 0。较 §6.9 定版 SQL 收紧为 sent_at >= ?
// （与 T0 同刻发出也算），并加 until 上界（同 RealSentAfter）；跨农场走 idx_mode_sent。
func (r KillSwitchRepo) RealReadAfter(ctx context.Context, sys SystemScope, t0 time.Time, until *time.Time) (int64, error) {
	if err := requireSystem(sys); err != nil {
		return 0, err
	}
	var n int64
	err := r.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM dispatch_attempt WHERE mode='REAL' AND sent_at >= ? AND flag_read_at >= ? AND (? IS NULL OR sent_at < ?)`,
		t0, t0, until, until).Row().Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("repository: REAL dispatch_attempt read flag after %s: %w", t0.Format(time.RFC3339Nano), err)
	}
	return n, nil
}

// HeldSince 返回农场 (T0, until) 内的 DISPATCH_HELD 审计条数（§6.9 定版 SQL 加 until 上界，按灰度农场逐个查）。
func (r KillSwitchRepo) HeldSince(ctx context.Context, tenantID, farmID uint64, t0 time.Time, until *time.Time) (int64, error) {
	if err := requireFarm(tenantID, farmID); err != nil {
		return 0, err
	}
	var n int64
	err := r.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM audit_log WHERE tenant_id=? AND farm_id=? AND action='DISPATCH_HELD' AND created_at > ? AND (? IS NULL OR created_at < ?)`,
		tenantID, farmID, t0, until, until).Row().Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("repository: DISPATCH_HELD farm (%d,%d) after %s: %w", tenantID, farmID,
			t0.Format(time.RFC3339Nano), err)
	}
	return n, nil
}
