package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// ErrJudgeRowConflict 表示专家回写时 row_ver 已变（他人先回写或行不存在）：乐观锁冲突，调用方重读后再提交。
var ErrJudgeRowConflict = errors.New("repository: online_judge_result row_ver conflict")

// AdviceCategories 是单农场一日出卡（outcome=ADVICE）的 category（升序，按 category 定额抽样用）。
func (r QualityRepo) AdviceCategories(ctx context.Context, scope model.Scope, from, to time.Time) ([]string, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var cats []string
	if err := r.DB.WithContext(ctx).Raw(`SELECT DISTINCT category FROM advice_card
WHERE tenant_id = ? AND farm_id = ? AND outcome = 'ADVICE' AND created_at >= ? AND created_at < ? ORDER BY category`,
		scope.TenantID, scope.FarmID, from.UTC(), to.UTC()).Scan(&cats).Error; err != nil {
		return nil, fmt.Errorf("repository: advice categories farm %d: %w", scope.FarmID, err)
	}
	return cats, nil
}

// AdviceRef 是抽中的一张卡。
type AdviceRef struct {
	ID    uint64
	RunID uint64
}

// SampleAdvice 按 category 定额抽样（SQL 照抄 §6.4 ②）：同盐同数据抽到同一批卡（ORDER BY CRC32(CONCAT(id, salt))）。
func (r QualityRepo) SampleAdvice(ctx context.Context, scope model.Scope, category string, from, to time.Time, salt string,
	limit int) ([]AdviceRef, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []AdviceRef
	if err := r.DB.WithContext(ctx).Raw(`SELECT id, run_id FROM advice_card
WHERE tenant_id = ? AND farm_id = ? AND category = ? AND outcome = 'ADVICE' AND created_at >= ? AND created_at < ?
ORDER BY CRC32(CONCAT(id, ?)) LIMIT ?`, scope.TenantID, scope.FarmID, category, from.UTC(), to.UTC(), salt, limit).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: sample advice farm %d %s: %w", scope.FarmID, category, err)
	}
	return rows, nil
}

// RunKnowledgeRef 是 run 出卡时用的知识版本（agent_run.knowledge_ref）：线上 judge 按它读所引条目原文。
func (r QualityRepo) RunKnowledgeRef(ctx context.Context, scope model.Scope, runID uint64) (string, error) {
	if err := requireScope(scope); err != nil {
		return "", err
	}
	var refs []string
	if err := r.DB.WithContext(ctx).Raw(`SELECT knowledge_ref FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ?`,
		scope.TenantID, scope.FarmID, runID).Scan(&refs).Error; err != nil {
		return "", fmt.Errorf("repository: run %d knowledge_ref: %w", runID, err)
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("repository: run %d: %w", runID, model.ErrNotFound)
	}
	return refs[0], nil
}

// InsertJudgeResult 写一条线上 judge 结果；uk_advice_dim 冲突时不覆盖（补跑不重复评），inserted 为 false。
func (r QualityRepo) InsertJudgeResult(ctx context.Context, scope model.Scope, j model.OnlineJudgeResult, at time.Time) (bool, error) {
	if err := checkScope(scope, j.TenantID, j.FarmID); err != nil {
		return false, err
	}
	res := r.DB.WithContext(ctx).Exec(`INSERT INTO online_judge_result (tenant_id, farm_id, run_id, advice_id, stat_date, category, dimension, judge_version, judge_score, status, row_ver, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE id = id`,
		scope.TenantID, scope.FarmID, j.RunID, j.AdviceID, j.StatDate, j.Category, j.Dimension, j.JudgeVersion, j.JudgeScore,
		j.Status, at.UTC(), at.UTC())
	if res.Error != nil {
		return false, fmt.Errorf("repository: insert online_judge_result advice %d: %w", j.AdviceID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

type judgeRow struct {
	ID           uint64
	RunID        uint64
	AdviceID     uint64
	StatDate     time.Time
	Category     string
	Dimension    string
	JudgeVersion string
	JudgeScore   *int
	ExpertScore  *int
	ExpertID     *uint64
	Status       string
	RowVer       int
}

// JudgeResults 读单农场一个业务日某维度的 judge 结果（按 id 升序）。
func (r QualityRepo) JudgeResults(ctx context.Context, scope model.Scope, statDate, dimension string) ([]model.OnlineJudgeResult, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []judgeRow
	if err := r.DB.WithContext(ctx).Raw(`SELECT id, run_id, advice_id, stat_date, category, dimension, judge_version, judge_score, expert_score, expert_id, status, row_ver
FROM online_judge_result WHERE tenant_id = ? AND farm_id = ? AND stat_date = ? AND dimension = ? ORDER BY id`,
		scope.TenantID, scope.FarmID, statDate, dimension).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list online_judge_result farm %d %s: %w", scope.FarmID, statDate, err)
	}
	out := make([]model.OnlineJudgeResult, 0, len(rows))
	for _, j := range rows {
		o := model.OnlineJudgeResult{ID: j.ID, TenantID: scope.TenantID, FarmID: scope.FarmID, RunID: j.RunID,
			AdviceID: j.AdviceID, StatDate: j.StatDate.Format(time.DateOnly), Category: j.Category, Dimension: j.Dimension,
			JudgeVersion: j.JudgeVersion, JudgeScore: j.JudgeScore, ExpertScore: j.ExpertScore, Status: j.Status, RowVer: j.RowVer}
		if j.ExpertID != nil {
			o.ExpertID = *j.ExpertID
		}
		out = append(out, o)
	}
	return out, nil
}

// ReviewJudgeResult 是专家回写（SQL 照抄 §6.4 ②，乐观锁）：row_ver 不符或行不存在返回 ErrJudgeRowConflict。
func (r QualityRepo) ReviewJudgeResult(ctx context.Context, scope model.Scope, id uint64, rowVer, score int, expertID uint64,
	at time.Time) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	res := r.DB.WithContext(ctx).Exec(`UPDATE online_judge_result SET expert_score = ?, expert_id = ?, status = 'REVIEWED', row_ver = row_ver + 1, updated_at = ?
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND row_ver = ?`, score, expertID, at.UTC(), scope.TenantID, scope.FarmID, id, rowVer)
	if res.Error != nil {
		return fmt.Errorf("repository: review online_judge_result %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id %d row_ver %d", ErrJudgeRowConflict, id, rowVer)
	}
	return nil
}

// JudgeTally 是一日某维度、某 judge 版本的结果计数：Scored 为有分（专家已回写取专家分，否则 judge 分）的行，
// Top 为其中等于 top 分的行，Unknown 为无分（UNKNOWN 且专家未回写）的行。
type JudgeTally struct {
	Scored  int
	Top     int
	Unknown int
}

// judgeTallySQL 按「专家分优先」计数；子查询 WHERE 由调用方补农场条件并闭合括号。
const judgeTallySQL = `SELECT COALESCE(SUM(s IS NOT NULL), 0) AS scored, COALESCE(SUM(s = ?), 0) AS top, COALESCE(SUM(s IS NULL), 0) AS unknown
FROM (SELECT IF(status = 'REVIEWED' AND expert_score IS NOT NULL, expert_score, judge_score) AS s
  FROM online_judge_result WHERE stat_date = ? AND dimension = ? AND judge_version = ?`

// FarmJudgeTally 是单农场一日的 judge 结果计数。
func (r QualityRepo) FarmJudgeTally(ctx context.Context, scope model.Scope, statDate, dimension, version string, top int) (JudgeTally, error) {
	if err := requireScope(scope); err != nil {
		return JudgeTally{}, err
	}
	var t JudgeTally
	if err := r.DB.WithContext(ctx).Raw(judgeTallySQL+` AND tenant_id = ? AND farm_id = ?) j`,
		top, statDate, dimension, version, scope.TenantID, scope.FarmID).Scan(&t).Error; err != nil {
		return JudgeTally{}, fmt.Errorf("repository: judge tally farm %d %s: %w", scope.FarmID, statDate, err)
	}
	return t, nil
}

// PlatformJudgeTally 是全部农场一日的 judge 结果计数（平台汇总行用，系统级）。
func (r QualityRepo) PlatformJudgeTally(ctx context.Context, sys SystemScope, statDate, dimension, version string, top int) (JudgeTally, error) {
	if err := requireSystem(sys); err != nil {
		return JudgeTally{}, err
	}
	var t JudgeTally
	if err := r.DB.WithContext(ctx).Raw(judgeTallySQL+`) j`, top, statDate, dimension, version).Scan(&t).Error; err != nil {
		return JudgeTally{}, fmt.Errorf("repository: platform judge tally %s: %w", statDate, err)
	}
	return t, nil
}
