package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// 审计链读取（auditctl verify / anchor-check、每日锚点、审计导出）：只读，按 (tenant_id, farm_id) 一条链读。

// AuditChainRepo 读 audit_chain_head 与带链字段的 audit_log 行。DB 可以是只读连接。
type AuditChainRepo struct{ DB *gorm.DB }

// InitChainHeadTx 在调用方事务内预建链 k 的 head（农场开通 / 种子 / cmd/migrate；INSERT IGNORE 幂等）。
// k 允许 (0,0)：平台 / 未解析农场的拒绝类审计链。
func InitChainHeadTx(ctx context.Context, tx *gorm.DB, k ChainKey, now time.Time) error {
	if _, err := execInTx(ctx, tx, InitChainHeadSQL, k.TenantID, k.FarmID, now.UTC().Truncate(time.Millisecond)); err != nil {
		return fmt.Errorf("repository: init audit_chain_head (%d,%d): %w", k.TenantID, k.FarmID, err)
	}
	return nil
}

// InitChainHeads 为 (0,0)、存量农场（farm_profile ∪ user_role 的 (tenant_id, farm_id)）与各租户级链 (tenant_id, 0)
// 预建 head（cmd/migrate up；幂等）。返回新建条数。迁移期系统级一次性操作，不经 SystemScope。
func InitChainHeads(ctx context.Context, db *gorm.DB, now time.Time) (int64, error) {
	ts := now.UTC().Truncate(time.Millisecond)
	var total int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		total = 0
		if err := InitChainHeadTx(ctx, tx, ChainKey{}, ts); err != nil {
			return err
		}
		res := tx.Exec(`INSERT IGNORE INTO audit_chain_head (tenant_id, farm_id, chain_seq, head_hash, updated_at)
SELECT tenant_id, farm_id, 0, REPEAT('0',64), ? FROM (SELECT tenant_id, farm_id FROM farm_profile UNION SELECT tenant_id, farm_id FROM user_role
UNION SELECT tenant_id, 0 FROM farm_profile) k`, ts)
		if res.Error != nil {
			return fmt.Errorf("repository: init audit_chain_head for existing farms: %w", res.Error)
		}
		total = res.RowsAffected
		return nil
	})
	return total, err
}

// Head 读一条链的 head；未预建返回 ErrAuditChainHeadMissing。
func (r AuditChainRepo) Head(ctx context.Context, k ChainKey) (ChainHead, error) {
	h := ChainHead{Key: k}
	var row struct {
		ChainSeq uint64
		HeadHash string
	}
	res := r.DB.WithContext(ctx).Raw(`SELECT chain_seq, head_hash FROM audit_chain_head WHERE tenant_id=? AND farm_id=?`,
		k.TenantID, k.FarmID).Scan(&row)
	if res.Error != nil {
		return h, fmt.Errorf("repository: read audit_chain_head (%d,%d): %w", k.TenantID, k.FarmID, res.Error)
	}
	if res.RowsAffected == 0 {
		return h, fmt.Errorf("%w: (%d,%d)", ErrAuditChainHeadMissing, k.TenantID, k.FarmID)
	}
	h.ChainSeq, h.HeadHash = row.ChainSeq, row.HeadHash
	return h, nil
}

// Heads 读全部链的 head（每日锚点；跨农场，须持 SystemScope；表行数 = 农场数 + 1）。
func (r AuditChainRepo) Heads(ctx context.Context, sys SystemScope) ([]ChainHead, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []struct {
		TenantID, FarmID, ChainSeq uint64
		HeadHash                   string
	}
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT tenant_id, farm_id, chain_seq, head_hash FROM audit_chain_head ORDER BY tenant_id, farm_id`).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list audit_chain_head (%s): %w", sys.Task(), err)
	}
	out := make([]ChainHead, 0, len(rows))
	for _, x := range rows {
		out = append(out, ChainHead{Key: ChainKey{x.TenantID, x.FarmID}, ChainSeq: x.ChainSeq, HeadHash: x.HeadHash})
	}
	return out, nil
}

const chainRowCols = `id, created_at, tenant_id, farm_id, run_id, actor_type, actor_id, action, target, payload_digest, summary, result, raw_object_id, chain_seq, prev_hash, row_hash`

// Rows 照抄 §6.8 校验读取：链 k 上 chain_seq ∈ [from, to] 的行，按 chain_seq 升序（历史 chain_seq IS NULL 行不在结果内）。
// 调用方按页推进 from（流式校验），单页行数由区间宽度控制。
func (r AuditChainRepo) Rows(ctx context.Context, k ChainKey, from, to uint64) ([]ChainRow, error) {
	rows, err := r.DB.WithContext(ctx).Raw(`SELECT `+chainRowCols+` FROM audit_log WHERE tenant_id=? AND farm_id=? AND chain_seq BETWEEN ? AND ? ORDER BY chain_seq`,
		k.TenantID, k.FarmID, from, to).Rows()
	if err != nil {
		return nil, fmt.Errorf("repository: read audit chain (%d,%d) [%d,%d]: %w", k.TenantID, k.FarmID, from, to, err)
	}
	out, scanErr := scanChainRows(rows)
	if err := errors.Join(scanErr, rows.Close()); err != nil {
		return nil, fmt.Errorf("repository: scan audit chain (%d,%d): %w", k.TenantID, k.FarmID, err)
	}
	return out, nil
}

// SeqRange 返回链 k 上 created_at ∈ [from, to) 的已封链行的 chain_seq 最小 / 最大值（审计导出取区间）；ok=false 表示区间内无行。
func (r AuditChainRepo) SeqRange(ctx context.Context, k ChainKey, from, to time.Time) (lo, hi uint64, ok bool, err error) {
	var row struct{ Lo, Hi sql.NullInt64 }
	if err := r.DB.WithContext(ctx).Raw(`SELECT MIN(chain_seq) AS lo, MAX(chain_seq) AS hi FROM audit_log WHERE tenant_id=? AND farm_id=? AND created_at >= ? AND created_at < ? AND chain_seq IS NOT NULL`,
		k.TenantID, k.FarmID, from, to).Scan(&row).Error; err != nil {
		return 0, 0, false, fmt.Errorf("repository: audit seq range (%d,%d): %w", k.TenantID, k.FarmID, err)
	}
	if !row.Lo.Valid || !row.Hi.Valid {
		return 0, 0, false, nil
	}
	return uint64(row.Lo.Int64), uint64(row.Hi.Int64), true, nil
}

// SeqBefore 返回链 k 上 created_at < t 的已封链行的最大 chain_seq（无行返回 0）；导出空窗口时用它定位窗口前一行。
func (r AuditChainRepo) SeqBefore(ctx context.Context, k ChainKey, t time.Time) (uint64, error) {
	var row struct{ Seq sql.NullInt64 }
	if err := r.DB.WithContext(ctx).Raw(`SELECT MAX(chain_seq) AS seq FROM audit_log WHERE tenant_id=? AND farm_id=? AND created_at < ? AND chain_seq IS NOT NULL`,
		k.TenantID, k.FarmID, t).Scan(&row).Error; err != nil {
		return 0, fmt.Errorf("repository: audit seq before (%d,%d): %w", k.TenantID, k.FarmID, err)
	}
	if !row.Seq.Valid {
		return 0, nil
	}
	return uint64(row.Seq.Int64), nil
}

func scanChainRows(rows *sql.Rows) ([]ChainRow, error) {
	var out []ChainRow
	for rows.Next() {
		var (
			c                ChainRow
			actorType        string
			target, summary  sql.NullString
			runID, rawObject sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &c.Rec.CreatedAt, &c.Rec.TenantID, &c.Rec.FarmID, &runID, &actorType, &c.Rec.Actor.ID,
			&c.Rec.Action, &target, &c.Rec.PayloadDigest, &summary, &c.Rec.Result, &rawObject,
			&c.ChainSeq, &c.PrevHash, &c.RowHash); err != nil {
			return nil, err
		}
		c.Rec.Actor.Type = model.ActorType(actorType)
		c.Rec.Target = target.String
		if summary.Valid {
			c.Rec.Summary = []byte(summary.String)
		}
		c.Rec.RunID = optU64(runID)
		c.Rec.RawObjectID = optU64(rawObject)
		out = append(out, c)
	}
	return out, rows.Err()
}

func optU64(v sql.NullInt64) *uint64 {
	if !v.Valid {
		return nil
	}
	u := uint64(v.Int64)
	return &u
}
