package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// AttributionRepo 是 P2 L8 归因周报的读写：逐农场取上周负向信号（农场 Scope），attribution_issue 平台行（SystemScope，
// tenant_id=0, farm_id=0）。upsert 照抄 §6.9 ②；其余查询自拟，均带 tenant_id / farm_id 条件。
type AttributionRepo struct{ DB *gorm.DB }

// ExpertWrongMaxScore 是「专家判错」的专家分上限（online_judge_result 1-3 分，1 分即判错）。
const ExpertWrongMaxScore = 1

// primaryKBSQL 是建议卡 c 的主 kb_id：seq 最小的 kb 引用（advice_citation uk_advice_seq 前缀）。
const primaryKBSQL = `(SELECT a.kb_id FROM advice_citation a WHERE a.tenant_id = c.tenant_id AND a.farm_id = c.farm_id
  AND a.advice_id = c.id AND a.type = 'kb' AND a.kb_id IS NOT NULL ORDER BY a.seq LIMIT 1)`

// weekHitsSQL 是单农场 [from, to) 的五类信号。驳回 / 差评 / 拒答卡按 created_at 过滤（rejection_reason、advice_feedback、
// advice_card 的 idx_scope_time）；专家判错按回写时刻 updated_at 过滤（online_judge_result 无 updated_at 索引，
// 走 uk_advice_dim 的 (tenant_id, farm_id) 前缀），并按 advice_id 去重：一张卡的多个 dimension × judge_version 判错
// 只算一次命中（source_id 取最小 id，命中时刻取最早回写）。
// 建议卡缺失时问题类型记 unknown、主 kb 为空。VALIDATION_FAILED / KB_MISS 取 advice_card 拒答卡（与 L3 日报同口径）。
const weekHitsSQL = `SELECT 'rejection' AS source, r.id AS source_id, r.run_id, r.reason_code AS reason,
  COALESCE(c.category, 'unknown') AS question_type, ` + primaryKBSQL + ` AS primary_kb, r.created_at AS hit_at
FROM rejection_reason r LEFT JOIN advice_card c ON c.tenant_id = r.tenant_id AND c.farm_id = r.farm_id AND c.id = r.advice_id
WHERE r.tenant_id = ? AND r.farm_id = ? AND r.created_at >= ? AND r.created_at < ?
UNION ALL
SELECT 'feedback', f.id, f.run_id, f.category, COALESCE(c.category, 'unknown'), ` + primaryKBSQL + `, f.created_at
FROM advice_feedback f LEFT JOIN advice_card c ON c.tenant_id = f.tenant_id AND c.farm_id = f.farm_id AND c.id = f.advice_id
WHERE f.tenant_id = ? AND f.farm_id = ? AND f.created_at >= ? AND f.created_at < ? AND f.verdict = 'DISLIKE' AND f.category IS NOT NULL
UNION ALL
SELECT IF(c.refuse_reason = 'KB_MISS', 'kb_miss', 'validation'), c.id, c.run_id,
  IF(c.refuse_reason = 'KB_MISS', 'KB_MISS', 'GUARDRAIL'), c.category, ` + primaryKBSQL + `, c.created_at
FROM advice_card c
WHERE c.tenant_id = ? AND c.farm_id = ? AND c.created_at >= ? AND c.created_at < ? AND c.outcome = 'REFUSED'
  AND c.refuse_reason IN ('VALIDATION_FAILED', 'KB_MISS')
UNION ALL
SELECT 'expert', j.id, j.run_id, 'EXPERT_WRONG', j.category, ` + primaryKBSQL + `, j.hit_at
FROM (SELECT tenant_id, farm_id, advice_id, MIN(id) AS id, MIN(run_id) AS run_id, MIN(category) AS category,
    MIN(updated_at) AS hit_at
  FROM online_judge_result
  WHERE tenant_id = ? AND farm_id = ? AND status = 'REVIEWED' AND expert_score <= ? AND updated_at >= ? AND updated_at < ?
  GROUP BY tenant_id, farm_id, advice_id) j
LEFT JOIN advice_card c ON c.tenant_id = j.tenant_id AND c.farm_id = j.farm_id AND c.id = j.advice_id`

// WeekHits 返回单农场 [from, to) 的负向信号（驳回、差评、护栏拦截、KB_MISS 拒答、专家判错）。
func (r AttributionRepo) WeekHits(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.AttributionHit, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	t, f, a, b := scope.TenantID, scope.FarmID, from.UTC(), to.UTC()
	var rows []struct {
		Source, Reason, QuestionType string
		SourceID, RunID              uint64
		PrimaryKb                    *string
		HitAt                        time.Time
	}
	if err := r.DB.WithContext(ctx).Raw(weekHitsSQL, t, f, a, b, t, f, a, b, t, f, a, b, t, f, ExpertWrongMaxScore, a, b).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: attribution hits farm %d: %w", f, err)
	}
	out := make([]model.AttributionHit, 0, len(rows))
	for _, row := range rows {
		h := model.AttributionHit{TenantID: t, FarmID: f, Reason: row.Reason, QuestionType: row.QuestionType, RunID: row.RunID,
			Source: row.Source, SourceID: row.SourceID, At: row.HitAt}
		if row.PrimaryKb != nil {
			h.PrimaryKB = *row.PrimaryKb
		}
		out = append(out, h)
	}
	return out, nil
}

// issueColumns 是 attribution_issue 的读取列。
const issueColumns = `id, signature, first_week, last_week, reason, question_type, primary_kb_id, hit_count, evidence_refs,
  disposition, owner_id, status, reopen_count, resolution_ref, row_ver, created_at, updated_at`

type issueRow struct {
	ID                             uint64
	Signature, FirstWeek, LastWeek string
	Reason, QuestionType, Status   string
	PrimaryKbID, Disposition       *string
	ResolutionRef                  *string
	HitCount, ReopenCount, RowVer  int
	EvidenceRefs                   []byte
	OwnerID                        *uint64
	CreatedAt, UpdatedAt           time.Time
}

// issues 读平台问题行（tenant_id=0, farm_id=0），where 为追加条件（固定片段，不拼外部输入）。
func (r AttributionRepo) issues(ctx context.Context, _ SystemScope, where string, args ...any) ([]model.AttributionIssue, error) {
	var rows []issueRow
	if err := r.DB.WithContext(ctx).Raw(`SELECT `+issueColumns+` FROM attribution_issue WHERE tenant_id = 0 AND farm_id = 0 AND `+where,
		args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list attribution_issue: %w", err)
	}
	out := make([]model.AttributionIssue, 0, len(rows))
	for _, row := range rows {
		i := model.AttributionIssue{ID: row.ID, Signature: row.Signature, FirstWeek: row.FirstWeek, LastWeek: row.LastWeek,
			Reason: row.Reason, QuestionType: row.QuestionType, PrimaryKB: row.PrimaryKbID, HitCount: row.HitCount,
			Disposition: row.Disposition, OwnerID: row.OwnerID, Status: row.Status, ReopenCount: row.ReopenCount,
			ResolutionRef: row.ResolutionRef, RowVer: row.RowVer, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
		if err := json.Unmarshal(row.EvidenceRefs, &i.Evidence); err != nil {
			return nil, fmt.Errorf("repository: decode attribution_issue %d evidence_refs: %w", row.ID, err)
		}
		out = append(out, i)
	}
	return out, nil
}

// IssuesBySignature 读一批签名的平台问题行（系统级）。
func (r AttributionRepo) IssuesBySignature(ctx context.Context, sys SystemScope, sigs []string) ([]model.AttributionIssue, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	if len(sigs) == 0 {
		return nil, nil
	}
	return r.issues(ctx, sys, `signature IN (?) ORDER BY signature`, sigs)
}

// WeekIssues 读 last_week = week 的平台问题行（按本周命中数降序、签名升序，最多 limit 行，系统级）。
func (r AttributionRepo) WeekIssues(ctx context.Context, sys SystemScope, week string, limit int) ([]model.AttributionIssue, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	return r.issues(ctx, sys, `last_week = ? ORDER BY hit_count DESC, signature LIMIT ?`, week, limit)
}

// upsertIssueSQL 照抄 §6.9 ②（省略号处补全列）：复发重开时 reopen_count 必须写在 status 之前（MySQL 按书写顺序求值 SET）。
const upsertIssueSQL = `INSERT INTO attribution_issue (tenant_id, farm_id, signature, first_week, last_week, reason, question_type,
  primary_kb_id, hit_count, evidence_refs, status, reopen_count, row_ver, created_at, updated_at)
VALUES (0, 0, ?, ?, ?, ?, ?, ?, ?, ?, 'OPEN', 0, 0, ?, ?) ON DUPLICATE KEY UPDATE
  last_week = VALUES(last_week), hit_count = VALUES(hit_count), evidence_refs = VALUES(evidence_refs),
  reopen_count = reopen_count + IF(status IN ('RESOLVED', 'VERIFIED'), 1, 0),
  status = IF(status IN ('RESOLVED', 'VERIFIED'), 'OPEN', status),
  row_ver = row_ver + 1, updated_at = VALUES(updated_at)`

// UpsertIssues 在一个事务内 upsert 一批平台问题行（系统级）；evidence 由调用方与旧值合并好。
func (r AttributionRepo) UpsertIssues(ctx context.Context, sys SystemScope, rows []model.IssueUpsert, at time.Time) error {
	if err := requireSystem(sys); err != nil {
		return err
	}
	return r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, u := range rows {
			ev, err := json.Marshal(u.Evidence)
			if err != nil {
				return fmt.Errorf("repository: encode evidence_refs %s: %w", u.Signature, err)
			}
			var kb any
			if u.PrimaryKB != "" {
				kb = u.PrimaryKB
			}
			if _, err := execInTx(ctx, tx, upsertIssueSQL, u.Signature, u.Week, u.Week, u.Reason, u.QuestionType, kb, u.HitCount,
				string(ev), at.UTC(), at.UTC()); err != nil {
				return fmt.Errorf("repository: upsert attribution_issue %s: %w", u.Signature, err)
			}
		}
		return nil
	})
}

// PatchIssue 是处置更新（平台行，系统级）：nil 字段不改，带 row_ver 乐观锁。行不存在 → model.ErrNotFound，
// row_ver 不符 → model.ErrConcurrentUpdate。返回更新后的行。文档未给，自拟：主键 + 平台作用域 + row_ver。
func (r AttributionRepo) PatchIssue(ctx context.Context, sys SystemScope, id uint64, p model.AttributionPatch,
	at time.Time) (model.AttributionIssue, error) {
	if err := requireSystem(sys); err != nil {
		return model.AttributionIssue{}, err
	}
	if p.RowVer == nil {
		return model.AttributionIssue{}, fmt.Errorf("repository: patch attribution_issue %d: %w", id, model.ErrAttributionPatch)
	}
	res := r.DB.WithContext(ctx).Exec(`UPDATE attribution_issue SET disposition = COALESCE(?, disposition),
  owner_id = COALESCE(?, owner_id), resolution_ref = COALESCE(?, resolution_ref), status = COALESCE(?, status),
  row_ver = row_ver + 1, updated_at = ?
WHERE tenant_id = 0 AND farm_id = 0 AND id = ? AND row_ver = ?`, p.Disposition, p.OwnerID, p.ResolutionRef, p.Status,
		at.UTC(), id, *p.RowVer)
	if res.Error != nil {
		return model.AttributionIssue{}, fmt.Errorf("repository: patch attribution_issue %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		if _, err := r.issue(ctx, sys, id); err != nil { // 不存在 → ErrNotFound
			return model.AttributionIssue{}, err
		}
		return model.AttributionIssue{}, fmt.Errorf("repository: patch attribution_issue %d row_ver %d: %w", id, *p.RowVer,
			model.ErrConcurrentUpdate)
	}
	return r.issue(ctx, sys, id)
}

func (r AttributionRepo) issue(ctx context.Context, sys SystemScope, id uint64) (model.AttributionIssue, error) {
	got, err := r.issues(ctx, sys, `id = ?`, id)
	if err != nil {
		return model.AttributionIssue{}, err
	}
	if len(got) == 0 {
		return model.AttributionIssue{}, fmt.Errorf("repository: attribution_issue %d: %w", id, model.ErrNotFound)
	}
	return got[0], nil
}

// BackflowCounts 是单农场 [from, to) 内回流状态推进到各状态的 run 数（按 run 去重）。
// 口径取 audit_log 的 BACKFLOW 状态事件时刻（idx_action），不用 agent_run.updated_at：后者会被其他写入改动、
// 且只留最后一次推进的时刻，跨周会错位。RETRY 是失败尝试，不是状态，不计。
func (r AttributionRepo) BackflowCounts(ctx context.Context, scope model.Scope, from, to time.Time) (map[string]int, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []struct {
		BackflowStatus string
		N              int
	}
	if err := r.DB.WithContext(ctx).Raw(`SELECT JSON_UNQUOTE(JSON_EXTRACT(summary, '$.status')) AS backflow_status,
  COUNT(DISTINCT run_id) AS n FROM audit_log
WHERE tenant_id = ? AND farm_id = ? AND action = ? AND created_at >= ? AND created_at < ?
  AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.status')) <> ?
GROUP BY backflow_status`, scope.TenantID, scope.FarmID, string(model.AuditBackflow), from.UTC(), to.UTC(),
		model.BackflowRetry).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: backflow counts farm %d: %w", scope.FarmID, err)
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.BackflowStatus] = row.N
	}
	return out, nil
}
