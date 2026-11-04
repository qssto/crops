package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StatsScope 是统计查询的范围：任务类型、维度、judge 版本；From / To 非零时只取 created_at 落在 [From, To) 的任务
// （月度复核按月统计，L2 评审 P2-8）。
type StatsScope struct {
	TaskType, Dimension, JudgeVersion string
	From, To                          time.Time
}

// where 返回别名 alias 下的过滤条件与参数（主标 / 双标由调用方另加）。
func (s StatsScope) where(alias string) (string, []any) {
	q := alias + ".task_type = ? AND " + alias + ".dimension = ? AND " + alias + ".judge_version = ?"
	args := []any{s.TaskType, s.Dimension, s.JudgeVersion}
	if !s.From.IsZero() {
		q += " AND " + alias + ".created_at >= ?"
		args = append(args, s.From)
	}
	if !s.To.IsZero() {
		q += " AND " + alias + ".created_at < ?"
		args = append(args, s.To)
	}
	return q, args
}

// ScorePair 是混淆矩阵的一格：(左分, 右分) 与次数。
type ScorePair struct {
	A, B  int
	Count int
}

// JudgeExpertMatrix 是 judge 与专家的混淆矩阵（排除 UNKNOWN，只取主标）：Pair.A = judge_score，Pair.B = expert_score。
func (r *Reviews) JudgeExpertMatrix(ctx context.Context, sc StatsScope) ([]ScorePair, error) {
	var rows []ScorePair
	cond, args := sc.where("t")
	err := r.db.WithContext(ctx).Raw(`SELECT t.judge_score AS a, t.expert_score AS b, COUNT(*) AS count FROM review_task t
WHERE `+cond+` AND t.status = 'DONE' AND t.rater_no = 1 AND t.judge_score IS NOT NULL
GROUP BY t.judge_score, t.expert_score`, args...).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: judge-expert matrix: %w", err)
	}
	return rows, nil
}

// ExpertMatrix 是专家间混淆矩阵（可达上限）：Pair.A = 主标 expert_score，Pair.B = 双标 expert_score；
// 同一专家的配对不算（a.assignee_id <> b.assignee_id，NULL 也不算）。
func (r *Reviews) ExpertMatrix(ctx context.Context, sc StatsScope) ([]ScorePair, error) {
	var rows []ScorePair
	cond, args := sc.where("a")
	err := r.db.WithContext(ctx).Raw(`SELECT a.expert_score AS a, b.expert_score AS b, COUNT(*) AS count
FROM review_task a JOIN review_task b
  ON b.experiment_id = a.experiment_id AND b.sample_id = a.sample_id AND b.trial_index = a.trial_index
 AND b.task_type = a.task_type AND b.dimension = a.dimension AND b.judge_version = a.judge_version
 AND b.rater_no = 2 AND b.status = 'DONE' AND a.assignee_id <> b.assignee_id
WHERE `+cond+` AND a.rater_no = 1 AND a.status = 'DONE'
GROUP BY a.expert_score, b.expert_score`, args...).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: expert matrix: %w", err)
	}
	return rows, nil
}

// UnknownRatio 是主标任务中 judge 返回 UNKNOWN（judge_score IS NULL）的占比；没有任务时 ok = false。
func (r *Reviews) UnknownRatio(ctx context.Context, sc StatsScope) (ratio float64, ok bool, err error) {
	var v sql.NullFloat64
	cond, args := sc.where("t")
	err = r.db.WithContext(ctx).Raw(`SELECT AVG(t.judge_score IS NULL) FROM review_task t
WHERE `+cond+` AND t.rater_no = 1`, args...).Row().Scan(&v)
	if err != nil {
		return 0, false, fmt.Errorf("store: unknown ratio: %w", err)
	}
	return v.Float64, v.Valid, nil
}
