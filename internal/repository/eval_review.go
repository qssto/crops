package repository

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// EvalReviewRepo 读写评测库 agri_eval 的 review_task，只读 trial（P2-L2b 专家评审）。DB 须是 eval_review 独立账号
// 连的评测库（最小授权：review_task SELECT / INSERT / UPDATE，trial SELECT）。评测库无 tenant / farm 维度，
// 方法一律持 SystemScope（平台评审 / 入队任务，由 app 构造）。表结构见 P2 §6.3 SQL，不加迁移。
type EvalReviewRepo struct{ DB *gorm.DB }

// reviewSelect 是带 holdout 判定的 review_task 查询列（别名 rt）。holdout 失败关闭：任务挂了样本（experiment_id 或
// sample_id 非空）时，只有找到同 (experiment_id, sample_id, trial_index) 的 trial 且 suite 都不是 holdout 才判为非 holdout；
// 找不到 trial、或任一列为 NULL，一律按 holdout 处理。
const reviewSelect = `SELECT rt.id, rt.task_type, rt.queue_reason, rt.experiment_id, rt.sample_id, rt.trial_index, rt.draft_ref,
  rt.dimension, rt.judge_version, rt.judge_score, rt.rater_no, rt.expert_score, rt.assignee_id, rt.status, rt.ref_key,
  rt.created_at, rt.done_at,
  CASE WHEN rt.experiment_id IS NULL AND rt.sample_id IS NULL THEN 0
    ELSE COALESCE((SELECT MAX(tr.suite = 'holdout') FROM trial tr WHERE tr.experiment_id = rt.experiment_id
      AND tr.sample_id = rt.sample_id AND tr.trial_index = rt.trial_index), 1) END AS holdout
FROM review_task rt`

// evalReviewBatch 是一条入队 INSERT 的最大行数。
const evalReviewBatch = 500

// reviewTaskRow 是 review_task 的扫描行（可空列用指针）。
type reviewTaskRow struct {
	ID           uint64
	TaskType     string
	QueueReason  *string
	ExperimentID *uint64
	SampleID     *string
	TrialIndex   *int
	DraftRef     *string
	Dimension    *string
	JudgeVersion *string
	JudgeScore   *int
	RaterNo      int
	ExpertScore  *int
	AssigneeID   *uint64
	Status       string
	RefKey       string
	CreatedAt    time.Time
	DoneAt       *time.Time
	Holdout      int
}

func (w reviewTaskRow) task() model.ReviewTask {
	return model.ReviewTask{ID: w.ID, TaskType: w.TaskType, QueueReason: derefStr(w.QueueReason), ExperimentID: w.ExperimentID,
		SampleID: derefStr(w.SampleID), TrialIndex: w.TrialIndex, DraftRef: derefStr(w.DraftRef), Dimension: derefStr(w.Dimension),
		JudgeVersion: derefStr(w.JudgeVersion), JudgeScore: w.JudgeScore, RaterNo: w.RaterNo, ExpertScore: w.ExpertScore,
		AssigneeID: w.AssigneeID, Status: w.Status, RefKey: w.RefKey, CreatedAt: w.CreatedAt, DoneAt: w.DoneAt,
		Holdout: w.Holdout != 0}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ListReviewTasks 按条件分页列任务（id 升序），并返回符合条件的总数。Statuses 为空取 model.ReviewPending。
func (r EvalReviewRepo) ListReviewTasks(ctx context.Context, sys SystemScope, f model.ReviewTaskFilter) ([]model.ReviewTask, int64, error) {
	if err := requireSystem(sys); err != nil {
		return nil, 0, err
	}
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = model.ReviewPending
	}
	cond, args := " WHERE rt.status IN ?", []any{statuses}
	if f.TaskType != "" {
		cond += " AND rt.task_type = ?"
		args = append(args, f.TaskType)
	}
	db := r.DB.WithContext(ctx)
	var total int64
	if err := db.Raw("SELECT COUNT(*) FROM review_task rt"+cond, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repository: count review tasks: %w", err)
	}
	var rows []reviewTaskRow
	if err := db.Raw(reviewSelect+cond+" ORDER BY rt.id LIMIT ? OFFSET ?", append(args, f.Limit, f.Offset)...).
		Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("repository: list review tasks: %w", err)
	}
	out := make([]model.ReviewTask, 0, len(rows))
	for _, w := range rows {
		out = append(out, w.task())
	}
	return out, total, nil
}

// GetReviewTask 读一条任务（含 holdout 判定）；不存在返回 model.ErrNotFound。
func (r EvalReviewRepo) GetReviewTask(ctx context.Context, sys SystemScope, id uint64) (model.ReviewTask, error) {
	if err := requireSystem(sys); err != nil {
		return model.ReviewTask{}, err
	}
	return r.taskOn(r.DB.WithContext(ctx), sys, id)
}

// taskOn 在 db（可为事务）上读一条任务。
func (EvalReviewRepo) taskOn(db *gorm.DB, _ SystemScope, id uint64) (model.ReviewTask, error) {
	var rows []reviewTaskRow
	if err := db.Raw(reviewSelect+" WHERE rt.id = ?", id).Scan(&rows).Error; err != nil {
		return model.ReviewTask{}, fmt.Errorf("repository: read review task %d: %w", id, err)
	}
	if len(rows) == 0 {
		return model.ReviewTask{}, fmt.Errorf("repository: review task %d: %w", id, model.ErrNotFound)
	}
	return rows[0].task(), nil
}

// ReviewTrialSummary 是非 holdout 任务所挂 trial 的定位信息（不含任何样本文本与 judge 分数）。
type ReviewTrialSummary struct {
	Suite         string
	SampleVersion int
	Status        string
}

// TrialSummary 读任务所挂 trial 的 suite / sample_version / status；任务未挂 trial 或找不到时 found=false。
// 调用方只对非 holdout 任务调用；这里再按 suite 拦一次，holdout trial 一律 found=false。
func (r EvalReviewRepo) TrialSummary(ctx context.Context, sys SystemScope, t model.ReviewTask) (ReviewTrialSummary, bool, error) {
	if err := requireSystem(sys); err != nil {
		return ReviewTrialSummary{}, false, err
	}
	if t.ExperimentID == nil || t.SampleID == "" || t.TrialIndex == nil {
		return ReviewTrialSummary{}, false, nil
	}
	var rows []ReviewTrialSummary
	if err := r.DB.WithContext(ctx).Raw(`SELECT suite, sample_version, status FROM trial
WHERE experiment_id = ? AND sample_id = ? AND trial_index = ? AND suite <> 'holdout' LIMIT 1`,
		*t.ExperimentID, t.SampleID, *t.TrialIndex).Scan(&rows).Error; err != nil {
		return ReviewTrialSummary{}, false, fmt.Errorf("repository: trial of review task %d: %w", t.ID, err)
	}
	if len(rows) == 0 {
		return ReviewTrialSummary{}, false, nil
	}
	return rows[0], true, nil
}

// ErrReviewConflict 表示任务状态不允许本次提交（已由别人完成 / 已派给别人 / SKIPPED / 主标双标同一专家）：
// 提交方重读后再决定，controller 回 409。
var ErrReviewConflict = errors.New("repository: review task conflict")

// ReviewClaim 是 ClaimReviewTask 的结果：Done 为 true 表示本专家已完成过该任务（Task.ExpertScore 为当时的分数），
// 调用方据此比对提交体判定重放；否则任务已是 ASSIGNED 给本专家（可继续写副作用并 Complete）。
type ReviewClaim struct {
	Task model.ReviewTask
	Done bool
}

// raterLockRow 是 Claim 锁住的主标 / 双标行。
type raterLockRow struct {
	ID         uint64
	Status     string
	AssigneeID *uint64
}

// ClaimReviewTask 是提交的第 ① 步（可重放状态机）：事务内按 id 顺序 FOR UPDATE 锁住同一 trial 同一维度的主标与双标两行，
// OPEN 或已派给本专家的 ASSIGNED 置为 ASSIGNED(expertID)；本专家已 DONE 的返回 Done；其余（他人持有 / 他人完成 / SKIPPED /
// 另一标是本专家）返回 ErrReviewConflict。不存在返回 model.ErrNotFound。
func (r EvalReviewRepo) ClaimReviewTask(ctx context.Context, sys SystemScope, id, expertID uint64) (ReviewClaim, error) {
	if err := requireSystem(sys); err != nil {
		return ReviewClaim{}, err
	}
	var out ReviewClaim
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		t, err := r.taskOn(tx, sys, id)
		if err != nil {
			return err
		}
		primary, second := t, t
		primary.RaterNo, second.RaterNo = model.RaterPrimary, model.RaterSecond
		var rows []raterLockRow
		if err := tx.Raw("SELECT id, status, assignee_id FROM review_task WHERE ref_key IN (?, ?) ORDER BY id FOR UPDATE",
			primary.Key(), second.Key()).Scan(&rows).Error; err != nil {
			return fmt.Errorf("repository: lock raters of review task %d: %w", id, err)
		}
		self, err := claimable(rows, id, expertID)
		if err != nil {
			return err
		}
		if self.Status == model.ReviewDone { // 锁内重读分数（锁前读到的行可能刚被本专家的并发请求完成）
			out.Task, err = r.taskOn(tx, sys, id)
			out.Done = true
			return err
		}
		if self.Status == model.ReviewOpen {
			res := tx.Exec("UPDATE review_task SET assignee_id = ?, status = ? WHERE id = ? AND status = ?",
				expertID, model.ReviewAssigned, id, model.ReviewOpen)
			if res.Error != nil {
				return fmt.Errorf("repository: claim review task %d: %w", id, res.Error)
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("%w: claim task %d affected %d rows", ErrReviewConflict, id, res.RowsAffected)
			}
		}
		t.Status, t.AssigneeID = model.ReviewAssigned, &expertID
		out.Task = t
		return nil
	})
	if err != nil {
		return ReviewClaim{}, err
	}
	return out, nil
}

// claimable 在锁住的主标 / 双标行中判定本专家能否提交 id：另一标派给了本专家即冲突（双标须独立）；
// 本行须为无人认领的 OPEN，或已派给 / 已由本专家完成。
func claimable(rows []raterLockRow, id, expertID uint64) (raterLockRow, error) {
	idx := slices.IndexFunc(rows, func(w raterLockRow) bool { return w.ID == id })
	if idx < 0 {
		return raterLockRow{}, fmt.Errorf("repository: review task %d: %w", id, model.ErrNotFound)
	}
	for i, w := range rows {
		if i != idx && w.AssigneeID != nil && *w.AssigneeID == expertID {
			return raterLockRow{}, fmt.Errorf("%w: task %d and %d would share expert %d", ErrReviewConflict, id, w.ID, expertID)
		}
	}
	self := rows[idx]
	mine := self.AssigneeID != nil && *self.AssigneeID == expertID
	switch {
	case self.Status == model.ReviewOpen && (self.AssigneeID == nil || mine):
		return self, nil
	case (self.Status == model.ReviewAssigned || self.Status == model.ReviewDone) && mine:
		return self, nil
	default:
		return raterLockRow{}, fmt.Errorf("%w: task %d is %s", ErrReviewConflict, id, self.Status)
	}
}

// CompleteReviewTask 是提交的最后一步：ASSIGNED(expertID) → DONE 并写 expert_score（BACKFLOW_EXPECT 为 NULL）。
// 0 行时重读：本专家已以同分完成视为重放（nil），否则 ErrReviewConflict。
func (r EvalReviewRepo) CompleteReviewTask(ctx context.Context, sys SystemScope, id, expertID uint64, score *int,
	at time.Time) error {
	if err := requireSystem(sys); err != nil {
		return err
	}
	db := r.DB.WithContext(ctx)
	res := db.Exec(`UPDATE review_task SET expert_score = ?, status = ?, done_at = ?
WHERE id = ? AND status = ? AND assignee_id = ?`, score, model.ReviewDone, at.UTC(), id, model.ReviewAssigned, expertID)
	if res.Error != nil {
		return fmt.Errorf("repository: complete review task %d: %w", id, res.Error)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	t, err := r.taskOn(db, sys, id)
	if err != nil {
		return err
	}
	if t.Status == model.ReviewDone && t.AssigneeID != nil && *t.AssigneeID == expertID && equalScore(t.ExpertScore, score) {
		return nil
	}
	return fmt.Errorf("%w: complete task %d (status %s)", ErrReviewConflict, id, t.Status)
}

func equalScore(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// SkipReviewTask 释放确定性冲突的任务：ASSIGNED(expertID) → SKIPPED（done_at 记释放时刻）。只动本专家持有的行，
// 0 行（已不是本专家的 ASSIGNED）返回 ErrReviewConflict。
func (r EvalReviewRepo) SkipReviewTask(ctx context.Context, sys SystemScope, id, expertID uint64, at time.Time) error {
	if err := requireSystem(sys); err != nil {
		return err
	}
	res := r.DB.WithContext(ctx).Exec(`UPDATE review_task SET status = ?, done_at = ?
WHERE id = ? AND status = ? AND assignee_id = ?`, model.ReviewSkipped, at.UTC(), id, model.ReviewAssigned, expertID)
	if res.Error != nil {
		return fmt.Errorf("repository: skip review task %d: %w", id, res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("%w: skip task %d affected %d rows", ErrReviewConflict, id, res.RowsAffected)
	}
	return nil
}

// reviewInsertCols 是 InsertReviewTasks 每行的占位符数。
const reviewInsertCols = 13

// InsertReviewTasks 批量插入 OPEN 任务（ON DUPLICATE KEY UPDATE id = id，按 ref_key 去重：重跑不重复派单），返回新插入行数。
func (r EvalReviewRepo) InsertReviewTasks(ctx context.Context, sys SystemScope, tasks []model.ReviewTask, at time.Time) (int, error) {
	if err := requireSystem(sys); err != nil {
		return 0, err
	}
	inserted := 0
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(tasks); start += evalReviewBatch {
			batch := tasks[start:min(start+evalReviewBatch, len(tasks))]
			var sb strings.Builder
			sb.WriteString(`INSERT INTO review_task (task_type, queue_reason, experiment_id, sample_id, trial_index, draft_ref,
  dimension, judge_version, judge_score, rater_no, status, ref_key, created_at) VALUES `)
			args := make([]any, 0, len(batch)*reviewInsertCols)
			for i, t := range batch {
				if !model.ValidReviewTaskType(t.TaskType) {
					return fmt.Errorf("repository: review task %d: task_type %q", start+i, t.TaskType)
				}
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				rater := t.RaterNo
				if rater == 0 {
					rater = model.RaterPrimary
				}
				args = append(args, t.TaskType, nullIfEmpty(t.QueueReason), t.ExperimentID, nullIfEmpty(t.SampleID), t.TrialIndex,
					nullIfEmpty(t.DraftRef), nullIfEmpty(t.Dimension), nullIfEmpty(t.JudgeVersion), t.JudgeScore, rater,
					model.ReviewOpen, t.Key(), at.UTC())
			}
			sb.WriteString(" ON DUPLICATE KEY UPDATE id = id")
			res := tx.Exec(sb.String(), args...)
			if res.Error != nil {
				return fmt.Errorf("repository: insert review tasks: %w", res.Error)
			}
			inserted += int(res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}
